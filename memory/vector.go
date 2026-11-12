package memory

import "math"

// Embedder 把文本映射成语义向量（设计文档第五节：相似度只信真嵌入）。
// 哈希词袋之类的代理向量被禁止：换词不同义，套阈值等于随机。
type Embedder interface {
	Embed(text string) ([]float32, error)
	ModelName() string // 落库时记录，换模型后据此判断是否需要回填
}

// Cosine 余弦相似度。任一向量缺失返回 0——调用方据此降级，而不是拿假相似度继续算。
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Normalize 把向量归一化，便于合成查询向量后仍按余弦比较。
func Normalize(v []float32) []float32 {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return v
	}
	n := math.Sqrt(norm)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / n)
	}
	return out
}
