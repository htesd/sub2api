# Codex 请求诊断与请求控制（未发布）

在账号创建、编辑或批量编辑中开启「Codex 请求控制」。默认关闭，只针对 OpenAI OAuth 的 Responses、compact 和 WS Responses；不会自动更改现有账号配置。实现优先级如下。

1. **诊断**：关联请求、WS turn、账号和真实传输操作。记录改写过的头名称、状态分类、耗时和 token 用量，不记录原始 UA、会话标识、凭据或 attestation token。`codex request context` 将现有日志上下文连接到 `route_request_id`；按该 ID 查 `codex_upstream_attempt/result`、`codex_forward_result`、`codex_request_summary`。HTTP 收到响应头不等于生成成功，以 forward result 为准。
2. **重试预算与冷却**：首个开启策略的账号确定本次请求的预算，跨账号、HTTP/WS 重试共享。默认最多 6 次传输操作，30 秒后禁止发起新的操作；已开始的流不会因为这个窗口被截断。一次 HTTP 请求、新建 WS 连接、WS response.create/prewarm 分别计一次，复用连接本身不计。新的客户端 WS turn 重置本轮预算，重试同一轮不会重置。现有各层重试规则仍可提前结束请求。原始客户端取消也会停止新的操作。
   上游 503 或明确容量错误触发默认 5 秒冷却，遵守更长的 Retry-After，并在等待时加入小幅随机延迟。同分组、模型 15 秒内有三个不同账号记录失败时启用短暂共享冷却；这只是保守的本地背压信号，不能证明模型全局过载。相同凭据的多个账号行会分别计数。冷却状态只保存在进程内，缓存有界；预算耗尽返回 503，容量队列满或超时返回 429。控制错误不会当作账号故障扣分。
3. **客户端身份对照**：默认继续现有规范化行为。可选择保留识别到的 Codex 客户端 UA，originator/version 从该 UA 配对，不盲信输入的组合。未知、过旧或过长 UA 回退到现有行为。账号和 key 的命名空间隔离仍生效。开启策略后普通 HTTP 转发补齐 session-id/thread-id/x-client-request-id 的允许列表，与已有隔离逻辑衔接。
4. **容量排队基线**：只对 capacity 模式生效。inherit 保留原容量策略，subagent 允许原有回退，queue 禁止新建合成子会话；已有映射继续其生命周期。先找其他授权可用账号，候选根容量全部不足后才有界等待，每秒重查，不承诺 FIFO。默认等 10 秒，最多 60 秒，并受重试窗口约束；0 为不等待。全进程最多 128 个容量等待者，不持有被拒账号的并发槽。严格 previous_response_id 归属和原生子会话规则不变。
5. **Attestation 诊断**：只输出缺失、信封状态或格式异常等分类。token_returned 只表示信封中有 token，不表示上游已验证。不会生成、复用或转发该 token。

## 配置和边界

配置保存为 `account.extra.codex_request_policy`：

```json
{"enabled":true,"max_attempts":6,"retry_window_seconds":30,"cooldown_seconds":5,"identity_mode":"canonical","capacity_mode":"inherit","capacity_wait_seconds":10}
```

数值读取时限制在 UI 展示范围内。关闭 enabled 保留参数；批量编辑必须显式选择修改该策略。预算在一次请求中采用首个已启用策略的快照；已有 WS 连接不会因修改参数立即切换预算。身份及容量策略随账号快照刷新，新请求使用更新配置。

本地传输计数不包括外部插件进程或下一级代理内部的重试。如果串接 CPA 等上游，应统一控制重试，否则一次本地操作可能产生多个真实上游请求。后台连接预热没有客户端请求上下文，不计入该请求预算。

这些功能用于限制重试放大、观察协议差异和建立容量对照，不能提高账号配额，也不能保证消除限流、过载或封控。短暂真实请求成功只能证明该次请求可用。测试结果应同时观察最终成功率、实际尝试数、换号次数和耗时。

当前生产入口仍运行 capacity.2。本变更需要先完成本地验证及隔离对照，不能通过官方在线升级或直接重建旧镜像部署。

## 2026-09-10 夜间排查：非 Responses 入口

生产日志确认大量 Chat Completions 选号失败的内部原因是 `session_capacity_requires_responses`，旧入口将其泛化为 503。本补丁将这个明确的协议不兼容分类为 400 并说明需 Responses 或兼容账号，避免把它误报成可重试的上游过载。此修复不增加 Chat Completions 对容量模式的支持。

如果同模型当前可选账号均为 capacity，Chat Completions 需要改用 Responses，或由管理员提供至少一个该分组/模型可用的非 capacity 账号；普通设备收敛可以作为兼容对照。缺少稳定 session/thread 或 prompt_cache_key 的 Responses 请求仍会返回 `session_identity_required`；不能通过每次随机造 session 来掩盖客户端身份缺失。

## 2026-09-11：会话身份与重试边界

`session_identity_required` 是本地准入的 400；容量校验失败的候选没有发送上游请求。错误消息现在说明客户端需提供稳定会话身份，400 不再附带 HTTP `Retry-After` 或 WS `retry_after`。原有 429/409/503 的等待提示保留。

客户端可在每段对话开始时生成一个 UUID，通过 `session-id` 请求头发送，同一对话后续轮次和重试复用它。`thread-id` 缺省时继承 session；只发送 `thread-id` 不够。也支持原有的 `client_metadata.session_id`、turn metadata 的 `session_id`、`session_id` 请求头及 `prompt_cache_key` 回退。不同对话应使用不同身份，不能使用每次请求的随机 ID 或整个 API key 共用的常量。已提供上述身份仍报错时，需要管理员检查网关入口透传和内部身份解析。

设备收敛不会自动生成客户端会话身份。缺少身份时，调度器仍可选择该分组/模型授权且可用的非 capacity 兼容账号；不会自动关闭某个 capacity 账号的容量限制。

原有换号逻辑是失败后依次尝试账号，各层重试有自己的上限。统一预算限制实际传输操作，不限制本地候选扫描次数。回归测试从真实 Responses handler 进入：模拟账号 1 上游返回 520，预算为 1 时账号 2 不会收到请求，即使账号 2 关闭策略或将预算提高至 20。关闭新策略的对照仍可尝试账号 1、2。客户端重新发起请求仍会获得独立预算。
