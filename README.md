# 桌面宠物记忆系统

给桌面宠物用的长期记忆引擎：五层记忆（情景 / 语义 / 程序 / 身份 / 工作记忆）× 三线管道（编码 / 召回 / 反思），只有编码时调用一次本地小模型，其余全是规则、嵌入与数学。

完整架构与设计取舍见 **[doc/design.html](doc/design.html)**（浏览器直接打开）。

## 快速开始

需要 Go 1.27+ 编译，本机有 [Ollama](https://ollama.com) 和两个模型（也可以指向任意 **OpenAI 兼容** 服务，见下方「配置」）：

```bash
ollama pull qwen3.5:0.8b         # 语义抽取 / 冲突仲裁（config: model）
ollama pull qwen3-embedding:0.6b # 语义向量（config: embedder；中文区分度正常，1024 维）
```

构建并试跑：

```bash
go build -o pet-memory ./cmd/pet-memory
./pet-memory add "记住：我偏好深色主题的界面，晚上十点以后不要打扰我"
./pet-memory ask "界面配色我喜欢什么风格"
./pet-memory reflect
./pet-memory list
```

## 目录

```
cmd/pet-memory/   CLI（package main）：入口、子命令、配置读写、控制台编码
memory/           库（package memory）：编码线 / 召回线 / 反思线 + SQLite 持久化
examples/         两个可运行片段：basic（编码 + 召回）、reminder（提醒交付）
doc/design.html   需求与架构的唯一来源
```

作为库使用时导入 `desktop-pet-memory/memory`；模块根不再放 `package main`，所以根路径本身可以被导入。

片段可直接跑（会用本机 Ollama 在当前目录建 `pet-memory.db`）：

```bash
go run ./examples/basic
go run ./examples/reminder
```

## CLI

| 命令 | 作用 |
|---|---|
| `add <turn...>` | 编码一轮对话（每个参数是一轮，依次进工作记忆并推进逻辑时钟；「明天下午3点 / 下周三 / 2026-11-15 09:30」会解析成提醒时刻） |
| `ask <query...>` | 召回相关记忆（查询本身也是一轮，先写入工作记忆） |
| `reflect` | 反思：结算衰减、身份层提炼、提醒过期归档 |
| `remind` | 列出该提醒的事（精确到分钟；读出即标记已提醒，同一个提醒只响一次；顺带列出"没说哪天"的提醒） |
| `due <id> <YYYY-MM-DD [HH:MM]>` | 给"没说哪天"的提醒补时刻（只给日期则默认 09:00） |
| `list` | 列出全部记忆（含已归档） |
| `config [set <键> <值>]` | 显示/修改配置 |
| `version` | 版本号与 schema 版本 |

## 配置

全部配置集中在一个 JSON 文件，**不用环境变量**：

```
~/.config/desktop-pet/config.json          # Windows: C:\Users\<你>\.config\desktop-pet\config.json
```

```bash
pet-memory config                                  # 显示配置（文件路径 + 生效值）
pet-memory config set db /path/to/pet-memory.db    # 修改并保存
pet-memory config set base_url ""                    # 空串 = 完全不调用模型
pet-memory config set base_url https://api.deepseek.com/v1
pet-memory config set api_key sk-xxxx
```

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `db` | `~/.config/desktop-pet/pet-memory.db` | SQLite 记忆库路径 |
| `base_url` | `http://localhost:11434/v1` | **任意 OpenAI 兼容地址**（要带 `/v1`）；设为空串则完全不调用模型 |
| `api_key` | 空 | 云端服务的 Bearer token；本地服务留空 |
| `model` | `qwen3.5:0.8b` | 语义判断模型 |
| `embedder` | `qwen3-embedding:0.6b` | 嵌入模型（换模型会自动失效旧向量并重算） |
| `arbiter` | 同 `model` | 冲突仲裁专用模型（0.8b 判关系不稳，建议指向更大的本地模型） |
| `merge` | `false` | 反思时自动合簇；合并会改写当前摘要，先回放验证再开 |

全局选项 `-config <路径>` 可指定另一份配置文件（多套配置/测试用）。

模型层只依赖 **OpenAI 兼容协议**（`POST /v1/chat/completions`、`POST /v1/embeddings`），兼容性处理是自动的，不需要手动开关：优先用 `response_format=json_schema` + `reasoning_effort=none`（约束最严、不烧思考链）；服务不认这两个字段就逐档退化为 `json_object`、再到不带 `response_format`；返回空内容（思考把 token 预算烧完）会用更大预算重试一次。

退出码：`0` 正常；`1` 出错。

模型不可用时**这一轮不产生长期记忆**（不是"降级存一条"）。原文会留在**工作记忆**里继续充当后续几轮的上下文，但工作记忆只是最近 8 轮对话的滚动窗口，它不会变成可召回的记忆——几轮之后就被挤出。这是有意的：没有可靠的语义判断就不记，像发烧时记不住事。

## 作为库使用

```go
import "desktop-pet-memory/memory"

store, err := memory.LoadStore(dbPath)   // 建库/建表/校验 schema 版本
defer store.Close()

model := memory.Ollama{URL: "http://localhost:11434", Model: "qwen3.5:0.8b", EmbedModel: "qwen3-embedding:0.6b"}
eng := memory.NewEngine(store, model, model, model) // Extractor / Embedder / Critic，缺哪个传 nil

store.AppendTurn(userTurn)              // 每轮对话：推进逻辑时钟 + 进工作记忆
mems, err := eng.Encode(memory.Event{ID: evID, Turn: userTurn})
if err != nil {
    // 模型不可用：这一轮不产生长期记忆（不是"降级存一条"）；怎么提示用户是宿主的事
}
if err := store.Flush(); err != nil {  // ← 必须自己落盘
    // ...
}
```

三条契约：

1. **落盘是宿主的责任。** `Encode` / `Reflect` 只改内存，必须调 `Store.Flush()`；忘了调用就是丢数据。
2. **单写者。** 同一时刻只让一个进程写同一个库（`Flush` 只重写变化的行，但没有多写者协议）。多个只读查看器没问题（WAL + busy_timeout 已开）。
3. **模型不可用 = 这一轮不产生长期记忆。** `Encode` 返回 `(nil, nil)` 表示本轮没有新信息，`(nil, err)` 表示模型不可用；两种都不落长期库，原文只留在工作记忆里（几轮后被挤出），库不替宿主决定降级策略。

## 核心机制（三句话）

1. **判断权分配**：语义价值只给模型，相似度只给嵌入，重要性/衰减/排序只给规则与数学；**规则不准判价值**。
2. **遗忘是压缩，不是消失**：归档只降低清晰度与排序权重，仍然可以被召回，被想起就重新变清晰。

## 测试

```bash
go test ./...          # 本地 CI：格式检查 + 单测 + 本地 Ollama 集成测试（连不上自动 skip）
go test -short ./...   # 只跑不依赖模型的单测
```

`go test` 自带 gofmt 检查（`format_test.go`）和 vet 的常用子集，不需要额外脚本。

## License

MIT
