# Perplexity Code Bridge

一个专为 **Claude Code CLI** 打造的本地 Anthropic `/v1/messages` 协议桥接器与隔离启动器（`claude-pplx.py`），底层对接 Perplexity Web 端 `v2.18` REST/SSE 接口（`POST /rest/sse/perplexity_ask`）。

通过将 Anthropic 原生工具定义（`tools[]`）与延迟工具目录（`<available-deferred-tools>`）转换为紧凑的 **`LOCAL ACTION PROTOCOL`**，本桥接器让云端模型（`Claude Sonnet 5.5`、`Claude Opus 5.5`、`GPT-6.1 Sol`）能够在本地终端 100% 驱动 **文件读写（`Read` / `Write` / `Edit`）、命令执行（`Bash`）、代码检索（`Glob` / `Grep`）、本地技能加载（`Skill` / `SKILL.md`）、延迟工具按需检索（`ToolSearch`）以及本地多子代理协同（`Agent` / `SendMessage`）**。

---

## 核心特性

1. **完整的本地工具调用闭环（Local Tool Execution）**
   - 自动将 Claude Code 下发的 JSON Schema 工具表压缩为 `## Functions` 签名描述，并在每轮末尾锚定 `LOCAL ACTION PROTOCOL`。
   - 内置 `tool_guard` 校验与自愈层（`internal/proxy/tool_guard.go` + `tool_guard_escape.go`）：自动修复模型输出中常见的未转义双引号、容忍常见参数别名（如 `file_path` ↔ `path`、`cmd` ↔ `command`）、拦截缺参/幻觉工具调用，再转换为标准 Anthropic `content_block_start (type: "tool_use")` + `input_json_delta` 流式事件返回给客户端。
   - 内置云端沙盒拦截与拒答自愈：当上游模型误以为自身处于云端只读环境而拒答时，桥接器自动检测并开启新线程携带纠偏指令重试一次（`shouldRetryPerplexityLocalToolDenial`），对客户端完全透明。

2. **多轮云端线程续接（1M 原生上下文 & 按次计费友好）**
   - 自动提取并缓存上游 SSE 返回的 `backend_uuid`、`read_write_token` 与 `frontend_context_uuid`（`perplexityThreadRegistry`）。
   - **首轮（Turn 1）** 发送经过 `sanitizePerplexityContext` 脱敏精简后的系统提示词、工具表与首条提问；**后续轮次（Turn 2+ / `mode=followup`）** 仅增量发送最新的 `LOCAL TOOL RESULT` 或用户追问，完整复用云端线程最高 **1,000,000 tokens** 的原生上下文，且每轮仅消耗 1 次 `remaining_pro` 查询配额。
   - 单轮超大输入（`> 38,000` Unicode 字符）自动按安全边界分片上送，并确保 `LOCAL ACTION PROTOCOL` 始终锚定在最后一个分片。

3. **双通道本地 Skills 索引与加载**
   - **Claude 原生技能（`Skill` 工具）**：默认注入 `--setting-sources user,project,local`，自动识别工作区 `.claude/skills/` 与用户级 `~/.claude/skills/`，支持通过 `Skill` 工具或斜杠命令触发。
   - **跨 Agent 扩展技能目录（`~/.agents/skills/` 自动索引 + `Read` 兜底）**：`claude-pplx.py` 启动时自动扫描 `~/.agents/skills/*/SKILL.md` 的 YAML Frontmatter（`name` 与 `description`），生成带真实路径的技能清单通过 `--append-system-prompt` 注入；当 `Skill` 工具无法解析外部技能名时，模型会自动调用 `Read` 直接读取对应的 `SKILL.md` 正文执行。

4. **本地 Subagents（`Agent` / `SendMessage`）与按需工具加载（`ToolSearch`）**
   - 默认开启 `Agent`、`SendMessage` 与 `ToolSearch`（`ENABLE_TOOL_SEARCH=true`）。
   - 自动透传 `<available-deferred-tools>` 列表；当任务需要 MCP 工具或延迟加载工具时，模型先调用 `ToolSearch` 拉取完整 Schema，下一轮自动合并进 `## Functions` 激活调用。
   - 支持从 `~/.local/share/perplexity-code-bridge/agents/*.md`（或环境变量 `CLAUDE_PPLX_AGENTS_DIR`）加载自定义子代理定义，并通过 `--agents` 挂载到 Claude Code。

