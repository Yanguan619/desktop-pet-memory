package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stubLLM struct {
	out   *Extracted
	err   error
	turns []string
}

func (s *stubLLM) Extract(turn string, at time.Time) (*Extracted, error) {
	s.turns = append(s.turns, turn)
	return s.out, s.err
}

// stubEmbed 按表给向量：测试只验证逻辑，不验证嵌入质量。
type stubEmbed struct{ table map[string][]float32 }

func (s stubEmbed) Embed(text string) ([]float32, error) {
	if v, ok := s.table[text]; ok {
		return v, nil
	}
	return nil, errors.New("stub: 没有该文本的向量")
}

func (s stubEmbed) ModelName() string { return "stub-embed" }

type stubCritic struct {
	rel      Relation
	merged   string
	identity string
	err      error
	calls    int
}

func (s *stubCritic) Relation(old, new string) (Relation, error) {
	s.calls++
	return s.rel, s.err
}
func (s *stubCritic) Merge(contents []string) (string, error) { return s.merged, s.err }
func (s *stubCritic) Identity(contents []string) (string, error) {
	return s.identity, s.err
}

func testEngine(llm Extractor, embed Embedder, critic Critic, mems ...*Memory) *Engine {
	store := &Store{}
	for _, m := range mems {
		store.Upsert(m)
	}
	return &Engine{Store: store, LLM: llm, Embed: embed, Critic: critic}
}

func TestScheduleNextReviewUsesTicks(t *testing.T) {
	m := &Memory{ReviewCount: 0, LastReviewTick: 10}
	if got := ScheduleNextReview(m); got != 11 {
		t.Errorf("第一次复习应在 1 轮后, got %d", got)
	}
	m.ReviewCount = 99
	if got := ScheduleNextReview(m); got != 10+ReviewIntervals[len(ReviewIntervals)-1] {
		t.Errorf("复习次数超表应钳到最后一档, got %d", got)
	}
}

func TestApplyDecayByExperienceNotClock(t *testing.T) {
	archived := &Memory{Status: StatusArchived, Strength: 0.9, DecayedTick: 0}
	ApplyDecay(archived, 1000)
	if archived.Strength != 0.9 {
		t.Error("归档记忆不应衰减")
	}

	fresh := &Memory{Status: StatusActive, Strength: 0.5, Importance: 0.5, LastReviewTick: 7, DecayedTick: 7}
	ApplyDecay(fresh, 7)
	if fresh.Strength != 0.5 {
		t.Errorf("同一 tick 内不该衰减, got %v", fresh.Strength)
	}

	never := &Memory{Status: StatusActive, Strength: 0.5, Importance: 0.5, LastReviewTick: 0, DecayedTick: 0}
	ApplyDecay(never, 100000)
	if never.Status != StatusArchived {
		t.Errorf("长期没被复习应归档, got %v", never.Status)
	}
}

// 衰减必须按「距上次结算」增量：同一 tick 反复结算不能叠加，否则召回越频繁忘得越快。
func TestApplyDecayIsIdempotent(t *testing.T) {
	m := &Memory{Status: StatusActive, Strength: 0.5, Importance: 0.5, LastReviewTick: 0, DecayedTick: 0}
	ApplyDecay(m, 100)
	once := m.Strength
	for i := 0; i < 50; i++ {
		ApplyDecay(m, 100)
	}
	if m.Strength != once {
		t.Errorf("同一 tick 结算 51 次与 1 次结果应相同: %.6f vs %.6f", m.Strength, once)
	}
	ApplyDecay(m, 101)
	if m.Strength >= once {
		t.Error("推进到下一个 tick 应继续衰减")
	}
}

// 压缩到底也保留地板：存储够用，记忆不该真的归零。
func TestStrengthNeverReachesZero(t *testing.T) {
	m := &Memory{Status: StatusActive, Strength: 0.5, Importance: 0.2}
	ApplyDecay(m, 200000)
	if m.Strength < StrengthFloor {
		t.Errorf("强度不应低于地板 %.2f, got %.6f", StrengthFloor, m.Strength)
	}
}

