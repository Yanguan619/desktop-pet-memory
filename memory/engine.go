package memory

import (
	"fmt"
	"sort"
	"sync/atomic"
	"time"
)

// Engine 串起三条管道：编码线（engine.go）、召回线（recall.go）、反思线（reflect.go）。
// 判断权分配：语义判断走 LLM/Critic，相似度走 Embed，强度与排序走规则与数学。
//
// 本包没有墙钟：创建时间只是宿主传进来的展示字段（Event.At / Reflect(now)），
// 强度、衰减、复习、召回、ID 全部只认逻辑时钟 tick。
type Engine struct {
	Store  *Store
	LLM    Extractor // nil 时编码线只会报错（模型不可用就不记）
	Embed  Embedder  // nil 时没有相似度，召回退化为强度/重要性排序
	Critic Critic    // nil 时不做合并与身份提炼：规则无权替它下结论

	// MergeEnabled 控制反思时是否自动合簇。默认关闭：
	// 合并会改写当前生效的摘要，属破坏性操作，等真实对话回放验证后再开。
	MergeEnabled bool
}

func NewEngine(store *Store, llm Extractor, embed Embedder, critic Critic) *Engine {
	return &Engine{Store: store, LLM: llm, Embed: embed, Critic: critic}
}

// idSeq 保证同一个 tick 内创建的多条记忆 ID 唯一。
var idSeq atomic.Uint64

// newID 用逻辑时钟 + 序号，不用时间：同一段对话序列跑两次得到同样的 ID。
func (e *Engine) newID() string {
	return fmt.Sprintf("m%d-%d", e.Store.Tick(), idSeq.Add(1))
}

// Encode 编码线，见设计文档第三、四节。
// 模型判定无新信息 → 不产生长期记忆；模型不可用 → 返回错误，原文只留在工作记忆里。
func (e *Engine) Encode(ev Event) ([]*Memory, error) {
	turn := ev.Turn
	if turn == "" && len(ev.Recent) > 0 {
		turn = ev.Recent[len(ev.Recent)-1]
	}
	if turn == "" {
		return nil, nil
	}
	tick := e.Store.Tick()

	ex, err := e.extract(turn, ev.At)
	if err != nil {
		// 模型连不上/解析失败是调用方的事：库不猜、不落半成品，原样把错误抛上去。
		return nil, err
	}
	if ex == nil || !ex.Worth || ex.Summary == "" {
		return nil, nil // 本轮没有新的长期信息，工作记忆里的原文仍在
	}

	mem := &Memory{
		ID:             e.newID(),
		Layer:          ex.Layer,
		Content:        ex.Summary,
		Importance:     ScoreImportance(turn, ex, e.isRepeatedPattern(ex.Summary)),
		Strength:       0.3,
		CreatedTick:    tick,
		LastReviewTick: tick,
		DecayedTick:    tick,
		SourceEventID:  ev.ID,
		Tags:           ex.Tags,
		Status:         StatusActive,
		Confidence:     ex.Confidence,
		DueAt:          ex.Due,
		PendingDue:     ex.PendingDue,
	}
	mem.NextReviewTick = ScheduleNextReview(mem)
	e.attachVector(mem)
	e.resolveRelation(mem)
	e.Store.Upsert(mem)
	return []*Memory{mem}, nil
}

func (e *Engine) extract(turn string, at time.Time) (*Extracted, error) {
	if e.LLM == nil {
		return nil, fmt.Errorf("没有可用的语言模型")
	}
	return e.LLM.Extract(turn, at)
}

// attachVector 给新记忆补上向量；嵌入不可用时留空，召回侧据此刻度降级。
func (e *Engine) attachVector(m *Memory) {
	if e.Embed == nil {
		return
	}
	if v, err := e.Embed.Embed(m.Content); err == nil {
		m.Embedding, m.EmbeddingModel = v, e.Embed.ModelName()
	}
}

// resolveRelation 相似度粗筛 → system2 判关系 → 更新走 supersede 链。
// 判断不了就不合并：宁可重复，不可覆盖（设计文档第五节）。
// 提醒不参与：两条提醒是两件待办，不该因为文字相似被判成"更新"而互相取代。
func (e *Engine) resolveRelation(mem *Memory) {
	if len(mem.Embedding) == 0 || e.Critic == nil {
		return
	}
	if mem.DueAt != "" || mem.PendingDue {
		return
	}
	type cand struct {
		sim float64
		mem *Memory
	}
	var cands []cand
	for _, old := range e.Store.Active() {
		// 不按层级过滤：同一件事两次可能被判成 semantic / identity，
		// 拿它当去重条件会让"改口"漏掉（实测踩过）。
		if old.ID == mem.ID || old.DueAt != "" || old.PendingDue {
			continue
		}
		sim := 0.0
		if ov := e.vectorOf(old); ov != nil && len(ov) == len(mem.Embedding) {
			sim = Cosine(ov, mem.Embedding)
		} else {
			continue // 取不到可比向量就不参与粗筛
		}
		if sim >= RelationThreshold {
			cands = append(cands, cand{sim, old})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].sim > cands[j].sim })

	for _, c := range cands {
		rel, err := e.Critic.Relation(c.mem.Content, mem.Content)
		if err != nil {
			return // 判断不了就都留
		}
		switch rel {
		case RelationUpdate, RelationContradict:
			mem.Supersedes = c.mem.ID
			c.mem.Status = StatusArchived // 原文保留，链路可追溯
			return
		case RelationUnrelated:
			continue
		case RelationSupplement:
			return // 两条都留，调用方不再逐条判
		}
	}
}
