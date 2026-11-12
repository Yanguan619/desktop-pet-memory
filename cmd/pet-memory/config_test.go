package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigDefaultsAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model == "" || cfg.Embedder == "" || cfg.BaseURL == "" {
		t.Fatalf("默认值没展开: %+v", cfg)
	}
	// 没写 db 时落在配置文件同目录，不能落到 cwd（宠物是双击启动的，cwd 不可控）
	if want := filepath.Join(filepath.Dir(path), "pet-memory.db"); cfg.dbPath() != want {
		t.Errorf("dbPath = %q, want %q", cfg.dbPath(), want)
	}
	if cfg.arbiter() != cfg.Model {
		t.Error("未设 arbiter 时应回落到 model")
	}

	if err := cfg.set("db", filepath.Join(t.TempDir(), "x.db")); err != nil {
		t.Fatal(err)
	}
	if err := cfg.set("merge", "true"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.save(); err != nil {
		t.Fatal(err)
	}

	back, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.DB != cfg.DB || !back.Merge {
		t.Errorf("保存/读回不一致: %+v", back)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("临时文件应已 rename 掉")
	}
}

// configKeys 仅测试用：保证每个键都能被 set（防止文档与实现漂移）。
func configKeys() []string {
	return []string{"arbiter", "api_key", "base_url", "db", "embedder", "merge", "model"}
}

func TestConfigRejectsUnknownKeyAndBadValue(t *testing.T) {
	cfg := defaultConfig()
	if err := cfg.set("nope", "x"); err == nil {
		t.Error("未知键应报错，而不是静默无效")
	}
	if err := cfg.set("merge", "maybe"); err == nil {
		t.Error("非法布尔值应报错")
	}
	// 每个合法键都必须能被 set（防止文档与实现漂移）
	for _, k := range configKeys() {
		if err := cfg.set(k, "1"); err != nil {
			t.Errorf("键 %q 无法设置: %v", k, err)
		}
	}
}

// 清空某项（如 ollama_url）必须能落盘成空串，否则读回时会被默认值覆盖——
// 所以这里不写 omitempty，宁可文件里全是显式值。
func TestConfigCanClearField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.set("base_url", ""); err != nil {
		t.Fatal(err)
	}
	if err := cfg.save(); err != nil {
		t.Fatal(err)
	}
	back, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.BaseURL != "" {
		t.Errorf("清空 ollama_url 后仍读回 %q（默认值把它盖回去了）", back.BaseURL)
	}
}

// 打印出来的配置必须包含所有生效值，方便用户核对。
func TestConfigStringShowsEffectiveValues(t *testing.T) {
	cfg := defaultConfig()
	cfg.path = filepath.Join("x", "config.json")
	out := cfg.String()
	for _, want := range []string{"db", "base_url", "model", "embedder", "arbiter", "merge"} {
		if !strings.Contains(out, want) {
			t.Errorf("config 输出缺少 %q", want)
		}
	}
}
