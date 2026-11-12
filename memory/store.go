package memory

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Store 记忆库：内存切片是唯一权威 + SQLite 单文件持久化。
// Engine 就地改 *Memory 指针，所以落盘靠行指纹发现变化：只重写变了的行。
type Store struct {
	path       string
	db         *sql.DB
	mems       []*Memory
	recent     []string          // 工作记忆：最近几轮原始对话（含用户查询）
	tick       int               // 逻辑时钟：每轮对话 +1，持久化在 meta 表
	saved      map[string]uint64 // 落盘时的行指纹：没变的行不重写
	savedTurns uint64
}

const schema = `
CREATE TABLE IF NOT EXISTS memories (
	id              TEXT PRIMARY KEY,
	layer           TEXT NOT NULL,
	content         TEXT NOT NULL,
	importance      REAL NOT NULL,
	strength        REAL NOT NULL,
	review_count    INTEGER NOT NULL,
	created_tick    INTEGER NOT NULL,
	last_review_tick INTEGER NOT NULL,
	next_review_tick INTEGER NOT NULL,
	decayed_tick    INTEGER NOT NULL,
	source_event_id TEXT NOT NULL,
	tags            TEXT NOT NULL,
	status          TEXT NOT NULL,
	embedding       BLOB,
	embedding_model TEXT NOT NULL,
	supersedes      TEXT NOT NULL,
	confidence      REAL NOT NULL,
	due_at          TEXT NOT NULL DEFAULT '',
	notified        INTEGER NOT NULL DEFAULT 0,
	pending_due     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS turns (
	id   INTEGER PRIMARY KEY AUTOINCREMENT,
	turn TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`

