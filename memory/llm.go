package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Extractor 是编码线的 system2：判断本轮值不值得记、属于哪层、怎么表述。
// 返回 (nil, nil) 表示「本轮没有新的长期信息」；(nil, err) 表示「模型不可用」。
// 两者必须区分：前者正常结局，后者不写记忆、错误交回宿主。
// at 由宿主提供，只用于把「明天 / 下周三」锚定成绝对日期；零值表示没有时间信息。
type Extractor interface {
	Extract(turn string, at time.Time) (*Extracted, error)
}

// Critic 是反思与冲突判定用的同一套语义判断（设计文档第五、八节）。
// 只有相似度粗筛命中时才调用，所以调用量很小。
type Critic interface {
	Relation(old, new string) (Relation, error) // 补充 / 更新 / 矛盾 / 无关
	Merge(contents []string) (string, error)    // 空串 = 不该合并
	Identity(contents []string) (string, error) // 空串 = 不构成稳定画像
}

// Entities 小模型输出不稳：有时给字符串数组，有时给 [{"name":...}] 对象数组。
// 两种都收，其他形状直接丢弃——实体只是重要性评分的一个信号，不值得毁掉整条记忆。
type Entities []string

func (e *Entities) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for _, r := range raw {
		var s string
		if err := json.Unmarshal(r, &s); err == nil {
			*e = append(*e, s)
			continue
		}
		var obj struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(r, &obj); err == nil && obj.Name != "" {
			*e = append(*e, obj.Name)
		}
	}
	return nil
}

// OpenAI 走 OpenAI 兼容协议（/v1/chat/completions、/v1/embeddings）：
// 同一份实现对 Ollama、LM Studio、vLLM、llama.cpp server、以及任何云端 OpenAI 兼容服务都成立。
type OpenAI struct {
	BaseURL    string // 如 http://localhost:11434/v1（要带 /v1）
	APIKey     string // 可选；云端服务用它做 Bearer 认证
	Model      string // 语义判断，如 qwen3.5:0.8b
	EmbedModel string // 向量，如 qwen3-embedding:0.6b
	Client     *http.Client
}

const extractSchema = `{
  "type": "object",
  "properties": {
    "worth":      {"type": "boolean"},
    "layer":      {"enum": ["episodic", "semantic", "procedural", "identity"]},
    "summary":    {"type": "string"},
    "tags":       {"type": "array", "items": {"type": "string"}},
    "entities":   {"type": "array", "items": {"type": "string"}},
    "confidence": {"type": "number"},
    "is_reminder": {"type": "boolean"},
    "due":        {"type": "string"}
  },
  "required": ["worth", "layer", "summary", "tags", "entities", "confidence", "is_reminder", "due"]
}`

const relationSchema = `{
  "type": "object",
  "properties": {
    "relation": {"enum": ["supplement", "update", "contradict", "unrelated"]}
  },
  "required": ["relation"]
}`

const mergeSchema = `{
  "type": "object",
  "properties": {"merged": {"type": "string"}},
  "required": ["merged"]
}`

const identitySchema = `{
  "type": "object",
  "properties": {"identity": {"type": "string"}},
  "required": ["identity"]
}`

