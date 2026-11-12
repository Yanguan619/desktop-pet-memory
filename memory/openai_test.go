package memory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 任何 OpenAI 兼容服务都能用：请求形态（认证头、json_schema、关思维链）必须对。
func TestOpenAIRequestShape(t *testing.T) {
	var got map[string]any
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"relation\":\"unrelated\"}"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	o := OpenAI{BaseURL: srv.URL + "/v1", APIKey: "sk-test", Model: "m", EmbedModel: "e"}
	if _, err := o.Relation("旧", "新"); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/chat/completions" {
		t.Errorf("路径应为 /v1/chat/completions, got %q", path)
	}
	if auth != "Bearer sk-test" {
		t.Errorf("认证头不对: %q", auth)
	}
	if got["temperature"] != float64(0) {
		t.Errorf("temperature 应为 0, got %v", got["temperature"])
	}
	if got["reasoning_effort"] != "none" {
		t.Errorf("应关闭思维链（reasoning_effort=none）, got %v", got["reasoning_effort"])
	}
	rf, ok := got["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_schema" {
		t.Fatalf("应带 json_schema 约束, got %v", got["response_format"])
	}
}

// 不支持 reasoning_effort / json_schema 的服务要能退化：忽略字段、只给 json_object，最终仍能解析。
func TestOpenAIFallsBackOnUnsupportedFields(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if _, has := body["reasoning_effort"]; has {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"unknown field reasoning_effort"}`))
			return
		}
		if rf, ok := body["response_format"].(map[string]any); ok && rf["type"] == "json_schema" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"response_format json_schema unsupported"}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"relation\":\"contradict\"}"}}]}`))
	}))
	defer srv.Close()

	o := OpenAI{BaseURL: srv.URL + "/v1", Model: "m", EmbedModel: "e"}
	rel, err := o.Relation("旧", "新")
	if err != nil {
		t.Fatalf("应退化到兼容形态, got %v", err)
	}
	if rel != RelationContradict {
		t.Errorf("结论不应因退化而改变, got %q", rel)
	}
	if calls != 2 {
		t.Errorf("应退化一次就成功（json_schema → json_object）, got %d 次调用", calls)
	}
}

// 空内容（思考烧完预算）要重试，而不是把空串当结论。
func TestOpenAIRetriesOnEmptyContent(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"length"}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"identity\":\"用户偏好深色界面\"}"}}]}`))
	}))
	defer srv.Close()

	o := OpenAI{BaseURL: srv.URL + "/v1", Model: "m", EmbedModel: "e"}
	got, err := o.Identity([]string{"用户偏好深色主题"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "用户偏好深色界面" {
		t.Errorf("重试后应拿到内容, got %q", got)
	}
}

func TestOpenAIEmbedParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("路径应为 /v1/embeddings, got %q", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	o := OpenAI{BaseURL: srv.URL + "/v1", Model: "m", EmbedModel: "e"}
	v, err := o.Embed("x")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 3 || v[1] != 0.2 {
		t.Errorf("向量解析不对: %v", v)
	}
}

// 服务报错时必须把错误抛给宿主（模型不可用 → 这一轮不写记忆），不能静默返回空结论。
func TestOpenAIErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	o := OpenAI{BaseURL: srv.URL + "/v1", Model: "m", EmbedModel: "e"}
	if _, err := o.Embed("x"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("应返回带状态码的错误, got %v", err)
	}
	if _, err := o.Relation("旧", "新"); err == nil {
		t.Error("chat 失败必须报错")
	}
}