5. **多账号池与 Pro / Max 分级路由**
   - 支持在 `accounts/` 下放置多个账号 JSON，启动时与后台定时调用 `/rest/rate-limit/all` 和 `/rest/user/settings` 刷新实时配额与套餐档位。
   - 请求 `sonnet-5.5` / `gpt-6.1-sol` 时优先路由至 `pro` 账号（按剩余 `remaining_pro` 加权轮询），保护 `max` 账号额度；请求 `opus-5.5` 时严格路由至 `max` 账号，并实时监控响应模型防止静默降级。

---

## 架构与单轮工具调用数据流

```text
Claude Code CLI (本地终端)
  │  1. claude-pplx.py 注入 --settings / --tools / Skills 索引 / 状态栏脚本
  ▼
POST http://127.0.0.1:18082/v1/messages (Anthropic Messages API)
  │  2. sanitizePerplexityContext 清洗冗余头部，保留 <available-deferred-tools> 与项目指令
  │  3. 注入 ## Functions 紧凑 Schema 表 + LOCAL ACTION PROTOCOL
  ▼
POST https://www.perplexity.ai/rest/sse/perplexity_ask (HTTP/2 + Chrome TLS 指纹)
  │  4. 首轮新建线程 (mode=new)；多轮/工具回传凭 last_backend_uuid + read_write_token 增量续接 (mode=followup)
  ▼
上游 SSE 事件流 (blocks -> diff_block / thinking / answer)
  │  5. 实时解析 <thinking> 思考块与 JSON 工具调用
  │  6. tool_guard 修复转义并校验必填参数；若检测到“无本地权限”误拒答则自动重放自愈
  ▼
Anthropic SSE 响应流 (thinking_delta / tool_use / text_delta)
  │  7. Claude Code 在本机执行 Bash / Read / Edit / Write / Skill / Agent / ToolSearch
  └─ 8. 下一轮携带 tool_result 自动转为 LOCAL TOOL RESULT 续接同一云端线程
```

---

## 快速开始

### 1. 环境要求

- Go `1.22+`
- Python `3.9+`（需安装 `pyyaml`：`pip install pyyaml`）
- Claude Code CLI（确保 `claude` 在 `PATH` 中可执行）

### 2. 配置账号与服务参数

```bash
# 1. 复制配置文件模板（可选；未创建时服务首次启动会自动生成默认 config.yaml 与随机 api_key）
cp config.example.yaml config.yaml

# 2. 创建本地账号文件（accounts/*.json 已在 .gitignore 中忽略，不会被 Git 追踪）
cp accounts/account.example.json accounts/my_account.json
```

编辑 `accounts/my_account.json`，填入已登录浏览器会话中的 `__Secure-next-auth.session-token` 值（完整 5 段式 `eyJhbGci...` JWE 字符串）或完整 Cookie：

```json
{
  "provider": "perplexity",
  "session_token": "eyJhbGciOiJkaXIiLCJlbmMiOiJBMjU2R0NNIn0..xxxx.yyyy.zzzz",
  "user_email": "user@example.com",
  "user_name": "Local User",
  "plan_type": "pro"
}
```

### 3. 一键启动 Claude Code CLI

直接运行 `claude-pplx.py`。若本地 `127.0.0.1:18082` 尚未运行 `perplexity-bridge`，启动器会自动编译二进制并在后台拉起服务：

```bash
# 默认启动（模型 sonnet-5.5，推理强度 medium，自动挂载底部实时配额状态栏）
python3 claude-pplx.py

# 指定 1M 上下文模型 gpt-6.1-sol 启动
python3 claude-pplx.py --model gpt-6.1-sol

# 使用 Max 账号启动旗舰模型 opus-5.5 并开启高推理强度
python3 claude-pplx.py --model opus-5.5 --effort high

# 非交互单次执行（适合脚本或 Harness 自动化验证）
python3 claude-pplx.py -p "用 Bash 工具运行 go version，并读取 go.mod 的模块名"
```

