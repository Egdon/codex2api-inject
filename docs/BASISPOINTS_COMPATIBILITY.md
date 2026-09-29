# Excel Basispoints 客户端兼容与回退

账号启用 Excel Basispoints 后，请求优先使用 BPS 适配器。本文说明启用方式、覆盖入口、客户端工具与历史的协议差异，以及何时回到原生 Codex。

## 启用方式

- **全局默认**：系统设置「Codex 传输 → Excel Basispoints」开启后，所有符合条件的 OAuth 账号默认走 BPS。中继、API Key 与 agent identity 账号始终不参与。
- **账号三态**：账号快捷配置可选「跟随全局 / 开启 / 关闭」，分别对应两项凭据标记都不存在、`openai_excel_bps=true`、`openai_excel_bps_opt_out=true`。账号显式开启时即使全局关闭也走 BPS；显式关闭时不受全局影响。
- **模型名单**：可选的全局模型名单（逗号、分号、空白或换行分隔，不区分大小写）非空时，只有名单内模型走 BPS，其余模型仍用原生 Codex。留空表示不限制。设置页可搜索、多选或输入自定义模型名，并可一键填入已知的 BPS 模型。
- 设置仅在数据库保存成功后发布到运行时。

## 覆盖入口

| 入口 | 行为 |
| --- | --- |
| `/v1/responses`、`/v1/responses/compact` | BPS 适配器直接处理 |
| `/v1/chat/completions`、`/v1/messages` | 翻译后的 Responses 请求经 BPS，结果按原入口格式输出 |
| 下游 Responses WebSocket | 该轮改用 HTTP 上游分支，先从调用方响应缓存展开 `previous_response_id` 再进入 BPS |

BPS 完成的回合写入调用方响应缓存，后续 `previous_response_id` 与原生回合一样展开。响应对象不回显 Excel 服务端指令、输入或原生工具，客户端只看到自己声明的工具。BPS 用量记录上游端点为 BPS，不按请求的 priority 档计费；实际运行的推理档（例如 `max` 发送为 `xhigh`）记为该条用量的推理强度。

## 工具与历史

- 中继生成的 function 参数为普通 JSON，响应显式携带 `encrypted_function_args: []`。直接工具调用带有明确加密声明时保留该声明。
- 对 `agent_message.content` 中误置于 `encrypted_content` 的明确自然语言明文，转换为 `input_text`，保留作者、接收者和完整文本。真正不透明的内容不做解码或改写。
- 原生 Responses 回放移除不兼容的展示用 `reasoning.content` 和 `status`，保留 `encrypted_content` 与 `summary`。
- 仅对已声明工具且解释唯一的 transport 错误进行恢复：误标 custom 的单个 JSON function 参数对象，或 raw 标记缺少可由声明唯一确定的字段。未声明目标、冲突 envelope、脚本、多个 JSON 值仍拒绝。
- 重复的工具声明只比较调用契约，忽略 `description` 与 `defer_loading`，客户端刷新工具目录时不再被判为冲突。
- 没有缓存原生条目的历史工具调用按当前目录声明的 transport 重建：custom 工具与单一 raw 字段的 function 使用 raw 形式，其余使用 JSON envelope。
- BPS 以 `invalid_encrypted_content` 拒绝无法校验的 reasoning（例如回退前由原生 Codex 生成）时，去掉这些 reasoning 条目重试一次；仍有其他不透明内容或重试后仍被拒绝时回到原生 Codex。
- 需要生图的请求走原生 Codex；未使用的 `image_generation` 工具（通常由网关注入）在进入 BPS 前直接移除，不再向提示词追加托管工具说明。
- 图片 `detail: "original"` 规范为 `high`；不重编码或修改图片数据。

## 何时使用原生 Responses

以下条件只在尚未向客户端写入响应且请求未取消时允许进入同一已获取账号的原生处理链路：

