package memory

import (
	"net/http"
	"testing"
	"time"
)

// 本地模型集成测试：连不上就跳过，连上了就必须真的抽出结构化记忆。
// 默认走本机 Ollama 的 OpenAI 兼容接口（http://localhost:11434/v1），`go test -short` 可跳过。
func TestLocalModelExtract(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过本地模型测试")
	}
	o, ok := localOpenAI(t)
	if !ok {
		return
	}

	got, err := o.Extract("记住：我偏好深色主题的界面，晚上十点以后不要打扰我",
		time.Date(2026, 11, 12, 10, 0, 0, 0, time.Local))
	if err != nil {
		t.Fatalf("本地模型调用失败: %v", err)
	}
	if got == nil {
		t.Fatal("模型判定本轮无新信息，但这句话明显该记")
	}
	if got.Summary == "" {
		t.Error("摘要为空")
	}
	switch got.Layer {
	case LayerEpisodic, LayerSemantic, LayerProcedural, LayerIdentity:
	default:
		t.Errorf("层级应被归一化到合法值, got %q", got.Layer)
	}
	t.Logf("摘要=%q 层级=%s 标签=%v", got.Summary, got.Layer, got.Tags)
}

// 嵌入是召回与聚类的唯一相似度来源，必须真的能拿到向量且语义相近的句子更近。
func TestLocalModelEmbed(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过本地模型测试")
	}
	o, ok := localOpenAI(t)
	if !ok {
		return
	}

	a, err := o.Embed("用户偏好深色主题界面")
	if err != nil {
		t.Fatalf("嵌入失败: %v", err)
	}
	b, err := o.Embed("界面配色喜欢暗色系")
	if err != nil {
		t.Fatalf("嵌入失败: %v", err)
	}
	c, err := o.Embed("今天午饭吃了火锅")
	if err != nil {
		t.Fatalf("嵌入失败: %v", err)
	}
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("向量维度异常: %d vs %d", len(a), len(b))
	}
	near, far := Cosine(a, b), Cosine(a, c)
	if near <= far {
		t.Errorf("语义相近应更相似: 近=%v 远=%v", near, far)
	}
	t.Logf("维度=%d 近=%0.3f 远=%0.3f", len(a), near, far)
}

// localOpenAI 连不上本地 OpenAI 兼容服务（默认 Ollama 的 /v1）就跳过。
func localOpenAI(t *testing.T) (OpenAI, bool) {
	t.Helper()
	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Get("http://localhost:11434/v1/models")
	if err != nil {
		t.Skipf("本地 OpenAI 兼容服务不可达（%v），跳过", err)
		return OpenAI{}, false
	}
	resp.Body.Close()
	return OpenAI{
		BaseURL:    "http://localhost:11434/v1",
		Model:      "qwen3.5:0.8b",
		EmbedModel: "qwen3-embedding:0.6b",
		Client:     client,
	}, true
}
