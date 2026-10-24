# Perplexity 订阅计费模式、Claude 额度核算、活动公告与上下文边界实测（2026-10-20）

本文档归档 `2026-10-20` 基于 `rea v6.7.0` (`58d604330`) 对 `https://www.perplexity.ai/` 的静态前端 bundle 与运行时 REST/SSE 接口逆向及实测结果。记录当时的订阅套餐结构、Claude 额度扣减规则、活动公告与时间戳、三层上下文窗口实测数据，以及后续政策或模型更新时的快速复查方法。

---

## 1. 快照时间、逆向版本与证据源

- **采样与实测时间**：`2026-10-20 01:55–03:45 EDT`（UTC `2026-10-20T05:55:00Z–07:45:00Z`）
- **逆向分析工具**：`rea v6.7.0` (`58d604330`)
- **核心前端 Bundle 证据源（`https://pplx-next-static-public.perplexity.ai/_spa/assets/`）**：
  - 全局路由、模式限流与会话状态：`spa-shell__v2-BF58qAEE.js`
  - 输入框 `40,000` 字符限制、附件 Token 估算与模型门禁：`ask-input__v2-BENIPtQg.js`、`composer-sdk__v2-CJuVxdMc.js`
  - 模型与推理强度菜单、无积分降级门禁：`SearchModelMenu__v2-qoFiw5-4.js`、`SearchModelMenuItems__v2-Ba1Ciq9d.js`
  - 套餐月度积分与首订奖励常量：`consts__v2-IiFetOo0.js`、`usePlanCreditBonus__v2-K3vPshmW.js`、`creditBonusAmounts__v2-PdmF6w5c.js`
  - 积分帮助中心 URL 与错误码映射：`constants__v2-ClxN2GoC.js`、`constants__v2-8Leu3mB0.js`
  - 线程级积分与免费 Pro 用量提示：`ThreadUsagePeriodToggleTip__v2-D_h2Sj9G.js`、`getThreadUsageDisplayValue__v2-DE82URoD.js`、`useGetThreadUsage__v2-9K6HBt2f.js`
  - 首页弹窗与公告分发器：`ModalUpsellSlot__v2-DpmBoUU1.js`、`ComputerFreeTurnsHomepageAnnouncementModal__v2-jkJx69qc.js`、`QuotaHomepageAnnouncementModal__v2-B9T7jrch.js`、`ComputerEmailAnnouncementModal__v2-1DC4RoJZ.js`、`SubscriptionPromoAnnouncementModal__v2-BJlm5kUx.js`
  - 中文国际化文案全集（`9,744` 条）：`zh-CN__v2-BLb2F2vm.js`

---

## 2. 当前生效的公告、促销时间节点与官方规则链接（截至 2026-10-20）

### 2.1 促销积分赠送与截止时间戳（`/rest/billing/ubb-config` & `consts__v2-IiFetOo0.js`）

Perplexity 当前在服务端 `/rest/billing/ubb-config` 与前端 `consts__v2-IiFetOo0.js` 中配置了针对个人（Consumer）与企业（Enterprise）的限时奖励积分（Bonus Credits）活动：

| 适用对象 | 活动名称 (`promotional_credit_grant_name`) | 月度固定套餐积分 | 一次性促销奖励积分 | 折算美元价值 (`100 credits = $1`) | 促销截止时间戳 (`promotional_credit_grant_deadline_unix`) | 奖励积分有效期 |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Consumer Pro** | `Pro Promo Bonus Credit` | `0`（含每周免费轮次 `free_turn`） | **`4,000` 积分** (`pro_plan_bonus_credits: 4e3`) | **`$40.00`** | **`1802822400`**（UTC **`2027-02-17T00:00:00Z`**） | 发放后 **`30` 天** (`expires_in_days: 30`) |
| **Consumer Max** | 首次升级 Max 奖励 (`00e0617c0e`) | **`10,000` 积分/月** (`max_plan_monthly_credits: 1e4`) | **`35,000` 积分** (`max_plan_bonus_credits: 35e3`) | 月度 **`$100`** + 首升奖励 **`$350`** | 前端常量配置（随套餐生效） | 月度积分当期有效，不结转 |
| **Enterprise Pro** | `Feb 2026 Enterprise Pro Bonus Credit` | **`500` 积分/席位/月** (`pro_plan_monthly_credits: 500`) | **`8,000` 积分** (`amount_cents: 8000`) | 月度 **`$5/席位`** + 促销 **`$80`** | **`1802822400`**（UTC **`2027-02-17T00:00:00Z`**） | 发放后 **`30` 天** |
| **Enterprise Max** | `Feb 2026 Enterprise Max Bonus Credit` | **`15,000` 积分/席位/月** (`max_plan_monthly_credits: 15000`) | **`40,000` 积分** (`amount_cents: 40000`) | 月度 **`$150/席位`** + 促销 **`$400`** | **`1802822400`**（UTC **`2027-02-17T00:00:00Z`**） | 发放后 **`30` 天** |