// Extract 只抽取「本轮」的长期信息，不喂上下文。
// 实测：给了工作记忆，0.8b 分不清本轮与上文，会把上文反复摘要成新记忆——
// 指代消解交给召回线（它本来就拿整段工作记忆去找线索），编码线只看本轮。
func (o OpenAI) Extract(turn string, at time.Time) (*Extracted, error) {
	today := "（未知，只能解析明确的绝对日期）"
	if !at.IsZero() {
		today = at.Format("2006-01-02")
	}
	prompt := fmt.Sprintf(`从下面这句「本轮」对话里抽取值得长期记忆的信息。
这句话没有值得长期记忆的内容时，worth 设为 false。
用户纠正或改变之前的说法（「我改成…」「我不再…」「其实…」）同样值得记，summary 写清新的状态。
layer 取值：episodic=发生过的一件事；semantic=关于用户的稳定事实或偏好；procedural=做事的方式或步骤；identity=用户是谁、长期不变的自我认知。
is_reminder：这句话是不是在让将来的某一天提醒某事（是/否）。
due：是提醒时输出那一天的绝对日期 YYYY-MM-DD（今天是 %s）；算不出来就输出空字符串，不要猜。
不是提醒时 is_reminder 为 false、due 为空。

示例：
本轮：记住：我每天晚上十一点睡觉
输出：{"worth":true,"summary":"用户每天晚上十一点睡觉","layer":"semantic","tags":["作息"],"entities":[],"confidence":0.9,"is_reminder":false,"due":""}

示例二（改主意也要保留新状态，不要把 summary 写成"改回了之前的说法"）：
本轮：我改成浅色主题了
输出：{"worth":true,"summary":"用户把界面主题改成了浅色","layer":"semantic","tags":["主题"],"entities":["浅色"],"confidence":0.9,"is_reminder":false,"due":""}

示例三（提醒；示例里的"今天"是 2020-01-01，只是演示换算，别照抄这个日期）：
本轮：明天提醒我买蛋糕
输出：{"worth":true,"summary":"用户要买蛋糕","layer":"episodic","tags":["提醒"],"entities":["蛋糕"],"confidence":0.9,"is_reminder":true,"due":"2020-01-02"}

示例四（是提醒但说不清哪天 → due 留空，由宿主追问）：
本轮：有空提醒我把书还了
输出：{"worth":true,"summary":"用户要还书","layer":"episodic","tags":["提醒"],"entities":["书"],"confidence":0.8,"is_reminder":true,"due":""}

本轮：
%s`, today, turn)

	var parsed struct {
		Worth      bool     `json:"worth"`
		Summary    string   `json:"summary"`
		Layer      string   `json:"layer"`
		Tags       []string `json:"tags"`
		Entities   Entities `json:"entities"`
		Confidence float64  `json:"confidence"`
		IsReminder bool     `json:"is_reminder"`
		Due        string   `json:"due"`
	}
	if err := o.generate(prompt, extractSchema, &parsed); err != nil {
		return nil, err
	}
	if !parsed.Worth || parsed.Summary == "" {
		return nil, nil
	}
	layer := Layer(parsed.Layer)
	switch layer {
	case LayerEpisodic, LayerSemantic, LayerProcedural, LayerIdentity:
	default:
		layer = LayerEpisodic
	}
	due, pending := resolveDue(turn, parsed.IsReminder, parsed.Due, at)
	return &Extracted{
		Worth:      true,
		Layer:      layer,
		Summary:    parsed.Summary,
		Tags:       parsed.Tags,
		Entities:   parsed.Entities,
		Confidence: parsed.Confidence,
		IsReminder: parsed.IsReminder,
		Due:        due,
		PendingDue: pending,
	}, nil
}

// Relation 判断新旧两条记忆的关系。相似度只负责挑候选，结论必须是语义判断。
// 提示词带两个示例：0.8b 在没有示例时会把「改口」判成 unrelated、把「无关」判成 update。
func (o OpenAI) Relation(old, new string) (Relation, error) {
	prompt := fmt.Sprintf(`比较旧记忆和新记忆，选一个关系：
- contradict：同一件事，新记忆给出相反的说法（旧的喜欢深色，新的改成浅色）
- update：同一件事，新记忆把旧的说法更新了（数值、名字、状态变了）
- supplement：新记忆是另一件相关的事，两条都成立
- unrelated：两件事没有关系

示例一：
旧：用户偏好深色主题
新：用户改成了浅色主题
答案：contradict

示例二：
旧：用户偏好深色主题
新：今天午饭吃了火锅
答案：unrelated

旧：%s
新：%s
答案：`, old, new)

	var parsed struct {
		Relation string `json:"relation"`
	}
	if err := o.generate(prompt, relationSchema, &parsed); err != nil {
		return "", err
	}
	r := Relation(parsed.Relation)
	switch r {
	case RelationSupplement, RelationUpdate, RelationContradict, RelationUnrelated:
		return r, nil
	default:
		return RelationUnrelated, nil
	}
}