// LoadStore 打开记忆库（不存在则建库并建表）。
func LoadStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	fresh := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fresh = true
	}
	// WAL + busy_timeout：常驻进程和外部工具同时读写时不会立刻 database is locked
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{path: path, db: db}
	if err := s.checkSchema(); err != nil {
		db.Close()
		return nil, err
	}
	if fresh {
		if err := s.importLegacyJSON(); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := s.load(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// schemaVersion 表结构版本。改动 memories 的列就要 +1——否则老库会在 INSERT 时才炸，
// 报出来的是 NOT NULL 约束错误，没人看得懂。
const schemaVersion = "3"

// SchemaVersion 当前表结构版本，供宿主展示/诊断用。
func SchemaVersion() string { return schemaVersion }

// checkSchema 校验版本：没有版本号且库里已有数据 → 明确报错，不做半吊子猜测。
func (s *Store) checkSchema() error {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
	switch {
	case err == sql.ErrNoRows:
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("记忆库 %s 由不兼容的旧版本创建（缺少 schema_version），无法自动升级；请备份后删掉该文件重建", s.path)
		}
		_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		return err
	case err != nil:
		return err
	case v != schemaVersion:
		return fmt.Errorf("记忆库 %s 的 schema 版本是 %s，当前程序需要 %s；请备份后重建", s.path, v, schemaVersion)
	}
	return nil
}

// memColumns 是 memories 表的列顺序，SQL 文本全部由它生成：
// 加字段只改这里 + schema + 扫描列表（三处都在同一文件相邻位置），
// 并且有 TestStoreColumnsMatchSchema 挡住"加了列忘了同步"。
var memColumns = []string{
	"id", "layer", "content", "importance", "strength", "review_count",
	"created_tick", "last_review_tick", "next_review_tick", "decayed_tick",
	"source_event_id", "tags", "status", "embedding", "embedding_model",
	"supersedes", "confidence", "due_at", "notified", "pending_due",
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// upsertSet 生成 `col=excluded.col` 列表（主键本身不更新）。
func upsertSet() string {
	parts := make([]string, 0, len(memColumns)-1)
	for _, c := range memColumns[1:] {
		parts = append(parts, c+"=excluded."+c)
	}
	return strings.Join(parts, ", ")
}

// load 把库读进内存切片（覆盖式，唯一调用点在 LoadStore）。
func (s *Store) load() error {
	s.mems, s.recent = nil, nil
	rows, err := s.db.Query(`SELECT ` + strings.Join(memColumns, ", ") + `
		FROM memories ORDER BY created_tick, id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			m    Memory
			tags string
			vec  []byte
		)
		if err := rows.Scan(&m.ID, &m.Layer, &m.Content, &m.Importance, &m.Strength, &m.ReviewCount,
			&m.CreatedTick, &m.LastReviewTick, &m.NextReviewTick, &m.DecayedTick, &m.SourceEventID,
			&tags, &m.Status, &vec, &m.EmbeddingModel, &m.Supersedes, &m.Confidence, &m.DueAt, &m.Notified, &m.PendingDue); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(tags), &m.Tags); err != nil {
			return fmt.Errorf("记忆 %s 的 tags 解析失败: %w", m.ID, err)
		}
		if len(m.DueAt) == len(dateLayout) { // v4 之前的库里只有日期：补默认时刻
			m.DueAt += " 09:00"
		}
		m.Embedding = decodeVec(vec)
		s.mems = append(s.mems, &m)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	turnRows, err := s.db.Query(`SELECT turn FROM turns ORDER BY id DESC LIMIT ?`, RecentLimit)
	if err != nil {
		return err
	}
	defer turnRows.Close()
	var newest []string
	for turnRows.Next() {
		var t string
		if err := turnRows.Scan(&t); err != nil {
			return err
		}
		newest = append(newest, t)
	}
	if err := turnRows.Err(); err != nil {
		return err
	}
	for i := len(newest) - 1; i >= 0; i-- { // DESC 读出来，倒回时间正序
		s.recent = append(s.recent, newest[i])
	}

	var tick string
	err = s.db.QueryRow(`SELECT value FROM meta WHERE key = 'tick'`).Scan(&tick)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	fmt.Sscanf(tick, "%d", &s.tick)
	// 时钟必须单调：meta 丢了或被改小也不能落后于任何一条记忆，否则会出现"未来创建的记忆"
	for _, m := range s.mems {
		if m.CreatedTick > s.tick {
			s.tick = m.CreatedTick
		}
		if m.LastReviewTick > s.tick {
			s.tick = m.LastReviewTick
		}
	}
	s.saved = make(map[string]uint64, len(s.mems))
	for _, m := range s.mems {
		s.saved[m.ID] = rowFingerprint(m)
	}
	s.savedTurns = turnsFingerprint(s.recent)
	return nil
}

// importLegacyJSON 旧版 JSON 库一次性导入（建库时执行），导入后改名留底，避免重复导入。
func (s *Store) importLegacyJSON() error {
	jsonPath := strings.TrimSuffix(s.path, filepath.Ext(s.path)) + ".json"
	data, err := os.ReadFile(jsonPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var f legacyFile
	if err := json.Unmarshal(data, &f); err != nil {
		var list []*Memory
		if err := json.Unmarshal(data, &list); err != nil { // 更旧的格式：裸记忆数组
			return fmt.Errorf("旧库 %s 解析失败: %w", jsonPath, err)
		}
		s.mems = list
	} else {
		s.mems, s.recent = f.Memories, f.Recent
	}
	for _, m := range s.mems { // 墙钟字段在旧库里毫无意义，统一落在当前 tick
		m.CreatedTick, m.LastReviewTick, m.NextReviewTick, m.DecayedTick = s.tick, s.tick, s.tick, s.tick
	}
	if err := s.Flush(); err != nil {
		return err
	}
	return os.Rename(jsonPath, jsonPath+".imported")
}

// legacyFile 旧版 JSON 落盘结构，只用于一次性导入。
type legacyFile struct {
	Memories []*Memory `json:"memories"`
	Recent   []string  `json:"recent"`
}

// Tick 当前逻辑时钟（每轮对话 +1）。
func (s *Store) Tick() int { return s.tick }

// Close 关闭库；常驻进程退出时调用，CLI 进程结束由系统回收。
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Flush 单事务落盘：只写发生变化的行，进程被杀也不会留下半个库。
// 记忆规模上去以后（每条还带 1024 维向量），全表重写会很贵，所以按指纹跳过未变的行。
func (s *Store) Flush() error {
	if s.db == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // 已 Commit 时返回 ErrTxDone，忽略

	if s.saved == nil {
		s.saved = map[string]uint64{}
	}
	for _, m := range s.mems {
		fp := rowFingerprint(m)
		if s.saved[m.ID] == fp {
			continue // 没变：不写
		}
		tags, err := json.Marshal(m.Tags)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO memories (`+strings.Join(memColumns, ", ")+`)
			VALUES (`+placeholders(len(memColumns))+`)
			ON CONFLICT(id) DO UPDATE SET `+upsertSet(),
			m.ID, m.Layer, m.Content, m.Importance, m.Strength, m.ReviewCount,
			m.CreatedTick, m.LastReviewTick, m.NextReviewTick, m.DecayedTick,
			m.SourceEventID, string(tags), m.Status, encodeVec(m.Embedding),
			m.EmbeddingModel, m.Supersedes, m.Confidence, m.DueAt, m.Notified, m.PendingDue); err != nil {
			return err
		}
		s.saved[m.ID] = fp
	}

	if fp := turnsFingerprint(s.recent); fp != s.savedTurns {
		if _, err := tx.Exec(`DELETE FROM turns`); err != nil {
			return err
		}
		for _, t := range s.recent {
			if _, err := tx.Exec(`INSERT INTO turns (turn) VALUES (?)`, t); err != nil {
				return err
			}
		}
		s.savedTurns = fp
	}
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('tick', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, fmt.Sprint(s.tick)); err != nil {
		return err
	}
	return tx.Commit()
}

// rowFingerprint 一行内容的指纹：字段全一致就跳过重写。
func rowFingerprint(m *Memory) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%s|%s|%v|%v|%d|%d|%d|%d|%d|%s|%v|%s|%s|%s|%v|%s|%v|%v",
		m.ID, m.Layer, m.Content, m.Importance, m.Strength, m.ReviewCount,
		m.CreatedTick, m.LastReviewTick, m.NextReviewTick, m.DecayedTick,
		m.SourceEventID, m.Tags, m.Status, m.EmbeddingModel, m.Supersedes,
		m.Confidence, m.DueAt, m.Notified, m.PendingDue)
	h.Write(encodeVec(m.Embedding))
	return h.Sum64()
}