也可以手动编译并前台运行桥接服务以查看实时调试日志：

```bash
go build -o perplexity-bridge ./cmd/perplexity-bridge
./perplexity-bridge
```

---

## 工具调用（Tools）、Skills 与 Subagents 详解

### 1. 默认启用的本地工具矩阵

`claude-pplx.py` 启动时默认通过 `--tools` 向 Claude Code 暴露以下 10 个核心与扩展工具：

| 工具名 | 类别 | 桥接器处理机制与用途 |
| :--- | :--- | :--- |
| `Read` / `Write` / `Edit` | 核心文件工具 | 压缩注入参数签名，`tool_guard` 校验路径与内容参数，直接在本地工作目录读写修改代码。 |
| `Bash` | 核心命令工具 | 在本机执行终端命令、编译与测试，执行输出在下一轮通过 `LOCAL TOOL RESULT` 增量回传。 |
| `Glob` / `Grep` | 核心检索工具 | 本地文件名匹配与正则内容搜索。 |
| `Skill` | 技能加载工具 | 加载 `.claude/skills` 与 `~/.claude/skills` 中的本地技能。 |
| `ToolSearch` | 延迟工具检索 | 配合 `<available-deferred-tools>` 按需搜索并激活 MCP 工具或扩展工具 Schema（默认开启 `ENABLE_TOOL_SEARCH=true`）。 |
| `Agent` | 本地子代理 | 在本地 Claude Code 进程内派生独立子代理（`CLAUDE_CODE_SUBAGENT_MODEL` 默认 `sonnet-5.5`），每个子代理在桥接层拥有独立的云端线程绑定。 |
| `SendMessage` | 子代理续接 | 按 Agent ID 向已创建的本地子代理发送后续指令并恢复其专属云端线程。 |

#### 自定义增减工具（环境变量控制）

- **追加额外工具**：`export CLAUDE_PPLX_EXTRA_TOOLS="WebFetch,NotebookEdit"`
- **禁用可选扩展工具**：`export CLAUDE_PPLX_DISABLE_TOOLS="SendMessage"`（注：核心工具 `Read,Edit,Write,Bash,Glob,Grep,Skill,Agent` 受保护不可禁用，误配会直接报错拦截）。

### 2. Skills 双路径工作流

`claude-pplx.py` 中的 `discover_skill_context()` 实现了两层技能发现：

1. **原生层（`.claude/skills/` & `~/.claude/skills/`）**：由 Claude Code 原生扫描并注册到 `Skill` 工具。
2. **扩展层（`~/.agents/skills/`）**：启动器自动解析每个技能目录下 `SKILL.md` 的 YAML Frontmatter（`name` 与 `description`），将去重后的技能目录与 `Source: /path/to/SKILL.md` 注入系统提示词。当模型匹配到某个技能描述时，优先调用 `Skill` 工具；若为 `~/.agents/skills/` 下的扩展技能，模型会自动调用 `Read` 工具读取 `Source` 指向的 `SKILL.md` 并严格按步骤执行。

### 3. 自定义 Subagents（`--agents`）

在 `~/.local/share/perplexity-code-bridge/agents/`（可通过 `CLAUDE_PPLX_AGENTS_DIR` 覆盖）下放入带 YAML Frontmatter 的 `.md` 文件即可自动注册自定义子代理：

```markdown
---
name: code-reviewer
description: 审查代码改动并定位潜在并发或边界缺陷
model: sonnet-5.5
effort: high
tools:
  - Read
  - Grep
  - Glob
  - Bash
---
你是一名严谨的代码审查专家，请先用 Read 和 Grep 核实调用链，再输出带 file:line 证据的审查结论。
```

---

## 支持的模型与上下文窗口

所有模型标识均支持不带前缀（推荐）或带 `perplexity/` 前缀：