// Merge 问「这些是不是同一件事，合并后怎么表述」，空串表示不该合并。
func (o OpenAI) Merge(contents []string) (string, error) {
	prompt := fmt.Sprintf(`下面几条记忆如果描述的是同一件事，用一句话合并表述；否则 merged 返回空字符串。
不要虚构原文没有的信息。

%s`, strings.Join(contents, "\n"))

	var parsed struct {
		Merged string `json:"merged"`
	}
	if err := o.generate(prompt, mergeSchema, &parsed); err != nil {
		return "", err
	}
	return strings.TrimSpace(parsed.Merged), nil
}

// Identity 问「这些记忆是否构成稳定的用户画像」，空串表示不构成。
func (o OpenAI) Identity(contents []string) (string, error) {
	prompt := fmt.Sprintf(`根据这些反复出现的语义记忆，判断是否构成稳定的用户画像（长期目标、偏好、边界）。
构成则用一句话表述，否则 identity 返回空字符串。不要虚构原文没有的信息。

%s`, strings.Join(contents, "\n"))

	var parsed struct {
		Identity string `json:"identity"`
	}
	if err := o.generate(prompt, identitySchema, &parsed); err != nil {
		return "", err
	}
	return strings.TrimSpace(parsed.Identity), nil
}

// ModelName 记录到记忆上，换嵌入模型后据此判断是否需要回填向量。
func (o OpenAI) ModelName() string { return o.EmbedModel }

func (o OpenAI) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return &http.Client{Timeout: 120 * time.Second}
}

func (o OpenAI) base() string {
	return strings.TrimRight(o.BaseURL, "/")
}

// generate 所有语义判断共用一个请求形态：JSON Schema 约束 + 关思维链 + 温度 0。
//
// 各家兼容度不同，按能力从强到弱退让，保证"换个服务也能跑"：
//  1. response_format=json_schema + reasoning_effort=none（约束最严、不烧思考）
//  2. response_format=json_object（只有宽松 JSON 约束的服务）
//  3. 不带 response_format（最老的服务）
//
// 空输出（finish_reason=length，思考把预算烧完）也会用更大预算重试一次。
func (o OpenAI) generate(prompt, schema string, out any) error {
	attempts := []struct {
		format   any
		effort   bool
		maxToken int
	}{
		{json.RawMessage(`{"type":"json_schema","json_schema":{"name":"result","strict":true,"schema":` + schema + `}}`), true, 512},
		{json.RawMessage(`{"type":"json_object"}`), false, 1024},
		{nil, false, 1024},
	}
	var lastErr error
	for _, a := range attempts {
		text, err := o.chat(prompt, a.format, a.effort, a.maxToken)
		if err != nil {
			lastErr = err
			continue // 服务不认这个字段：退到下一档
		}
		if strings.TrimSpace(text) == "" {
			lastErr = fmt.Errorf("模型返回空内容")
			continue
		}
		if err := json.Unmarshal([]byte(text), out); err != nil {
			lastErr = fmt.Errorf("解析模型输出: %w（原文：%.120s）", err, text)
			continue
		}
		return nil
	}
	return lastErr
}

// chat 一次 /chat/completions。返回 message.content 原文。
func (o OpenAI) chat(prompt string, format any, effort bool, maxTokens int) (string, error) {
	payload := map[string]any{
		"model":       o.Model,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"temperature": 0, // 判断是确定性任务：越稳定越好
		"max_tokens":  maxTokens,
		"stream":      false,
	}
	if effort {
		// qwen3/qwen3.5 默认开思维链，会把预算全烧在 thinking 上（实测 content 为空、finish=length）。
		// OpenAI 兼容服务普遍认这个字段；不认的服务会在下面退化成不带它的请求。
		payload["reasoning_effort"] = "none"
	}
	if format != nil {
		payload["response_format"] = format
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, o.base()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chat/completions: status %s: %.200s", resp.Status, data)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("chat/completions: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("chat/completions: 没有返回 choices")
	}
	return out.Choices[0].Message.Content, nil
}

// Embed 走 /v1/embeddings，返回该模型原始维度的向量。
func (o OpenAI) Embed(text string) ([]float32, error) {
	body, err := json.Marshal(map[string]any{"model": o.EmbedModel, "input": text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, o.base()+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings: status %s: %.200s", resp.Status, data)
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embeddings: 空向量")
	}
	return out.Data[0].Embedding, nil
}