### 2.2 前端在线公告槽位（`ModalUpsellSlot__v2-DpmBoUU1.js`）

当前客户端首页弹窗槽位注册了以下 5 类活跃公告事件：

1. **`COMPUTER_FREE_TURNS_HOMEPAGE_ANNOUNCEMENT`（标签：`LIMITED PREVIEW`）**：
   - 组件：`ComputerFreeTurnsHomepageAnnouncementModal__v2-jkJx69qc.js`
   - 公告内容：Pro 套餐现已包含**每周自动重置的免费 Perplexity Computer 包含用量**（对应 `zh-CN` 文案 `[780be48548]: "您的套餐现已包含每周免费使用 Computer 的用量"`、`[246e7d1178]: "包含的用量每周重置。用完后，积分可让您继续使用 Computer。"`）。
   - 对应接口字段：`GET /rest/billing/asi-access-decision` 返回 `"budget_type": "free_turn"`。
2. **`QUOTA_HOMEPAGE_ANNOUNCEMENT`（标签：`Early access`）**：
   - 组件：`QuotaHomepageAnnouncementModal__v2-B9T7jrch.js`
   - 公告内容：新版用量配额系统（Usage Quota，对应接口 `GET /rest/billing/quota` 与 `GET /rest/billing/quota?allowances`）正在灰度测试。
   - 当前状态：2026-10-20 实测普通 Pro 账号请求 `/rest/billing/quota` 返回 `{"detail": {"error_code": "USAGE_QUOTA_FEATURE_DISABLED", "message": "Usage quota is not enabled for this user"}}`，说明当前仍沿用 `/rest/rate-limit/all` 的经典按次配额池。**后续若发现 `/rest/rate-limit/all` 失效，应第一时间检查 `/rest/billing/quota` 是否已全量转正。**
3. **`COMPUTER_EMAIL_HOMEPAGE_ANNOUNCEMENT`（标签：`NEW`）**：
   - 组件：`ComputerEmailAnnouncementModal__v2-1DC4RoJZ.js`（常量文件 `constants__v2-8Leu3mB0.js`）
   - 公告内容：支持将任意任务发送邮件至 `computer@perplexity.com` 直接触发云端 Computer 会话，执行结果与生成文件自动落入同一线程。
4. **`SUBSCRIPTION_PROMO`（Pro 7 天免费试用公告）**：
   - 组件：`SubscriptionPromoAnnouncementModal__v2-BJlm5kUx.js`（文案 `Try a week of Pro for free`）。
5. **`BRAINTREE_TRIAL_END_HOMEPAGE_UPSELL`（试用期到期挽留折扣）**：
   - 组件：`handleProTrialEndHomepageUpsell__v2-D8OdRT_D.js`（提供试用结束后续订百分比立减优惠 `[2f33d82bb0]`）。

### 2.3 官方计费与条款文档固定链接（硬编码于前端 Bundle）

- **个人套餐积分运作规则（`constants__v2-ClxN2GoC.js`）**：
  - `https://www.perplexity.ai/help-center/en/articles/13838041-how-credits-work-on-perplexity`
  - `https://www.perplexity.ai/help-center/articles/13838041-how-credits-work-on-perplexity#h_c31362c9d2`
- **企业组织积分与共享池指南（`constants__v2-ClxN2GoC.js`）**：
  - `https://www.perplexity.ai/help-center/en/articles/13901321-how-credits-work-for-enterprise-organizations-member-guide`
- **Claude Fable 5 企业特殊数据保留条款提示（`zh-CN` `[0f8d7d47e2]`）**：
  - 开启 `Claude Fable 5.1` (`claudefable51`) 需组织管理员额外勾选同意 Anthropic 针对 Fable 模型的特定输入输出存储条款。

---

## 3. 五档订阅计费模式与额度全景对照