// 归档 = 细节压缩，不是消失：命中查询时仍能想起来，并因回忆重新变清晰。
func TestArchivedMemoryStillRecallable(t *testing.T) {
	embed := stubEmbed{table: map[string][]float32{
		"用户偏好深色主题": {1, 0, 0},
		"界面配色怎么定":  {0.99, 0.01, 0},
	}}
	archived := &Memory{ID: "old", Layer: LayerSemantic, Content: "用户偏好深色主题",
		Status: StatusArchived, Strength: 0.03, Importance: 0.5, DecayedTick: 0}
	e := testEngine(nil, embed, nil, archived)

	got := e.Retrieve([]string{"界面配色怎么定"})
	if len(got) == 0 || got[0].ID != "old" {
		t.Fatalf("归档记忆应仍可被召回, got %+v", got)
	}
	if got[0].Status != StatusActive {
		t.Errorf("被想起后应重新变清晰, got %s", got[0].Status)
	}
}

// 语义不相关就不该被"想起来"：强度再高也不行（否则无关查询会把记忆顶上来并强化它）。
func TestRetrieveFiltersIrrelevant(t *testing.T) {
	embed := stubEmbed{table: map[string][]float32{
		"用户偏好深色主题": {1, 0, 0},
		"无关查询":     {0.5, 0.866, 0}, // cos = 0.5 → 归一化 0.27，低于 MinRelevance
		"相关查询":     {0.9, 0.436, 0}, // cos = 0.9 → 归一化 1.0
	}}
	mem := &Memory{ID: "a", Layer: LayerSemantic, Content: "用户偏好深色主题",
		Strength: 1, Importance: 1, Status: StatusActive, ReviewCount: 1}
	e := testEngine(nil, embed, nil, mem)

	if got := e.Retrieve([]string{"无关查询"}); len(got) != 0 {
		t.Errorf("不相关查询不该召回, got %+v", got)
	}
	if got := e.Retrieve([]string{"相关查询"}); len(got) != 1 {
		t.Errorf("相关查询应召回, got %+v", got)
	}
}

func TestFreshnessByTicks(t *testing.T) {
	m := &Memory{LastReviewTick: 0}
	if got := Freshness(m, 0); got != 1 {
		t.Errorf("同一 tick 应完全新鲜, got %v", got)
	}
	if got := Freshness(m, 1000); got >= 0.01 {
		t.Errorf("很久没复习应几乎不新鲜, got %v", got)
	}
}

func TestScoreImportance(t *testing.T) {
	ex := &Extracted{Layer: LayerIdentity, Entities: []string{"a", "b", "c", "d"}}
	if got := ScoreImportance("记住：我的长期目标", ex, true); got != 1.0 {
		t.Errorf("各分项累加应封顶 1.0, got %v", got)
	}
	if got := ScoreImportance("你好", nil, false); got != 0 {
		t.Errorf("无任何特征应为 0, got %v", got)
	}
}

func TestEncodeWorthlessProducesNothing(t *testing.T) {
	e := testEngine(&stubLLM{out: &Extracted{Worth: false}}, stubEmbed{}, nil)
	got, err := e.Encode(Event{Recent: []string{"你好"}})
	if err != nil {
		t.Fatalf("模型正常时不应报错: %v", err)
	}
	if got != nil {
		t.Error("模型判定无新信息时不应产生记忆")
	}
}