| 条件 | 日志 reason |
| --- | --- |
| `previous_response_id` 未能从调用方响应缓存展开 | `stored_response` |
| BPS schema 无法表示的 agent/其他不透明上下文 | `agent_context` / `opaque_context` |
| 需要实时联网搜索：`external_web_access: true`、`search_context_size: "high"` 或 `tool_choice` 指定 web_search | `web_search` |
| 显式或自然语言的生图意图 | `image_generation` |
| 本地 BPS 请求准备拒绝为不支持 | `unsupported_request` |
| BPS 上游 HTTP 5xx | `upstream_5xx` |
| 精确 HTTP 403 + `basispoints_model_access_changed` | `model_access` |
| 去掉 reasoning 重试后仍为 HTTP 400 `invalid_encrypted_content` | `encrypted_context` |
| BPS 上游 HTTP 429，或 HTTP 200 后、任何输出前的 `rate_limit_exceeded` 事件 | `rate_limited` |
| HTTP 200 后、任何输出前的 `server_is_overloaded` 事件 | `upstream_5xx` |
| BPS 上游 HTTP 401（由原生链路负责刷新令牌） | `auth` |
| 传输错误或空响应 | `transport_error` |
| 账号或模型的 BPS 路由处于暂停或 429 冷却中 | 不尝试 BPS，直接原生 |

`tool_choice: "none"` 时不因托管工具回退。回退响应带有 `X-Codex2api-Upstream-Fallback: basispoints-to-codex`。已发生的 BPS 失败保留为 retry-attempt 用量记录；提前判定不适合 BPS 的请求不伪造一次上游尝试。回退不替换模型、不修改账号的 BPS 设置；后续原生处理继续使用既有调度及错误处理规则。

`basispoints_model_access_changed` 以外的 403、上游使用策略拒绝、已输出后的失败及请求取消不会让当前请求回退，错误照常返回；403 会按下节暂停路由，之后的请求直接走原生。不要将“请求被使用策略拒绝”直接解释为永久封号：该提示本身未给出具体触发规则。

## 403 暂停、429 冷却与自动探测

- **429**：只冷却该账号的 BPS 路由，时长取上游 `Retry-After`（限制在 1–600 秒）；没有可用的 `Retry-After` 时使用「限流冷却时间」设置（1–600 秒，默认 5 秒）。不影响原生 Codex 调度。BPS 的 token 速率限制通常以 HTTP 200 加 `error`/`response.failed` 事件返回；网关在输出前最多检查 3 秒的生命周期事件，识别到后按 429 处理，已读取的事件原样交给桥接层。
- **403**：`basispoints_model_access_changed` 暂停该账号的该模型，其他 403 暂停该账号全部 BPS 路由。暂停是运行时状态，不修改账号配置或 Codex 调度状态。账号级暂停保存在共享运行时缓存中，重启后保留；模型级暂停只在内存中。
- **探测**：后台按「探测间隔」（1–10080 分钟，默认 1 分钟）检查暂停的路由。先请求 BPS access 端点：模型级暂停要求返回的模型列表包含该模型；模型列表读取 `model_catalog.models[].id` 并排除 `model_catalog.restricted_models`，兼容旧版顶层 `models`。账号级暂停要求 `allowed=true`（实测被使用策略 403 的账号 access 仍返回 `allowed=true`，所以不能只看 access），并再发一次要求原样返回随机令牌的最小生成（带 Excel 服务端提示词，约 2.2 万输入 token，大部分命中缓存）。探测成功即恢复，失败保持暂停并等待下一次。
- **管理**：系统设置可关闭自动暂停，调整探测间隔和限流冷却时间（`codex_basispoints_429_cooldown_seconds`，越界返回 400）。账号列表显示 BPS 暂停或冷却状态，管理员可立即恢复（`POST /api/admin/accounts/:id/bps-pause/clear`）。账号被删除、失去资格或不再启用 BPS 时，其暂停在下一次探测时清除。

## 验证边界

单元和 handler 集成测试覆盖参数、历史、图片 detail、托管工具判定、reasoning 重试、回退、暂停与探测及账号释放。模拟 agent 协议通过不代表所有客户端的真实子代理流程已端到端验证。

BPS 上游只使用 HTTP/SSE，不提供上游 WebSocket，也不提供首字速度保证。HTTP/SSE 请求、文本首 delta 与完整工具参数可执行时间属于不同观测点，应分开测量。
