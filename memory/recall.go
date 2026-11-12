package memory

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// recall.go 召回线：把工作记忆变成候选与排序。
// 判断权：相似度只来自真嵌入；强度/重要性/新鲜度只来自规则与数学。

// Retrieve 召回线，见设计文档第七节。
//
// 两段式：门（focus）与排序（context）分开。
// 只用本轮最后一句话决定"要不要想起"，因为整段上下文混合会把无关旧账也顶上来
// （实测：连续对话里问"今天天气怎么样"，会带出前面几轮的偏好）。排序仍用 0.5^k
// 加权的上下文向量，所以「那它呢」这类依赖上文的表达依然能正确排序。
//
// 召回范围是**全部记忆**：归档只代表"细节被压缩"，不代表消失。
func (e *Engine) Retrieve(turns []string) []*Memory {
	tick := e.Store.Tick()
	focus := e.focusVector(turns)
	qvec := e.queryVector(turns)

	type scored struct {
		score float64
		mem   *Memory
	}
	var all []scored
	for _, m := range e.Store.All() {
		ApplyDecay(m, tick)
		relevance := 0.0
		if v := e.vectorOf(m); v != nil {
			// 门：语义上不相关就不进候选，强度/重要性再高也不该被"想起来"。
			// focus 为 nil（追问太短或没有嵌入模型）时不做门，保持旧行为。
			if focus != nil && normalizeSimilarity(Cosine(v, focus)) < MinRelevance {
				continue
			}
			if qvec != nil {
				relevance = normalizeSimilarity(Cosine(v, qvec))
			}
		}
		score := relevance*0.4 + m.Strength*0.25 + m.Importance*0.2 + Freshness(m, tick)*0.15
		if m.Layer == LayerIdentity {
			score = math.Max(score, 0.5*relevance) // 身份层保底：忘不掉"你是谁"
		}
		if m.Status == StatusArchived {
			score *= ArchivedPenalty // 压缩态仍可被想起，只是排在清晰记忆之后
		}
		all = append(all, scored{score, m})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })

	var results []*Memory
	for _, s := range all {
		if len(results) >= MaxReturn {
			break
		}
		UpdateStrengthOnRecall(s.mem, s.score, tick) // 被动复习
		results = append(results, s.mem)
	}
	return results
}

// contextOnly 是"没有检索内容、只能靠上文"的表达闭集。
// 门不能仅凭长度判断——短句也可能是真查询（「我不吃香菜」），拿长度当代理正是设计里禁止的。
var contextOnly = map[string]bool{
	"继续": true, "然后呢": true, "接着": true, "接着说": true, "还有呢": true,
	"那它呢": true, "那这个呢": true, "那那个呢": true, "这个呢": true, "然后": true,
	"嗯": true, "好": true, "对": true, "是": true, "行": true, "ok": true,
}

// focusVector 只嵌本轮最后一句话：它回答"现在在问什么"。
// 纯追问（「那它呢」「继续」）没有检索价值，返回 nil 让调用方跳过这道门、只靠上下文排序。
func (e *Engine) focusVector(turns []string) []float32 {
	if len(turns) == 0 {
		return nil
	}
	last := strings.TrimSpace(turns[len(turns)-1])
	if contextOnly[strings.ToLower(last)] || utf8.RuneCountInString(last) <= 2 {
		return nil
	}
	v, err := e.embed(last)
	if err != nil {
		return nil
	}
	return Normalize(v)
}

// queryVector 逐轮加权合成查询向量；一轮都嵌不出来就返回 nil（调用方退化为规则排序）。
func (e *Engine) queryVector(turns []string) []float32 {
	var sum []float32
	for i, t := range turns {
		v, err := e.embed(t)
		if err != nil {
			continue
		}
		w := math.Pow(0.5, float64(len(turns)-1-i)) // 越近的轮次权重越高
		if sum == nil {
			sum = make([]float32, len(v))
		}
		if len(v) != len(sum) {
			continue
		}
		for j := range sum {
			sum[j] += float32(w) * v[j]
		}
	}
	if sum == nil {
		return nil
	}
	return Normalize(sum)
}

// embed 取文本向量；没有嵌入模型时返回错误，调用方据此降级而不是假造相似度。
func (e *Engine) embed(text string) ([]float32, error) {
	if e.Embed == nil || strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("没有可用的嵌入模型")
	}
	return e.Embed.Embed(text)
}

// vectorOf 取记忆的向量；缺向量或向量来自别的嵌入模型时按需重算并写回。
// 换嵌入模型后新旧向量混用是致命的（不同模型的相似度不可比），所以必须按模型名失效。
func (e *Engine) vectorOf(m *Memory) []float32 {
	if len(m.Embedding) > 0 && m.EmbeddingModel == e.Embed.ModelName() {
		return m.Embedding
	}
	v, err := e.embed(m.Content)
	if err != nil {
		return nil
	}
	m.Embedding, m.EmbeddingModel = v, e.Embed.ModelName()
	return v
}

// normalizeSimilarity 把余弦映射成"像不像"的 0~1 分。
// 真嵌入模型对任意两句中文都有 0.35~0.45 的基线相似度，直接用余弦会让无关内容也过阈值；
// 实测 qwen3-embedding:0.6b：无关 0.37~0.61、相关 0.78~0.90，故按 [0.35, 0.90] 线性拉伸。
// 换嵌入模型必须重标这两个常数——它们不是普适的，是这个模型的分布。
func normalizeSimilarity(cos float64) float64 {
	v := (cos - SimilarityFloor) / (SimilarityCeil - SimilarityFloor)
	return math.Max(0, math.Min(1, v))
}

// isRepeatedPattern 同类内容是否已出现过（重要性评分的「重复模式」信号）。
// 只和已有记忆的向量比，不对全库重新计算。
func (e *Engine) isRepeatedPattern(summary string) bool {
	v, err := e.embed(summary)
	if err != nil {
		return false
	}
	for _, m := range e.Store.Active() {
		if old := e.vectorOf(m); old != nil && normalizeSimilarity(Cosine(v, old)) >= 0.7 {
			return true
		}
	}
	return false
}