func turnsFingerprint(turns []string) uint64 {
	h := fnv.New64a()
	for _, t := range turns {
		h.Write([]byte(t))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// RecentTurns 返回工作记忆（最近几轮的副本），本轮在最后一条。
func (s *Store) RecentTurns() []string {
	return append([]string(nil), s.recent...)
}

// AppendTurn 把一轮内容写入工作记忆并推进逻辑时钟（超出 RecentLimit 丢弃最早的）。
// tick 在这里同步 +1：编码失败、重试、延迟处理都不影响「经历了多少轮」。
func (s *Store) AppendTurn(turn string) {
	s.recent = append(s.recent, turn)
	if n := len(s.recent) - RecentLimit; n > 0 {
		s.recent = s.recent[n:]
	}
	s.tick++
}

// All 返回全部记忆（含归档）。
func (s *Store) All() []*Memory { return s.mems }

// Active 返回未归档的记忆。
func (s *Store) Active() []*Memory {
	var out []*Memory
	for _, m := range s.mems {
		if m.Status != StatusArchived {
			out = append(out, m)
		}
	}
	return out
}

// Upsert 按 ID 插入或替换。
func (s *Store) Upsert(m *Memory) {
	for i, old := range s.mems {
		if old.ID == m.ID {
			s.mems[i] = m
			return
		}
	}
	s.mems = append(s.mems, m)
}

// encodeVec float32 向量打包成 BLOB（小端）。
func encodeVec(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	if len(b) < 4 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}
