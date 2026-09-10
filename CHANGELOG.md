## [0.2.4-session-capacity.3] - 2026-09-11
### Features
- 新增默认关闭的 Codex 请求诊断、共享重试预算与过载冷却、客户端身份对照、容量排队基线，以及 attestation 状态诊断。
- 创建、编辑、批量编辑支持保存独立策略，开启策略后普通 HTTP 转发补齐现代 Codex 会话头的隔离传递。
### Fixes
- Chat Completions 等入口保留容量模式需要 Responses 的明确 400，不再将协议不兼容包装为泛化 503。
- 为 `session_identity_required` 提供稳定会话身份的接入提示；容量准入 400 不再附带重试等待提示，保留其他状态的退避信息。补充 handler 级回归验证本地拒绝不触达上游、兼容账号可选，以及跨账号预算不重置。
### Design Rationale
- 先建立安全的可观测性，再限制重试放大和做单变量对照；不把代理层变化当作解除上游限制的保证。
### Notes & Caveats
- 仅 Responses 系列 OAuth 路径；默认关闭。状态为进程内，WS 每个新 turn 重置预算，外部插件内部重试不包含在计数中。
- 首轮生产试部署后，用户反馈大量 `codex_retry_budget_exhausted`，已回退 capacity.2 并关闭本次开启的控制。默认时间窗口对实际流量的影响仍需重新评估，不应依据短请求成功判断整体效果。配置和日志解释见 docs/codex-request-control.md。

## [0.2.4-session-capacity.2] - 2026-09-10
### Fixes
- 修复上游失败、冷却或停用后，普通完整历史请求被旧账号绑定阻止故障转移而返回 `session_owner_unavailable` 的问题。
- 保留历史曝光和 response 归属，优先复用原账号，无法使用时在授权候选范围内选择其他 capacity 账号。带 `previous_response_id` 的请求仍严格绑定响应生成账号。
- 保留每账号活跃根会话的容量保护，移除跨账号的普通会话排他锁；后续 WS turn 失败不再允许外层重放首轮内容。
### Design Rationale
- 按 llm-access 的设计区分普通 affinity 和响应续接归属，不能把响应记录的 24 小时保留期当成普通请求的排他账号锁。
### Notes & Caveats
- 不在本补丁中跨到非 capacity 模式；候选受原分组、模型、健康和权限规则约束。其他账号仍可能遇到上游 503 或容量不足。
- 活跃租约继续占用原账号的根容量，但不阻止独立完整历史请求换号。已进入多轮的 WS 出现需要外层换号的错误时关闭连接，要求客户端以完整历史重新建立请求。

## [0.2.4-session-capacity.1] - 2026-09-10
### Features
- 增加可选 Codex session 容量模式：滚动根 session 上限、同 API key 子会话挂载、独立 thread 与 response 归属检查。
- 保留设备收敛，HTTP/SSE、compact、WS ctx_pool 统一使用容量分配后的标识。
### Design Rationale
- 先沿用原调度器的健康、并发和计费约束，再做容量准入；正常账号优先于 overflow。
- 单实例原子预留，发送后记录曝光，未发送回滚。续接失败不删除 previous_response_id 重放。
### Notes & Caveats
- 默认关闭；初次在未使用账号上验证。内存状态重启清空，多实例及混用凭据别名不保证统一上限。
- 不支持 Messages/Chat Completions/独立图片端点；capacity 的 WS 使用 ctx_pool。
- 策略不能保证额度或封控结果。详见 docs/codex-session-capacity.md。
