## [0.2.4-session-capacity.2-chat.1] - 2026-09-11
### Fixes
- 将 Chat Completions 接入容量准入和现有 Responses 转换链，修复 capacity 账号在选取阶段返回 `session_capacity_requires_responses`、对外显示通用 503 的问题；客户端仍接收 Chat Completions JSON/SSE。
- 容量分配后的 session/thread 不再被兼容缓存注入覆盖；请求头与请求体使用一致的设备标识。容量错误保留 400/409/429/503，400 不提示重试。
- 保留原生子会话元数据与请求头，在 Chat 响应提交时登记 turn-state 所属账号，防止换号后延迟回传的旧状态发给其他账号。
### Design Rationale
- 基于线上 `0.2.4-session-capacity.2` 单独修复，复用已有双向协议转换，不恢复已回退的请求预算策略。
- 有显式会话标识时沿用；普通 Chat 请求缺少标识时每个入站请求分配独立身份，同一次内部重试复用，不通过相同提示词推断同一对话。
### Notes & Caveats
- 无会话标识的多轮调用不能自动复用同一容量绑定，可能更快耗尽容量或触发子会话额度；建议客户端传稳定且按对话独立的 `session-id`。容量和子会话上限不变。
- 原生 Responses 仍要求会话身份；HTTP `previous_response_id` 仍明确拒绝，不删除续接状态重放。已于 2026-09-11 部署；源码提交 `5511411f0`。

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
