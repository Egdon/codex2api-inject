# Excel Basispoints 客户端兼容与回退

账号启用 Excel Basispoints 后，Responses 请求优先使用 BPS 适配器。本补丁处理客户端工具、agent 历史与图片提示值的协议差异，不改变账号的 BPS 开关。

## 工具与历史

- 中继生成的 function 参数为普通 JSON，响应显式携带 `encrypted_function_args: []`。直接工具调用带有明确加密声明时保留该声明。
- 对 `agent_message.content` 中误置于 `encrypted_content` 的明确自然语言明文，转换为 `input_text`，保留作者、接收者和完整文本。真正不透明的内容不做解码或改写。
- 原生 Responses 回放移除不兼容的展示用 `reasoning.content` 和 `status`，保留 `encrypted_content` 与 `summary`。
- 仅对已声明工具且解释唯一的 transport 错误进行恢复：误标 custom 的单个 JSON function 参数对象，或 raw 标记缺少可由声明唯一确定的字段。未声明目标、冲突 envelope、脚本、多个 JSON 值仍拒绝。
- 图片 `detail: "original"` 规范为 `high`；不重编码或修改图片数据。

## 何时使用原生 Responses

以下条件只在尚未向客户端写入响应且请求未取消时允许进入同一已获取账号的原生处理链路：

| 条件 | 日志 reason |
| --- | --- |
| `previous_response_id` | `stored_response` |
| BPS schema 无法表示的 agent/其他不透明上下文 | `agent_context` / `opaque_context` |
| 本地 BPS 请求准备拒绝为不支持 | `unsupported_request` |
| BPS 上游 HTTP 5xx | `upstream_5xx` |
| 精确 HTTP 403 + `basispoints_model_access_changed` | `model_access` |
| 传输错误或空响应 | `transport_error` |

回退响应带有 `X-Codex2api-Upstream-Fallback: basispoints-to-codex`。已发生的 BPS 失败保留为 retry-attempt 用量记录；提前判定不适合 BPS 的请求不伪造一次上游尝试。回退不替换模型、不切换 BPS 设置；后续原生处理继续使用既有调度及错误处理规则。

普通 401/403、上游使用策略拒绝、已输出后的失败及请求取消不会触发此回退。不要将“请求被使用策略拒绝”直接解释为永久封号：该提示本身未给出具体触发规则。

## 验证边界

单元和 handler 集成测试覆盖参数、历史、图片 detail、回退与账号释放。模拟 agent 协议通过不代表所有客户端的真实子代理流程已端到端验证。

本补丁不增加 BPS WebSocket，也不提供上游首字速度保证。HTTP/SSE 请求、文本首 delta 与完整工具参数可执行时间属于不同观测点，应分开测量。
