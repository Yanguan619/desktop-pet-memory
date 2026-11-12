# Changelog

## 0.1.0 — 2026-11-12（首个发布）

**提醒（新）**
- `due_at` 精确到分钟（`YYYY-MM-DD HH:MM`，本地时间），支持「明天下午3点」「今晚8点」「11月20日 09:15」；只说日期默认 09:00。
- 日期/时刻由代码确定性解析（`memory/dateparse.go`），模型只判断「是不是提醒」——0.8b 实测会把示例日期照抄、把「下周三」算成明天。
- 三级兜底：代码解析 → 模型给出的绝对时间（校验区间）→ `pending_due`，由宿主回问「哪天？」，绝不猜。
- `remind` 只响一次（`notified`）；过期且已交付的提醒在 `reflect` 时归档，原文保留；未交付的不归档。
- 提醒不参与关系仲裁：两条提醒是两件待办，实测 0.8b 会把「提醒我体检」判成「提醒我开会」的 update，导致前者被归档。
- 新命令：`remind`、`due <id> <YYYY-MM-DD [HH:MM]>`。

**嵌入（替换）**
- 默认嵌入模型从 `nomic-embed-text` 换成 `qwen3-embedding:0.6b`（1024 维）。旧模型对中文短句几乎不区分内容（「用户要买蛋糕」vs「用户要交房租」余弦 = 1.000）。
- 相似度按实测分布归一化（`[0.35, 0.90]` → 0~1），新增 `MinRelevance` 语义门槛：不相关的候选直接剔除，避免无关查询把记忆顶上来并强化它。
- 换嵌入模型时旧向量自动失效重算（按 `embedding_model` 判断），不会新旧向量混用。
- 阈值按新模型重标：`RelationThreshold` 0.65、`MergeThreshold` 0.85。
- 自动合簇（`PET_MERGE`）默认关闭：合并会改写当前生效的摘要。

**记忆（语义）**
- 遗忘改为「压缩」：强度有地板（0.03），归档记忆仍参与召回（排序打 0.5 折），被想起即恢复清晰。
- 衰减按「距上次结算」增量结算（`decayed_tick`）：修掉同一段时间被反复扣的 bug（实测 116 轮就归档 → 修好后 6594 轮）。
- 库里彻底不存墙钟时间（`created_at` 移除），时间戳只能由宿主按记忆 ID 自己记。

**召回（改进）**
- 门与排序分开：只用本轮最后一句算语义门槛（MIN_RELEVANCE），排序仍用 0.5^k 上下文向量。实测连续聊过界面后问「今天天气怎么样」，不再返回界面偏好。
- 纯追问（「那它呢」「继续」）跳过这道门，只靠上下文召回。
- 删掉恒为真的 `MIN_SCORE` 总分门槛（0.40×0.30 已等于 0.12）。

**模型接入**
- 模型层改为 **OpenAI 兼容协议**（`/v1/chat/completions` + `/v1/embeddings`）：Ollama、LM Studio、vLLM、llama.cpp server、DeepSeek、OpenAI 都可用，只改 `base_url` / `api_key` / 模型名。
- 兼容性自动退化：`json_schema` + `reasoning_effort=none` → `json_object` → 无 `response_format`；空内容（思考烧完预算）自动加大预算重试。实测 Ollama 的 `/v1` 接口忽略 `think:false` 但认 `reasoning_effort:"none"`。

**配置**
- 取消全部环境变量，改为 `~/.config/desktop-pet/config.json` + `pet-memory config [set <键> <值>]`；全局选项 `-config <路径>` 可指定另一份配置。配置项：`db` / `base_url` / `api_key` / `model` / `embedder` / `arbiter` / `merge`。
- 配置目录固定为 `~/.config/desktop-pet`（Windows 上也不再落 AppData）。

**结构**
- `engine.go` 拆分：召回线 → `recall.go`，反思与提醒 → `reflect.go`，编码线留在 `engine.go`。
- SQL 列清单收敛到 `memColumns` 单点生成（INSERT / ON CONFLICT / SELECT），并用 `TestStoreColumnsMatchSchema` 挡住"加了列忘了同步"。
- 三个模型构造函数合并为 `clients(cfg)`；`usage` 收敛，配置项只留一处详版（README）。

**工程**
- `Flush` 改为增量落盘：按行指纹只重写变化的记忆，`turns` 变了才重写（原为每轮全表重写，含 1024 维向量）。
- SQLite 持久化（`modernc.org/sqlite`，纯 Go 无 cgo，可交叉编译单文件 16MB）。
- `meta.schema_version` 校验：老库会明确报错而不是在 INSERT 时才炸；`meta.tick` 丢失时按记忆最大刻度自愈。
- `go test ./...` 即本地 CI（含 gofmt 检查与本地 Ollama 集成测试，连不上自动 skip）。
- 三平台交叉编译通过（linux/amd64、darwin/arm64、windows/amd64）。
