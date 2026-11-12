// 桌面宠物记忆系统初版 CLI，实现见 doc/design.html。
package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"desktop-pet-memory/memory"
)

const usage = `桌面宠物记忆系统（实现见 doc/design.html）

用法:
  pet-memory add <turn...>        编码一轮对话（「明天下午3点」这类会解析成提醒时刻）
  pet-memory ask <query...>       召回相关记忆（查询也作为一轮写入工作记忆）
  pet-memory reflect              反思：结算衰减、身份提炼、提醒过期归档
  pet-memory remind               列出该提醒的事（→ 也列出「没说哪天」的）
  pet-memory due <ID> <日期 [时间]>  给「没说哪天」的提醒补时刻
  pet-memory list                 列出全部记忆（含已归档）
  pet-memory config [set <键> <值>] 查看/修改配置
  pet-memory version              版本号与 schema 版本

全局选项:
  -config <路径>   使用指定配置文件（默认 ~/.config/desktop-pet/config.json）

配置项见 README 或直接跑 pet-memory config；常用：
  base_url  OpenAI 兼容地址（带 /v1，空串=不调用模型）   model  语义判断模型
  embedder  嵌入模型        arbiter  仲裁模型        merge  自动合簇 true/false
`

// version 版本号：发版时改这里（配合 CHANGELOG.md）。
const version = "0.1.0"

// restoreConsole 由 useUTF8Console 赋值，放在包级是因为 die 走 os.Exit 会跳过 defer。
var restoreConsole = func() {}