// I1：规则不得用长度判价值，5 字的真偏好必须存下来。
func TestEncodeShortPreferenceKept(t *testing.T) {
	llm := &stubLLM{out: &Extracted{Worth: true, Layer: LayerSemantic, Summary: "用户不吃香菜"}}
	e := testEngine(llm, stubEmbed{}, nil)
	got, err := e.Encode(Event{Recent: []string{"我不吃香菜"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "用户不吃香菜" {
		t.Fatalf("短偏好不应被丢弃, got %+v", got)
	}
}

// I4：模型不可用必须是 error，不能伪装成「本轮没有新信息」，也不由库决定怎么兜。
func TestEncodeModelErrorIsReturned(t *testing.T) {
	e := testEngine(&stubLLM{err: errors.New("ollama down")}, stubEmbed{}, nil)
	got, err := e.Encode(Event{ID: "ev1", Recent: []string{"今天我们讨论了数据库迁移方案"}})
	if err == nil {
		t.Fatal("模型不可用时应把错误抛给调用方")
	}
	if got != nil || len(e.Store.All()) != 0 {
		t.Error("库不应自作主张落任何半成品")
	}
}

// I2：skip（正常结论）与 error（模型不可用）必须可区分。
func TestEncodeDistinguishesSkipFromError(t *testing.T) {
	skip := testEngine(&stubLLM{out: &Extracted{Worth: false}}, stubEmbed{}, nil)
	down := testEngine(&stubLLM{err: errors.New("down")}, stubEmbed{}, nil)
	turn := []string{"我们决定记忆库用 SQLite"}

	if got, err := skip.Encode(Event{Recent: turn}); err != nil || got != nil {
		t.Errorf("skip 应是 (nil, nil), got %+v %v", got, err)
	}
	if _, err := down.Encode(Event{Recent: turn}); err == nil {
		t.Error("模型不可用应是 (nil, err)")
	}
}

// I3：内容永不被覆写。更新走 supersede 链，旧记录留档。
func TestEncodeUpdateKeepsOldContent(t *testing.T) {
	old := &Memory{ID: "old", Layer: LayerSemantic, Content: "用户偏好深色主题", Status: StatusActive, LastReviewTick: 0, Strength: 0.5}
	embed := stubEmbed{table: map[string][]float32{
		"用户偏好深色主题": {1, 0, 0},
		"用户偏好浅色主题": {0.99, 0.01, 0},
	}}
	llm := &stubLLM{out: &Extracted{Worth: true, Layer: LayerSemantic, Summary: "用户偏好浅色主题"}}
	critic := &stubCritic{rel: RelationContradict}
	e := testEngine(llm, embed, critic, old)

	got, err := e.Encode(Event{Recent: []string{"记住：我改成浅色主题了"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatal("应产生一条新记忆")
	}
	if got[0].Supersedes != "old" {
		t.Errorf("新记忆应 supersedes 旧记忆, got %q", got[0].Supersedes)
	}
	if old.Content != "用户偏好深色主题" || old.Status != StatusArchived {
		t.Errorf("旧记忆应原文保留并归档, got %q %q", old.Content, old.Status)
	}
	if critic.calls != 1 {
		t.Errorf("粗筛命中后应恰好问一次模型, got %d", critic.calls)
	}
}

// 粗筛未命中就不该花模型调用：省调用的地方只能在"确定无关"上，不能在价值判断上。
func TestResolveRelationSkipsUnrelatedWithoutCallingModel(t *testing.T) {
	old := &Memory{ID: "old", Layer: LayerSemantic, Content: "今天午饭吃了火锅", Status: StatusActive, Strength: 0.5}
	embed := stubEmbed{table: map[string][]float32{
		"今天午饭吃了火锅": {0, 1, 0},
		"用户偏好深色主题": {1, 0, 0}, // 与旧记忆正交，余弦 0
	}}
	llm := &stubLLM{out: &Extracted{Worth: true, Layer: LayerSemantic, Summary: "用户偏好深色主题"}}
	critic := &stubCritic{rel: RelationContradict}
	e := testEngine(llm, embed, critic, old)

	got, err := e.Encode(Event{Recent: []string{"记住：我偏好深色主题"}})
	if err != nil {
		t.Fatal(err)
	}
	if critic.calls != 0 {
		t.Errorf("相似度不到阈值时不应调用模型, got %d 次", critic.calls)
	}
	if got[0].Supersedes != "" || old.Status != StatusActive {
		t.Error("无关记忆必须都保留")
	}
}

func TestResolveRelationSkipsWhenCriticMissing(t *testing.T) {
	old := &Memory{ID: "old", Layer: LayerSemantic, Content: "用户偏好深色主题", Status: StatusActive, Strength: 0.5}
	embed := stubEmbed{table: map[string][]float32{
		"用户偏好深色主题": {1, 0, 0},
		"用户偏好浅色主题": {0.99, 0.01, 0},
	}}
	llm := &stubLLM{out: &Extracted{Worth: true, Layer: LayerSemantic, Summary: "用户偏好浅色主题"}}
	e := testEngine(llm, embed, nil, old) // 没有 Critic

	got, err := e.Encode(Event{Recent: []string{"记住：我改成浅色主题了"}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Supersedes != "" || old.Status != StatusActive {
		t.Error("判断不了关系时宁可重复，不可覆盖")
	}
}

func TestRetrieveRanksAndReviews(t *testing.T) {
	embed := stubEmbed{table: map[string][]float32{
		"用户偏好深色主题": {1, 0, 0},
		"界面配色怎么定":  {0.9, 0.1, 0},
		"今天午饭吃了火锅": {0, 1, 0},
	}}
	e := testEngine(nil, embed, nil,
		&Memory{ID: "a", Layer: LayerSemantic, Content: "用户偏好深色主题", Strength: 0.6, Importance: 0.6, Status: StatusActive, LastReviewTick: 0, NextReviewTick: 0},
		&Memory{ID: "b", Layer: LayerSemantic, Content: "今天午饭吃了火锅", Strength: 0.2, Importance: 0.2, Status: StatusActive, LastReviewTick: 0, NextReviewTick: 0},
	)
	got := e.Retrieve([]string{"界面配色怎么定"})
	if len(got) == 0 || got[0].ID != "a" {
		t.Fatalf("最相关的应排第一, got %+v", got)
	}
	if got[0].ReviewCount != 1 {
		t.Error("召回应计为一次被动复习")
	}
	if got[0].LastReviewTick != e.Store.Tick() {
		t.Error("复习应把逻辑时钟写进 last_review_tick")
	}
}

// 嵌入不可用时不得假造相似度：按强度/重要性排序，且不 panic。
func TestRetrieveWithoutEmbeddingsFallsBackToRules(t *testing.T) {
	e := testEngine(nil, nil, nil,
		&Memory{ID: "strong", Layer: LayerSemantic, Content: "x", Strength: 0.9, Importance: 0.9, Status: StatusActive},
		&Memory{ID: "weak", Layer: LayerSemantic, Content: "y", Strength: 0.1, Importance: 0.1, Status: StatusActive},
	)
	got := e.Retrieve([]string{"随便问问"})
	if len(got) == 0 || got[0].ID != "strong" {
		t.Fatalf("没有嵌入时应按规则排序, got %+v", got)
	}
}

// 查询向量按 0.5^k 加权：越近的轮次越决定找回什么。
func TestRetrieveWeightsRecentTurns(t *testing.T) {
	embed := stubEmbed{table: map[string][]float32{
		"用户偏好深色主题界面": {1, 0, 0},
		"今天午饭吃了火锅":   {0, 1, 0},
	}}
	newEng := func() *Engine {
		return testEngine(nil, embed, nil,
			&Memory{ID: "theme", Layer: LayerSemantic, Content: "用户偏好深色主题界面", Strength: 0.5, Importance: 0.5, Status: StatusActive},
			&Memory{ID: "food", Layer: LayerSemantic, Content: "今天午饭吃了火锅", Strength: 0.5, Importance: 0.5, Status: StatusActive},
		)
	}

	if got := newEng().Retrieve([]string{"今天午饭吃了火锅", "用户偏好深色主题界面"}); len(got) == 0 || got[0].ID != "theme" {
		t.Fatalf("本轮提到界面时应先想起界面, got %+v", got)
	}
	if got := newEng().Retrieve([]string{"用户偏好深色主题界面", "今天午饭吃了火锅"}); len(got) == 0 || got[0].ID != "food" {
		t.Fatalf("本轮提到火锅时应先想起火锅, got %+v", got)
	}
}

// 门用"本轮最后一句话"：上文提到界面、但本轮在问天气时，不该翻出界面偏好。
func TestRetrieveGateUsesLastTurnOnly(t *testing.T) {
	embed := stubEmbed{table: map[string][]float32{
		"用户偏好深色主题界面": {1, 0, 0},
		"今天天气怎么样":    {0, 1, 0},
	}}
	mem := &Memory{ID: "theme", Layer: LayerSemantic, Content: "用户偏好深色主题界面",
		Strength: 0.5, Importance: 0.5, Status: StatusActive}
	e := testEngine(nil, embed, nil, mem)

	// 上文聊界面，本轮问天气 → 不该想起界面
	if got := e.Retrieve([]string{"用户偏好深色主题界面", "今天天气怎么样"}); len(got) != 0 {
		t.Errorf("本轮无关时不该带出上文记忆, got %+v", got)
	}
	// 上文聊天气，本轮回到界面 → 应该想起
	if got := e.Retrieve([]string{"今天天气怎么样", "用户偏好深色主题界面"}); len(got) != 1 {
		t.Errorf("本轮相关时应想起, got %+v", got)
	}
}

// 纯追问没有检索内容，跳过门、只靠上下文排序（否则「那它呢」永远想不起任何东西）。
func TestRetrievePronounFallsBackToContext(t *testing.T) {
	embed := stubEmbed{table: map[string][]float32{
		"用户偏好深色主题界面": {1, 0, 0},
		"那它呢":        {0, 1, 0}, // 追问本身的向量没有意义
	}}
	mem := &Memory{ID: "theme", Layer: LayerSemantic, Content: "用户偏好深色主题界面",
		Strength: 0.5, Importance: 0.5, Status: StatusActive}
	e := testEngine(nil, embed, nil, mem)

	if got := e.Retrieve([]string{"用户偏好深色主题界面", "那它呢"}); len(got) != 1 {
		t.Errorf("追问应靠上下文召回, got %+v", got)
	}
}

func TestWorkingMemoryTrimsAndAdvancesTick(t *testing.T) {
	s := &Store{}
	for i := 0; i < RecentLimit+3; i++ {
		s.AppendTurn(fmt.Sprintf("turn-%d", i))
	}
	turns := s.RecentTurns()
	if len(turns) != RecentLimit {
		t.Fatalf("工作记忆应保持 %d 轮, got %d", RecentLimit, len(turns))
	}
	if turns[len(turns)-1] != fmt.Sprintf("turn-%d", RecentLimit+2) {
		t.Errorf("应保留最新一轮, got %s", turns[len(turns)-1])
	}
	if s.Tick() != RecentLimit+3 {
		t.Errorf("每轮应推进一次逻辑时钟, got %d", s.Tick())
	}
}

// 提醒不被压缩：到期前不该因为"三周没提"就沉下去。
func TestReminderNotCompressed(t *testing.T) {
	m := &Memory{Status: StatusActive, Strength: 0.5, Importance: 0.5, DueAt: "2026-12-01 09:00", DecayedTick: 0}
	ApplyDecay(m, 100000)
	if m.Strength != 0.5 || m.Status != StatusActive {
		t.Errorf("提醒不应参与衰减, got strength=%.3f status=%s", m.Strength, m.Status)
	}
}

// 提醒只响一次，重复轮询不会刷屏。
func TestDueRemindersFiresOnce(t *testing.T) {
	due := &Memory{ID: "r1", Layer: LayerEpisodic, Content: "买蛋糕", Status: StatusActive, DueAt: "2026-11-15 09:00"}
	notYet := &Memory{ID: "r2", Layer: LayerEpisodic, Content: "交房租", Status: StatusActive, DueAt: "2026-12-01 09:00"}
	e := testEngine(nil, nil, nil, due, notYet)

	got := e.DueReminders(moment("2026-11-15 12:00"))
	if len(got) != 1 || got[0].ID != "r1" {
		t.Fatalf("只应返回到期的那条, got %+v", got)
	}
	if !due.Notified {
		t.Error("交付后应标记已提醒")
	}
	if again := e.DueReminders(moment("2026-11-15 12:00")); len(again) != 0 {
		t.Errorf("同一提醒不该重复响, got %+v", again)
	}
}

// 方案 c：过期且已交付过 → 归档（原文保留，仍可召回）。
func TestExpiredNotifiedReminderArchived(t *testing.T) {
	m := &Memory{ID: "r1", Layer: LayerEpisodic, Content: "买蛋糕",
		Status: StatusActive, DueAt: "2026-11-15 09:00", Notified: true, CreatedTick: 1, LastReviewTick: 1}
	e := testEngine(nil, nil, nil, m)
	e.Reflect(moment("2026-11-16 12:00"))
	if m.Status != StatusArchived {
		t.Errorf("过期提醒应归档, got %s", m.Status)
	}
	if m.Content != "买蛋糕" {
		t.Error("归档不该改动原文")
	}
}

// 宿主关机期间没能交付的过期提醒不归档：否则就是静默丢失。
func TestUnexpiredPendingReminderSurvives(t *testing.T) {
	m := &Memory{ID: "r1", Layer: LayerEpisodic, Content: "买蛋糕",
		Status: StatusActive, DueAt: "2026-11-15 09:00", Notified: false, CreatedTick: 1, LastReviewTick: 1}
	e := testEngine(nil, nil, nil, m)
	e.Reflect(moment("2026-11-20 12:00"))
	if m.Status == StatusArchived {
		t.Error("还没交付过的过期提醒不应归档")
	}
	if got := e.DueReminders(moment("2026-11-20 12:00")); len(got) != 1 {
		t.Errorf("补提醒应仍然生效, got %+v", got)
	}
}

// 兜底知道「哪天」：识别成提醒就一定会留下来，只是可能等一句确认。
func TestPendingDueStoredThenConfirmed(t *testing.T) {
	llm := &stubLLM{out: &Extracted{Worth: true, Layer: LayerEpisodic,
		Summary: "用户要还书", IsReminder: true, PendingDue: true}}
	e := testEngine(llm, nil, nil)

	got, err := e.Encode(Event{Turn: "有空提醒我把书还了"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].PendingDue || got[0].DueAt != "" {
		t.Fatalf("应是 pending 提醒, got %+v", got[0])
	}
	if len(e.PendingReminders()) != 1 {
		t.Error("应进入待补日期列表")
	}
	if len(e.DueReminders(moment("2026-11-12 12:00"))) != 0 {
		t.Error("日期未定不该触发提醒")
	}

	if err := e.ConfirmDue(got[0].ID, "2026-11-20", day("2026-11-12")); err != nil {
		t.Fatal(err)
	}
	if got[0].PendingDue || got[0].DueAt != "2026-11-20 09:00" {
		t.Errorf("补日期后应解除 pending, got %+v", got[0])
	}
	if len(e.DueReminders(moment("2026-11-12 12:00"))) != 0 {
		t.Error("还没到期不该提醒")
	}
	if len(e.DueReminders(moment("2026-11-20 12:00"))) != 1 {
		t.Error("到期当天应提醒")
	}

	if err := e.ConfirmDue(got[0].ID, "下周三", day("2026-11-12")); err == nil {
		t.Error("非法日期应报错")
	}
	if err := e.ConfirmDue("nope", "2026-12-01", day("2026-11-12")); err == nil {
		t.Error("不存在的记忆应报错")
	}
}

// 两条提醒是两件待办：文字再像也不能互相取代，否则一条提醒会被另一条"吃掉"。
func TestRemindersNeverSupersedeEachOther(t *testing.T) {
	old := &Memory{ID: "old", Layer: LayerEpisodic, Content: "用户要体检", Status: StatusActive,
		Strength: 0.5, DueAt: "2026-11-15 09:30"}
	embed := stubEmbed{table: map[string][]float32{
		"用户要体检": {1, 0, 0},
		"用户要开会": {0.99, 0.01, 0},
	}}
	llm := &stubLLM{out: &Extracted{Worth: true, Layer: LayerEpisodic, Summary: "用户要开会", Due: "2026-11-13 15:00"}}
	critic := &stubCritic{rel: RelationUpdate} // 模型认为这是"更新"，也必须忽略
	e := testEngine(llm, embed, critic, old)

	got, err := e.Encode(Event{Turn: "明天下午3点提醒我开会"})
	if err != nil {
		t.Fatal(err)
	}
	if critic.calls != 0 {
		t.Errorf("提醒不该去问关系仲裁, got %d 次", critic.calls)
	}
	if got[0].Supersedes != "" || old.Status != StatusActive {
		t.Error("两条提醒必须各自独立保留")
	}
}

func TestReflectArchivesAndPromotes(t *testing.T) {
	critic := &stubCritic{identity: "用户在追求稳定可维护的软件架构"}
	e := testEngine(nil, nil, critic,
		&Memory{ID: "weak", Layer: LayerEpisodic, Status: StatusFading, Strength: 0.01, LastReviewTick: 0},
		&Memory{ID: "s1", Layer: LayerSemantic, Status: StatusActive, Strength: 0.5, Importance: 0.5, Tags: []string{"架构"}, Content: "偏好简单架构"},
		&Memory{ID: "s2", Layer: LayerSemantic, Status: StatusActive, Strength: 0.5, Importance: 0.5, Tags: []string{"架构"}, Content: "讨厌过度设计"},
		&Memory{ID: "s3", Layer: LayerSemantic, Status: StatusActive, Strength: 0.5, Importance: 0.5, Tags: []string{"架构"}, Content: "要求可维护"},
	)
	e.Reflect(moment("2026-11-12 12:00"))
	if m := findByID(e, "weak"); m.Status != StatusArchived {
		t.Error("强度过低的 fading 记忆应被归档")
	}
	promoted := false
	for _, m := range e.Store.All() {
		if m.Layer == LayerIdentity && strings.Contains(m.Content, "可维护") {
			promoted = true
		}
	}
	if !promoted {
		t.Error("复现 3 次的语义模式应提炼为身份记忆")
	}
}

func TestReflectWithoutCriticDoesNotPromote(t *testing.T) {
	e := testEngine(nil, nil, nil,
		&Memory{ID: "s1", Layer: LayerSemantic, Status: StatusActive, Strength: 0.5, Tags: []string{"a"}, Content: "x"},
		&Memory{ID: "s2", Layer: LayerSemantic, Status: StatusActive, Strength: 0.5, Tags: []string{"a"}, Content: "y"},
		&Memory{ID: "s3", Layer: LayerSemantic, Status: StatusActive, Strength: 0.5, Tags: []string{"a"}, Content: "z"},
	)
	e.Reflect(moment("2026-11-12 12:00"))
	for _, m := range e.Store.All() {
		if m.Layer == LayerIdentity {
			t.Error("没有语义判断能力时不能靠 tag 计数升级身份层")
		}
	}
}

func findByID(e *Engine, id string) *Memory {
	for _, m := range e.Store.All() {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// 时钟必须单调：meta 里的 tick 被改小或丢失时，不能落后于库里任何一条记忆，
// 否则会出现"未来创建的记忆"（delta 为负、永不再衰减）。
func TestTickClampedToMemories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem.db")
	s1, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Upsert(&Memory{ID: "x", Layer: LayerSemantic, Content: "c", Status: StatusActive,
		CreatedTick: 1, LastReviewTick: 9, NextReviewTick: 12})
	if err := s1.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE meta SET value = '0' WHERE key = 'tick'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s2, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })
	if got := s2.Tick(); got != 9 {
		t.Errorf("tick 应被抬到最大记忆刻度 9, got %d", got)
	}
}

// schema 里的列顺序与 memColumns 必须一致——测试会拦。
func TestStoreColumnsMatchSchema(t *testing.T) {
	start := strings.Index(schema, "CREATE TABLE IF NOT EXISTS memories (")
	if start < 0 {
		t.Fatal("schema 里找不到 memories 建表语句")
	}
	body := schema[start:]
	body = body[strings.Index(body, "(")+1:]
	body = body[:strings.Index(body, ")")]

	var cols []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cols = append(cols, strings.Fields(line)[0])
	}
	if len(cols) != len(memColumns) {
		t.Fatalf("schema 有 %d 列，memColumns 有 %d 列：加字段时两处都要改\nschema=%v\nmemColumns=%v",
			len(cols), len(memColumns), cols, memColumns)
	}
	for i := range cols {
		if cols[i] != memColumns[i] {
			t.Errorf("第 %d 列不一致：schema=%q memColumns=%q", i+1, cols[i], memColumns[i])
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem.db")
	s1, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Upsert(&Memory{
		ID: "x", Layer: LayerSemantic, Content: "c", Status: StatusActive,
		Importance: 0.5, Strength: 0.4, ReviewCount: 2, Tags: []string{"偏好", "主题"},
		CreatedTick: 1, LastReviewTick: 2, NextReviewTick: 8,
		Supersedes: "old", Confidence: 0.8,
		Embedding: []float32{0.1, 0.2, 0.3}, EmbeddingModel: "nomic-embed-text",
	})
	s1.AppendTurn("第一轮")
	s1.AppendTurn("第二轮")
	if err := s1.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })

	if len(s2.All()) != 1 || s2.Tick() != 2 {
		t.Fatalf("落盘/读回不一致: mems=%d tick=%d", len(s2.All()), s2.Tick())
	}
	m := s2.All()[0]
	if len(m.Tags) != 2 || m.ReviewCount != 2 || m.CreatedTick != 1 || m.LastReviewTick != 2 || m.NextReviewTick != 8 {
		t.Errorf("刻度字段没完整往返: %+v", m)
	}
	if m.DecayedTick != 0 {
		t.Errorf("decayed_tick 应原样往返, got %d", m.DecayedTick)
	}
	if len(m.Embedding) != 3 || m.EmbeddingModel != "nomic-embed-text" {
		t.Errorf("向量没往返: %v %q", m.Embedding, m.EmbeddingModel)
	}
	if m.Supersedes != "old" || m.Confidence != 0.8 {
		t.Errorf("审计字段没往返: %+v", m)
	}
	if turns := s2.RecentTurns(); len(turns) != 2 || turns[0] != "第一轮" {
		t.Errorf("工作记忆应保持时间正序, got %v", turns)
	}
}

// 新库要写上 schema 版本；老库（有数据但没版本号）必须明确报错，而不是等到 INSERT 才炸。
func TestSchemaVersionGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem.db")
	s, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Upsert(&Memory{ID: "x", Layer: LayerSemantic, Content: "c", Status: StatusActive})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM meta WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if _, err := LoadStore(path); err == nil {
		t.Fatal("缺少 schema_version 的老库应当报错，而不是继续使用")
	}
}

// 增量落盘：没变的行不重写；改了一行，另一行的数据也必须完好。
func TestFlushWritesOnlyChangedRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem.db")
	s1, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a := &Memory{ID: "a", Layer: LayerSemantic, Content: "甲的偏好", Status: StatusActive, Strength: 0.5}
	b := &Memory{ID: "b", Layer: LayerSemantic, Content: "乙的偏好", Status: StatusActive, Strength: 0.5}
	s1.Upsert(a)
	s1.Upsert(b)
	if err := s1.Flush(); err != nil {
		t.Fatal(err)
	}

	a.Strength = 0.9 // 只改 a
	if err := s1.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })
	if len(s2.All()) != 2 {
		t.Fatalf("增量写不能丢行, got %d 条", len(s2.All()))
	}
	for _, m := range s2.All() {
		switch m.ID {
		case "a":
			if m.Strength != 0.9 {
				t.Errorf("改过的行应写入, got %.2f", m.Strength)
			}
		case "b":
			if m.Content != "乙的偏好" {
				t.Errorf("没改的行应保持原样, got %q", m.Content)
			}
		}
	}
}

func TestLoadStoreImportsLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "pet-memory.json")
	old := `[{"ID":"x","Layer":"semantic","Content":"c","Status":"active"}]`
	if err := os.WriteFile(jsonPath, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadStore(filepath.Join(dir, "pet-memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if len(s.All()) != 1 || s.All()[0].Content != "c" {
		t.Fatal("旧格式（裸数组）应能导入")
	}
	if _, err := os.Stat(jsonPath + ".imported"); err != nil {
		t.Error("导入后应把旧文件改名留底，避免重复导入")
	}
}