Perplexity 在 `spa-shell__v2-BF58qAEE.js` 的 `IE(e)` 函数中根据 `subscription_status`（`active` / `trialing` / `white_glove_past_due`）与 `subscription_tier`（`free` / `basic` / `pro` / `max`）及 `org_uuid`（区分个人与 `Enterprise`）将用户划分为以下五档套餐，叠加按量购买的 **Computer Credits（积分池）**：

| 套餐档位 | 订阅价格 | 常规搜索与对话配额 (`mode: "search"` / `"copilot"`) | 高级研究模式配额 (`/rest/rate-limit/all`) | Computer 智能体积分 (`mode: "asi"`) | 文件与知识库上限 (`/rest/user/settings`) | 可用 Claude 模型矩阵 |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Free** | `$0` | 仅默认 `Sonar` (`turbo`)，每日约 `3–5` 次免费 Pro Search，隐藏模型切换菜单 (`payment_tier === "free"`) | `0` | 无 | 基础限制 | 不可手动选择 Claude |
| **Pro** (含 `trialing` / `Education Pro`) | **`$20/月`**（学生优惠 `$10/月`）<br>年付 `$200/年` (`~$17/月`) | `remaining_pro` 满额 **`200` 次/周**（自 2026-02-15 起个人 Pro 与学生 Education Pro 统一由日更 `300–600` 下调为 **`200` 次/周**滚动恢复，约 `~28` 次/天；部分 0 元第三方促销码账号会被进一步限缩至 `100` 次/周） | 深入研究 (`remaining_research`): **`20` 次/月**<br>Labs (`remaining_labs`): **`25` 次/月**<br>模型委员会 (`remaining_agentic_research`): **`0` 次** | 每周免费 `Light` 轮次 (`free_turn`) + 正价首订促销 **`4,000` 积分 (`$40`)**（`free_with_pm` 试用/教育号无 `$40` 赠金）；支持按需充值（最低 `$5 = 500` 积分，单月消费上限 `$100 = 10,000` 积分） | 单文件 **`50 MB`**（ASI/MCP 源 **`200 MB`**），每周上传上限 **`50` 个**（`upload_limit: 50`），单库 `50` 文件，全局 `500` 文件 | `claude55sonnet` (`Sonnet 5.5`)<br>`claude55sonnetthinking`<br>`claude50sonnet` (`Sonnet 5`)<br>`claude50sonnetthinking`<br>`claude55haiku` (`Haiku 5.5`)<br>`claude55haikuthinking`<br>`comet_browser_agent_sonnet` |
| **Max** (Consumer Max) | **`$200/月`**<br>年付 `$2,000/年` (`~$167/月`) | 最高优先级 `Pro Search`（`remaining_pro` 基准 `600`）+ Max 模型专属频控字典 (`model_specific_limits`) | 解锁最高频次深入研究与 Labs；**独占解锁「模型委员会 (`agentic_research`)」** | 每月固定赠送 **`10,000` 积分 (`$100/月`)** + 首次升级赠送 **`35,000` 积分 (`$350`)** | 解锁高阶文件编目同步 (`[00aa79a5f0]`) | 全系 Pro 模型 +<br>**`claude55opus` (`Opus 5.5`)**<br>**`claude55opusthinking`**<br>**`claudefable51` (`Fable 5.1`)**<br>**`claudefable51thinking`**<br>**`comet_browser_agent_opus` (`Opus 4.8`)** |
| **Enterprise Pro** | **`$40/席位/月`**<br>年付 `$325/席位/年` | 企业级 Pro Search 配额（**`400` 次/周**）+ 零数据保留（不用于训练） | 深入研究 **`50` 次/月** + Labs + 企业连接器（部分高级数据源如 BMJ/Statista/PitchBook 每月 `5` 次免费配额） | 每席位每月 **`500` 积分 (`$5`)** + 促销 **`8,000` 积分 (`$80`)**；支持组织共享积分池（最低充值 `$10`，月上限 `$2,000`） | 每周上传 **`100` 个** + 企业连接器与仓库扩展 | 同 Pro（不含 Opus 5.5 / Fable 5.1） |
| **Enterprise Max** | **`$325/席位/月`**<br>年付 `$271/席位/月` | 企业级最高搜索配额 + 零数据保留 | 全量开放深入研究、Labs 与模型委员会 | 每席位每月 **`15,000` 积分 (`$150`)** + 促销 **`40,000` 积分 (`$400`)** + 组织共享积分池 | 高达 `50,000` 组织文件容量 | 全系 Claude 模型（含 `Opus 5.5` 与需管理员授权的 `Fable 5.1`） |