| CLI 模型名 (`--model`) | 上游目标 (`model_preference`) | 所需账号档位 | 客户端自动压缩窗口 | 云端线程原生上下文 |
| :--- | :--- | :--- | :--- | :--- |
| `sonnet-5.5` / `claude-sonnet-5` | `claude55sonnet`（带推理时 `claude55sonnetthinking`） | `Pro` / `Max` | `200,000` | `1,000,000` tokens |
| `opus-5.5` / `claude-opus-5-5` | `claude55opusthinking` / `claude55opus` | **仅限 `Max`** | `200,000` | `1,000,000` tokens |
| `gpt-6.1-sol` / `claude-sol-6-1` | `gpt6_1_sol`（带推理时 `gpt6_1_sol_thinking`） | `Pro` / `Max` | `1,000,000` | `1,000,000` tokens |
| `auto` | 默认映射至 `claude55sonnet` | `Pro` / `Max` | `200,000` | `1,000,000` tokens |

- **推理强度（`--effort`）**：支持 `low`、`medium`（默认）、`high`。当 `effort` 为 `medium` 或 `high`（或请求开启 `thinking`）时，桥接器自动切换到上游对应的 `*thinking` 模型变体并流式输出 `<thinking>` 块。
- **底部实时状态栏（`scripts/pplx-statusline.py`）**：终端底部每 10 秒刷新一次当前模型、对话轮数（`Turn`）、上下文占用百分比（`Ctx`）、推理档位（`Effort`）、账号池实时剩余额度（`Pro:<remaining>` / `Max:<remaining>`）以及 Git 分支与变更统计。

---

## HTTP 接口一览

默认监听 `http://127.0.0.1:18082`（`/v1/*` 与 `/models` 校验 `Authorization: Bearer <api_key>` 或 `x-api-key`）：

| 方法与路径 | 说明 |
| :--- | :--- |
| `POST /v1/messages` | Anthropic Messages API 主入口（支持流式 SSE 与非流式 JSON、工具调用、多轮会话续接） |
| `POST /v1/messages/count_tokens` | 本地启发式 Token 估算接口（供 Claude Code 判断上下文占用与触发自动压缩） |
| `GET /v1/models` | 返回支持的模型列表（兼容 Anthropic / OpenAI `/v1/models` 格式） |
| `GET /health` | 健康检查、在途请求数（`in_flight`）与号池配额汇总 |
| `GET /admin/accounts` | 查看账号池各账号状态、可用性（`available`）、套餐档位与实时剩余配额（`quota_info.premium_balance`） |
| `POST /admin/accounts/reload` | 从磁盘 `accounts/` 目录热重载账号文件（无需重启进程） |
| `POST /admin/accounts/refresh` | 立即触发全量账号向 Perplexity 上游刷新实时剩余配额与套餐状态 |

---

## Harness 实测与自动化测试验证

本仓库包含完整的单元测试与真实 CLI Harness 验证链路：

```bash
# 1. 运行 Go 核心协议、工具守卫 (tool_guard)、多轮续接、拒答自愈与无品牌泄露单测
go test -v ./... -count=1

# 2. 运行 Python 启动器、Skills 发现、自定义 Subagents 与状态栏渲染单测
python3 -B -m unittest discover -s tests -v

# 3. 真实 Claude Code CLI Harness 端到端工具调用验证（需配置有效账号）
python3 claude-pplx.py -p "用 Bash 工具运行 go version，然后直接回复输出的精确版本号，只回一行。"
```

真实 Harness 运行输出示例：

```text
[claude-pplx] tools (--tools): Read,Edit,Write,Bash,Glob,Grep,Skill,Agent,SendMessage,ToolSearch
[claude-pplx] model=sonnet-5.5 effort=medium via http://127.0.0.1:18082
go version go1.26.5 darwin/arm64
```

对应服务端日志可见完整的两轮闭环（首轮 `mode=new` 触发本地 `Bash` 工具调用，次轮 `mode=followup` 凭 `context` 续接同一云端线程回传 `LOCAL TOOL RESULT` 并生成最终回答）。

---

## 深入文档

- [`docs/perplexity-local-tools.md`](docs/perplexity-local-tools.md) — 本地工具桥接协议（`LOCAL ACTION PROTOCOL`）、分片规则、拒答自愈与上下文清洗细节
- [`docs/perplexity-billing-and-limits.md`](docs/perplexity-billing-and-limits.md) — 上游订阅档位、配额扣减机制与三层上下文窗口实测边界
