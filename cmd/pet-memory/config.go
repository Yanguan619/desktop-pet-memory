package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 全部配置集中在一个 JSON 文件里，不再用一堆环境变量。
// 默认位置 <用户配置目录>/desktop-pet/config.json，可用 `pet-memory -config <路径>` 指定。
type Config struct {
	DB       string `json:"db,omitempty"`       // 记忆库路径（默认同目录 pet-memory.db）
	BaseURL  string `json:"base_url,omitempty"` // OpenAI 兼容地址（要带 /v1）；空串 = 完全不调用模型
	APIKey   string `json:"api_key,omitempty"`  // 云端服务的 Bearer token（本地服务留空）
	Model    string `json:"model,omitempty"`    // 语义判断模型
	Embedder string `json:"embedder,omitempty"` // 嵌入模型
	Arbiter  string `json:"arbiter,omitempty"`  // 冲突仲裁模型（默认同 Model）
	Merge    bool   `json:"merge,omitempty"`    // 反思时自动合簇（默认关闭）

	path string // 配置文件路径，不写入 JSON
}

func defaultConfig() Config {
	return Config{
		BaseURL:  "http://localhost:11434/v1",
		Model:    "qwen3.5:0.8b",
		Embedder: "qwen3-embedding:0.6b",
	}
}

// configPath 默认配置位置：<用户主目录>/.config/desktop-pet/config.json。
// 不用 os.UserConfigDir()——Windows 上它会落到 AppData，这里统一用 .config。
// 也不能依赖工作目录：宠物由桌面快捷方式/开机自启拉起，cwd 不可控。
func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "desktop-pet", "config.json")
	}
	return filepath.Join(home, ".config", "desktop-pet", "config.json")
}

// loadConfig 读配置；文件不存在就用默认值（不自动创建，避免在只读目录报错）。
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	cfg.path = path
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("配置文件 %s 解析失败: %w", path, err)
	}
	cfg.path = path
	return cfg, nil
}

func (c Config) save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		DB       string `json:"db"`
		BaseURL  string `json:"base_url"`
		APIKey   string `json:"api_key"`
		Model    string `json:"model"`
		Embedder string `json:"embedder"`
		Arbiter  string `json:"arbiter"`
		Merge    bool   `json:"merge"`
	}{c.DB, c.BaseURL, c.APIKey, c.Model, c.Embedder, c.Arbiter, c.Merge}, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path) // 原子替换：不会留下半个配置
}

// set 按名字设置一项；未知键直接报错（拼错时不会静默无效）。
func (c *Config) set(key, value string) error {
	switch strings.ToLower(key) {
	case "db":
		c.DB = value
	case "base_url", "url", "ollama_url": // ollama_url 是旧键名，保留以防旧配置习惯
		c.BaseURL = value
	case "api_key", "key":
		c.APIKey = value
	case "model":
		c.Model = value
	case "embedder":
		c.Embedder = value
	case "arbiter":
		c.Arbiter = value
	case "merge":
		switch strings.ToLower(value) {
		case "1", "true", "on", "yes":
			c.Merge = true
		case "0", "false", "off", "no", "":
			c.Merge = false
		default:
			return fmt.Errorf("merge 的值应为 true/false，收到 %q", value)
		}
	default:
		return fmt.Errorf("未知配置项 %q（可用：db, base_url, api_key, model, embedder, arbiter, merge）", key)
	}
	return nil
}

// dbPath 展开默认值：配置里没写就落在配置文件同目录。
func (c Config) dbPath() string {
	if c.DB != "" {
		return c.DB
	}
	return filepath.Join(filepath.Dir(c.path), "pet-memory.db")
}

func (c Config) arbiter() string {
	if c.Arbiter != "" {
		return c.Arbiter
	}
	return c.Model
}

// String 打印用：把生效值（含默认展开）列出来。
func (c Config) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "配置文件: %s\n", c.path)
	fmt.Fprintf(&b, "  db         = %s\n", c.dbPath())
	fmt.Fprintf(&b, "  base_url   = %s%s\n", c.BaseURL, onOff(c.BaseURL == "", "（空 → 不调用模型）", ""))
	fmt.Fprintf(&b, "  api_key    = %s\n", maskKey(c.APIKey))
	fmt.Fprintf(&b, "  model      = %s\n", c.Model)
	fmt.Fprintf(&b, "  embedder   = %s\n", c.Embedder)
	fmt.Fprintf(&b, "  arbiter    = %s%s\n", c.arbiter(), onOff(c.Arbiter == "", "（同 model）", ""))
	fmt.Fprintf(&b, "  merge      = %v\n", c.Merge)
	b.WriteString("修改：pet-memory config set <db|base_url|api_key|model|embedder|arbiter|merge> <值>")
	return b.String()
}

// maskKey 打印时不泄露完整密钥。
func maskKey(k string) string {
	if k == "" {
		return "（未设置）"
	}
	if len(k) <= 8 {
		return strings.Repeat("*", len(k))
	}
	return k[:4] + strings.Repeat("*", len(k)-8) + k[len(k)-4:]
}

func onOff(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}