> **注：独立开发者 API（Perplexity Sonar API）与上述 Web 端订阅隔离**，仅提供自研 `sonar` / `sonar-pro` / `sonar-reasoning-pro` / `sonar-deep-research` 接口按请求费 + Token 计费，不对外暴露 Claude 或 GPT 模型。

---

## 4. Claude 额度计算规则详解（按三大调用通道切分）

在 Perplexity 内部，调用 Claude 模型分为三个完全独立的计费通道（定义于 `/rest/models/config/v2` 的 `mode` 字段）：

### 4.1 通道一：常规搜索与多轮云端线程（`mode: "search"`，请求 `POST /rest/sse/perplexity_ask` 传 `mode: "copilot"`）

- **计费核心原则：按「提问轮次（Query Count）」扣减套餐次数池，消耗 `0` 积分，与单轮输入长度及历史累积上下文 Token 数完全脱钩。**
  - 前端证据：`ThreadUsagePeriodToggleTip__v2-D_h2Sj9G.js` 明确定义文案 `You get included usage with your Pro plan which does not consume credits`（中文包 `[0181996b40]: "您的 Pro 套餐提供的包含用量不会消耗积分。"`）。
  - 实测证据：在 15 轮多轮云端线程实测中，单线程累计注入 **`909,640` `o200k_base` tokens（近 `1M` 原生上下文）**，每一轮在服务端仅计为 **1 次普通 `pro_search` 查询**，不因携带近百万 Token 上下文而加倍扣减次数或扣除任何美元积分。
- **Pro 级 Claude 模型（`claude55sonnet` / `claude55sonnetthinking` / `claude55haiku`）**：
  - 共享扣减 `/rest/rate-limit/all` 中的 `remaining_pro` 池（个人 Pro 与学生 Education Pro 满额基准为 **`200` 次/周**，Enterprise Pro 为 **`400` 次/周**，按滚动窗口恢复）。
  - 底层通道：实测 `claude55sonnet` 的工具调用 ID 前缀为 `toolu_bdrk_01...`，证实由 **AWS Bedrock** 提供算力。
- **Max 级 Claude 模型（`claude55opus` / `claude55opusthinking` / `claudefable51` / `claudefable51thinking`）**：
  - **门禁与单模型独立限流 (`model_specific_limits`)**：在 `ask-input__v2-BENIPtQg.js` 的 `Zs()` 函数中，门禁判据为：
    ```javascript
    !(f?.subscription_tier === "max" && (n[e] ?? 0) <= 0 || ...)
    ```
    其中 `n` 对应 `/rest/rate-limit/all` 返回的 `model_specific_limits` 字典。这意味着在 Max 账号下，`Sonnet 5.5` 走通用 `pro_search` 池，而高成本的 `Opus 5.5` 与 `Fable 5.1` 由服务端通过 `model_specific_limits["claude55opus"]` 等键值单独下发剩余次数配额。
  - **Pro 账号越权请求時的静默降级**：当 Pro 账号在 `perplexity_ask` 中强行指定 `model_preference: "claude55opus"`（或 `"claude55opusthinking"`）时，服务端返回 HTTP `200`，保留 `user_selected_model: "claude55opus"`，但将 `_extras.pro_search_mode` 改为 `"auto"`，并将实际执行模型 `display_model` **静默降级为 `"gpt6_1_sol"`（或 `"gpt6_1_sol_thinking"`）**。

### 4.2 通道二：Perplexity Computer 自主智能体（`mode: "asi"`）

- **计费核心原则：按「美元积分（Credits，100 积分 = $1 USD）」实时结算底层 Token 与子代理消耗。**
  - 接口链路：提交前调用 `GET /rest/billing/credits/computer-submit-gate` 与 `GET /rest/billing/asi-access-decision` 校验余额；执行后通过 `GET /rest/billing/credits/thread-usage?thread_id=<id>` 查询该线程消耗的 `usage_cents`（`1 cent = 1 credit`）。
