// Package memory 实现桌面宠物记忆系统（doc/design.html）：
// 五层记忆对象 + 三线管道（编码线 / 召回线 / 反思线）。
// 判断权划分见设计文档第一节：语义判断只给 system2，相似度只给 embedding，
// 强度与排序只给规则和数学，谁也无权丢弃记忆。
package memory

import "time"

type Layer string

const (
	LayerEpisodic   Layer = "episodic"
	LayerSemantic   Layer = "semantic"
	LayerProcedural Layer = "procedural"
	LayerIdentity   Layer = "identity"
)

type Status string

const (
	StatusActive   Status = "active"
	StatusFading   Status = "fading"
	StatusArchived Status = "archived"
)

// Relation 新记忆与已有记忆的关系，由 system2 判定（设计文档第五节）。
type Relation string

const (
	RelationSupplement Relation = "supplement" // 补充：两条都留
	RelationUpdate     Relation = "update"     // 更新：新记录取代旧的
	RelationContradict Relation = "contradict" // 矛盾：新记录取代旧的，旧记录留档
	RelationUnrelated  Relation = "unrelated"  // 无关：各留各的
)

// Memory 记忆对象，见 doc/design.html 第二节。
// 强度只按逻辑时钟（tick）结算，不读墙钟：没有新经历就没有遗忘。
type Memory struct {
	ID             string
	Layer          Layer
	Content        string
	Importance     float64
	Strength       float64
	ReviewCount    int
	CreatedTick    int
	LastReviewTick int
	NextReviewTick int
	DecayedTick    int // 上次结算衰减用的 tick：保证重复结算不会叠加
	SourceEventID  string
	Tags           []string
	Status         Status

	Embedding      []float32
	EmbeddingModel string
	Supersedes     string  // 被本条取代的旧记忆 ID（原文永不覆写）
	Confidence     float64 // system2 自评，只用于排查抖动

	// DueAt 是「内容里说的那一天」（提醒），不是记忆的生命周期：
	// 生命周期一律用 tick，日期只作为事实由宿主提供、由宿主触发。
	DueAt      string // YYYY-MM-DD，空串表示不是提醒
	Notified   bool   // 是否已交付给宿主提醒过（避免同一个提醒反复响）
	PendingDue bool   // 用户说要提醒但日期没算出来：等宿主回问，绝不猜
}

// Event 一次对话事件：本轮原文 + 工作记忆快照（快照只用于理解指代）。
type Event struct {
	ID     string
	Turn   string
	Recent []string
	// At 由宿主提供，只用于把「明天 / 下周三 / 15 号」锚定成绝对日期。
	// 零值表示宿主没有时间信息，此时提醒里只能接受明确的绝对日期。
	At time.Time
}

// Extracted 是 system2 的结构化输出，见 doc/design.html 第三节。
type Extracted struct {
	Worth      bool
	Layer      Layer
	Summary    string
	Tags       []string
	Entities   Entities
	Confidence float64
	IsReminder bool   // 这句话是不是在让将来某天提醒某事（语义判断）
	Due        string // YYYY-MM-DD，能算出来时才有
	PendingDue bool   // 是提醒但日期算不出来：等宿主回问
}

const (
	MaxReturn           = 5    // 召回最多返回条数
	RecentLimit         = 8    // 工作记忆保留的对话轮数
	IdentityMinEvidence = 3    // 身份层至少需要几条独立证据
	MinRelevance        = 0.30 // 语义相关度下限（归一化后）：不相关的根本不进候选，避免把无关记忆顶上来
	// 衰减只影响"清晰度"（压缩程度）与排序，不会让记忆消失：存储够用，就不该缺东西。
	DecayScale        = 30.0 // 多少轮经历后细节明显变淡（待标定）
	StrengthFloor     = 0.03 // 强度地板：压缩到底也保留最小可召回权重
	ArchivedPenalty   = 0.5  // 压缩态记忆的排序折扣，仍可被想起
	FreshScale        = 8.0  // 多少轮后不再新鲜（待标定）
	RelationThreshold = 0.65 // 冲突粗筛：qwen3-embedding:0.6b 实测「相关」0.78~0.90、「无关」0.37~0.61，取两者之间
	MergeThreshold    = 0.85 // 反思合簇：qwen3-embedding 近义句 0.78~0.90，取偏保守的 0.85（默认不启用）
	// 相似度拉伸区间：真嵌入对任意两句中文都有 ~0.4 的基线，必须按实测分布归一化后才有区分度
	SimilarityFloor = 0.35
	SimilarityCeil  = 0.90
)