func main() {
	restoreConsole = useUTF8Console()
	defer restoreConsole()

	args := os.Args[1:]
	cfgPath := configPath()
	if len(args) >= 2 && args[0] == "-config" { // 全局选项：指定配置文件（测试/多套配置用）
		cfgPath = args[1]
		args = args[2:]
	}
	if len(args) == 0 {
		fmt.Print(usage)
		return
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		die(err)
	}

	cmd, cmdArgs := args[0], args[1:]
	switch cmd {
	case "-v", "--version", "version":
		fmt.Printf("pet-memory %s (schema v%s)\n", version, memory.SchemaVersion())
		return
	case "config":
		if len(cmdArgs) == 0 {
			fmt.Println(cfg)
			return
		}
		if cmdArgs[0] != "set" || len(cmdArgs) < 2 {
			die(errors.New("用法: pet-memory config set <键> <值>；不带参数则显示当前配置"))
		}
		value := ""
		if len(cmdArgs) >= 3 {
			value = strings.Join(cmdArgs[2:], " ")
		}
		if err := cfg.set(cmdArgs[1], value); err != nil {
			die(err)
		}
		if err := cfg.save(); err != nil {
			die(err)
		}
		fmt.Println(cfg)
		return
	}

	store, err := memory.LoadStore(cfg.dbPath())
	if err != nil {
		die(err)
	}
	defer store.Close()

	ext, emb, crit := clients(cfg)
	eng := memory.NewEngine(store, ext, emb, crit)
	eng.MergeEnabled = cfg.Merge

	switch cmd {
	case "add":
		if len(cmdArgs) == 0 {
			die(errors.New("add 至少需要一轮对话"))
		}
		for _, t := range cmdArgs {
			store.AppendTurn(t) // 每轮推进一次逻辑时钟
		}
		mem, encErr := eng.Encode(memory.Event{
			ID:     fmt.Sprintf("ev-%d", store.Tick()),
			At:     time.Now(), // 只用于把「明天/下周三」锚定成绝对日期
			Turn:   strings.Join(cmdArgs, "\n"),
			Recent: store.RecentTurns(),
		})
		if flushErr := store.Flush(); flushErr != nil { // 先把工作记忆落盘，再报模型的问题
			die(flushErr)
		}
		if encErr != nil {
			// 模型不可用就不记这一轮（像发烧失忆）；怎么提示用户是 CLI 自己的策略。
			die(fmt.Errorf("本轮未写入记忆: %w", encErr))
		}
		if mem == nil {
			fmt.Println("（本轮没有新的长期信息）")
			return
		}
		for _, m := range mem {
			fmt.Printf("已存储 [%s] imp=%.2f str=%.2f %s\n", m.Layer, m.Importance, m.Strength, m.Content)
		}

	case "ask":
		if len(cmdArgs) == 0 {
			die(errors.New("ask 需要查询词"))
		}
		store.AppendTurn(strings.Join(cmdArgs, " ")) // 查询也是一轮对话，先进工作记忆
		results := eng.Retrieve(store.RecentTurns())
		if err := store.Flush(); err != nil { // 衰减与被动复习改动了记忆
			die(err)
		}
		if len(results) == 0 {
			fmt.Println("（无相关记忆）")
			return
		}
		for _, m := range results {
			fmt.Printf("[%s|%s] %s\n", m.Layer, m.Status, m.Content)
		}

	case "reflect":
		eng.Reflect(time.Now())
		if err := store.Flush(); err != nil {
			die(err)
		}
		fmt.Printf("反思完成，当前 %d 条记忆，逻辑时钟 tick=%d\n", len(store.All()), store.Tick())

	case "remind":
		due := eng.DueReminders(time.Now())
		pending := eng.PendingReminders()
		if err := store.Flush(); err != nil { // 标记了已提醒，要落盘
			die(err)
		}
		if len(due) == 0 && len(pending) == 0 {
			fmt.Println("（今天没有要提醒的事）")
			return
		}
		for _, m := range due {
			fmt.Printf("[%s] %s（应于 %s）\n", m.ID, m.Content, m.DueAt)
		}
		for _, m := range pending {
			fmt.Printf("[%s] %s —— 没说哪天，用 due %s <YYYY-MM-DD> 补上\n", m.ID, m.Content, m.ID)
		}

	case "due":
		if len(cmdArgs) != 2 {
			die(errors.New("due 需要 <记忆ID> <YYYY-MM-DD [HH:MM]>"))
		}
		if err := eng.ConfirmDue(cmdArgs[0], cmdArgs[1], time.Now()); err != nil {
			die(err)
		}
		if err := store.Flush(); err != nil {
			die(err)
		}
		fmt.Printf("已设定提醒日期：%s -> %s\n", cmdArgs[0], cmdArgs[1])

	case "list":
		mem := store.All()
		sort.Slice(mem, func(i, j int) bool { return mem[i].CreatedTick > mem[j].CreatedTick })
		for _, m := range mem {
			content := []rune(m.Content)
			if len(content) > 36 {
				content = content[:36]
			}
			fmt.Printf("%-16s %-10s %-8s imp=%.2f str=%.2f rev=%d tick=%d/%d %s\n",
				m.ID, m.Layer, m.Status, m.Importance, m.Strength, m.ReviewCount,
				m.LastReviewTick, store.Tick(), string(content))
		}
		if len(mem) == 0 {
			fmt.Println("（空）")
		}

	default:
		fmt.Print(usage)
	}
}

// openai 按配置返回 OpenAI 兼容客户端；BaseURL 为空串表示完全不调用模型。
func openai(cfg Config) (memory.OpenAI, bool) {
	if cfg.BaseURL == "" {
		return memory.OpenAI{}, false
	}
	return memory.OpenAI{
		BaseURL:    cfg.BaseURL,
		APIKey:     cfg.APIKey,
		Model:      cfg.Model,
		EmbedModel: cfg.Embedder,
	}, true
}

// clients 把配置变成一个 Engine 的三件套：抽取、嵌入、仲裁。
// 同一个 OpenAI 兼容客户端同时充当三者；仲裁模型单独可配（config arbiter），
// 实测 0.8b 判不准「补充/更新/矛盾/无关」，建议指向更大的本地模型；
// 判不准时的安全行为是"两条都留"，不会丢数据。
// BaseURL 为空串时三者都是 nil：编码不写记忆，召回退化为规则排序。
func clients(cfg Config) (memory.Extractor, memory.Embedder, memory.Critic) {
	o, ok := openai(cfg)
	if !ok {
		return nil, nil, nil
	}
	arbiter := o
	arbiter.Model = cfg.arbiter()
	return o, o, arbiter
}

func die(err error) {
	restoreConsole()
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}