- **Computer 模式下的 Claude 编排器模型与推理档位（`/rest/models/config/v2`）**：
  - `pplx_asi_sonnet`（`Claude Sonnet 5.5`）：支持 `allowed_reasoning_efforts: ["low", "medium", "high", "xhigh"]`，默认 `"high"`。
  - `pplx_asi_opus` / `pplx_asi_opus_fast`（`Claude Opus 5.5`）：支持 `["low", "medium", "high", "xhigh", "max"]`，默认 `"medium"`；其中 `fast` 档在 `spa-shell__v2-BF58qAEE.js` 中标注为 `Increased credit usage`（积分消耗倍率上浮）。
  - `pplx_asi_fable_5`（`Claude Fable 5.1`）：支持 `["low", "medium", "high", "xhigh", "max"]`，默认 `"high"`。
  - `pplx_asi`（默认 Computer 模型，`provider: "ANTHROPIC"`）。
- **典型任务积分消耗区间（官方基准与前端文案 `[331c4e6898]`、`[05362d5def]`）**：
  - 简单事实查询 / 轻度任务：`15–70` 积分（`$0.15–$0.70`）
  - 多步深度研究报告：`350–950` 积分（`$3.50–$9.50`）
  - 数据分析看板构建：`2,400–9,800` 积分（`$24–$98`）
  - 完整应用或多日工程构建：`15,000+` 积分（`$150+`）
- **无积分时的强制锁死与降级机制（`SearchModelMenu__v2-qoFiw5-4.js`）**：
  - 当账号积分耗尽（`is_out_of_credits: true`，如未充值的 Pro 账号仅剩每周免费轮次 `budget_type: "free_turn"`）时，前端 `SearchModelMenu` 将 `variant` 置为 `"quota"` 或 `"free_turn_preview"`，弹出提示 `<modelName>{model}</modelName>适用于日常任务。添加积分即可使用更高级的模型。`（`[6c35b0d000]`），并将 Computer 编排器**强制锁定为免费的 `Light` 模式（`orchestrator_model: "pplx_asi_glm"`，即 Perplexity 后训练的 GLM 编排器）**，禁止切换至 `pplx_asi_sonnet` 或 `pplx_asi_opus`。

### 4.3 通道三：模型委员会 (`mode: "agentic_research"`) 与浏览器代理 (`mode: "browser_agent"`)

- **模型委员会 (`agentic_research`)**：
  - 仅限 `Max` 订阅（`subscriptionTier: "max"`）。
  - `/rest/models/config/v2` 中定义 `agentic_research_compare_models: ["gpt6_1_sol_thinking", "claude55opusthinking", "gemini4argonthinking"]`。每次提问同时调用这三个推理模型并汇总，扣减 1 次 `remaining_agentic_research`。
- **Comet 浏览器代理 (`browser_agent`)**：
  - `comet_browser_agent_sonnet`（`Claude Sonnet 5`）归属 `Pro` 档；`comet_browser_agent_opus`（`Claude Opus 4.8`）归属 `Max` 档。

---

## 5. 上下文窗口三层边界实测汇总（`claude55sonnet` & `gpt6_1_sol`）

核心边界实测结论如下：

| 层级 | 作用位置 | 实测硬边界 | 超限行为与关键机制 |
| :--- | :--- | :--- | :--- |
| **第一层：单轮输入 `query_str`** | `POST /rest/sse/perplexity_ask` 的 `params.query_str` | **`40,000` Unicode 字符 (`4e4`)** | **按 Unicode 字符数（非字节、非 Token）硬截断**。前端超 `40,000` 字符禁止直接提交并在粘贴时自动转为 `paste.txt` 附件；绕过前端直调 API 发送 `55,218`–`220,221` 字符返回 HTTP `200`，但服务端静默截断保留前 `40,000` 字符（`stored_query_len == 40000`）。单轮发送 `39,500` 个生僻汉字（`118.5 KB`，合 **`116,960` `o200k_base` tokens**）零截断、100% 召回。 |
| **第二层：多轮云端线程 (Cloud Thread)** | 同线程通过 `last_backend_uuid` + `read_write_token` + `frontend_context_uuid` 续接 | **`1,000,000` tokens (`1M` 原生上下文)** | 在 `909,640` `o200k_base` tokens（14 轮累积，`553,000` 汉字）下，`claude55sonnet` 与 `gpt6_1_sol` 均在不调用任何工具（仅 `INITIAL_QUERY` -> `FINAL`）的情况下 **100% 精确背出第 1 轮至第 11 轮任意偏移位置的未提问原文切片**。第 15 轮突破 `1M`（达 **`1,026,600` tokens**）时：**`claude55sonnet` 触顶硬报错 (`status: "failed"`, `answer: ""`)**；**`gpt6_1_sol` 触发滑动窗口淘汰最早的 Turn 1 (`status: "completed"`, `T1_SLICE=MISSING`)**。 |
| **第三层：文件附件 (`attachments`)** | `/rest/uploads/batch_create_upload_urls` + `/rest/sse/attachment_processing/subscribe` | 单文件 **`50 MB`**（前端 `500,000` tokens 仅弹警告 Toast，实测 `550,000` tokens / `3.8 MB` 放行） | 附件**不会**全量塞入模型 Prompt，仅在 `web_results` 注入前 `~1,958` 字符 `snippet`，完整文件挂载至后端 **Python 沙盒**（触发 `prefill_list_files` + `CODE_ASSET` 执行 `open('paste.txt')` 按需检索），耗时增加 `16s–36s`，且上传的 `s3_url` 绑定首次消费的线程，不可跨线程直接复用。 |

---

## 6. 后续更新快速复查指南（无需重跑完整逆向）

当怀疑 Perplexity 调整了模型列表、订阅配额、促销活动时间或启用了新版 `Usage Quota` 时，可在已登录 Perplexity 的浏览器控制台（DevTools Console）或通过携带 Cookie 的请求直接执行以下探针脚本，一键拉取最新实时配置并与本文档第 2–4 节比对：

```javascript
// 在 https://www.perplexity.ai 页面 DevTools Console 中直接运行：
(async () => {
  const endpoints = [
    "/rest/user/settings",
    "/rest/models/config/v2",
    "/rest/models/modes",
    "/rest/rate-limit/all",
    "/rest/rate-limit/status",
    "/rest/billing/ubb-config",
    "/rest/billing/asi-access-decision",
    "/rest/billing/credits/computer-submit-gate",
    "/rest/billing/quota"
  ];
  const out = { checked_at_utc: new Date().toISOString() };
  for (const ep of endpoints) {
    const res = await fetch(ep, { headers: { "x-perplexity-csrf-protection": "1" } });
    out[ep] = { status: res.status, data: await res.json().catch(() => null) };
  }
  console.log("=== Perplexity Live Billing & Model Snapshot ===", out);
  return out;
})();
```

**重点比对字段清单**：
1. **查模型上新或下线**：看 `/rest/models/config/v2` 的 `models` 与 `search_config`（关注 `subscription_tier` 是 `"pro"` 还是 `"max"`，以及 `replaced_by` 字段）。
2. **查普通对话配额与 Max 单模型限流**：看 `/rest/rate-limit/all` 的 `remaining_pro`、`remaining_research` 与 `model_specific_limits`。
3. **查新版配额系统是否转正**：看 `/rest/billing/quota` 是否不再返回 `USAGE_QUOTA_FEATURE_DISABLED`。
4. **查积分汇率、月度赠额与活动延期**：看 `/rest/billing/ubb-config` 的 `usd_cents_to_credits`、`pro_plan_monthly_credits`、`max_plan_monthly_credits` 与 `promotional_credit_grant_deadline_unix`。

---

## 7. 多账号池混搭与路由保护机制（`internal/proxy/account.go` -> `perplexityAccountRank`）

1. **多账号池并发叠加**：单号 Pro 每周 `200` 次（日均 `~28` 次）、Max 每周 `600` 次，可通过 `accounts/` 目录放入多个账号，由 `perplexityAccountRank` 自动挑选剩余 `remaining_pro` 最多的账号轮询分摊。
2. **控制首轮单次注入体积**：经过 `sanitizePerplexityContext` 裁剪后，Claude Code 首轮上下文压缩至 `< 8,000` 字符（`chunks=1`，仅扣 1 次）；若单轮超过 `38,000` 字符，每多切 1 个分片会额外扣减 1 次 `remaining_pro`。
3. **Pro / Max 自动分流保护**：
   - 下游请求 `claude-sonnet-5` / `sonnet-5.5` / `gpt-6.1-sol` 时，`perplexityAccountRank` 优先赋予 `plan_type == "pro"` 账号更高权重（`tierRank = 2`），优先消耗 Pro 号额度；仅当 Pro 号耗尽时才由 Max 号兜底（`tierRank = 1`）。
   - 下游请求 `claude-opus-5-5` / `opus-5.5` 时，严格过滤仅允许 `plan_type == "max"` 的账号接单，杜绝 Pro 号被服务端静默降级为 `gpt6_1_sol`。

