## [0.2.5-session-capacity.4] - 2026-09-16
### Features
- 合并上游 v0.2.5（`881f32026`）：抢占旁路移入 `BeginOpenAIWSIngressSessionPreemptionWithClient` 并保留上游 `*openAIWSSessionPreemptState` 断言；WS 会话状态键改为「上游执行作用域 + capacity 凭据命名空间」的组合键，兼顾并发 subagent 隔离与换号不复用旧连接状态。
- 把 capacity 模式拆成身份投影层与根会话准入层。选择 `capacity` 即启用投影；准入需显式配置 `max_root_sessions > 0`，默认 `0`（对齐 llm-access 默认值）。
- 准入决策落结构化日志：`codex_capacity_rejected` / `mounted` / `plain_fallback` / `failover` / `selection_failed`，含根占用、上限、绑定数、窗口内新建子会话数与准入开关状态。
### Fixes
- 账号亲和恢复为偏好：capacity 候选全部不可用时允许退回普通账号，只有带 `previous_response_id` 的严格续接仍绑定真实生成响应的账号。此前一个 thread 只要 24 小时内被 capacity 账号服务过，就再也不能落到普通账号，容量耗尽即硬失败。
- 上限只约束新根暴露：已曝光的既有会话继续说话不再受限；空闲超过窗口的长连接让出根位（未发送的租约仍立即占位）。此前 WS 租约活到连接关闭且无条件占位，空闲会话会永久占死根槽位。
- 根计数与宿主资格统一到 `(凭据, api key)` 维度。此前计数是凭据级、宿主筛选是 key 级，一个 key 用满上限会让其它 key 既拿不到新根也找不到宿主，返回无解的 429。
- turn-state 归属未知时按外来处理剥掉（重启、绑定过期、客户端换线程）。此前会被认成当前账号所有并原样转发，重启后必然触发上游会话链鉴权失败。
- WS 增量帧一致性只比对帧内显式声明的 session/thread；`prompt_cache_key` 与请求头回退不参与，避免正常连接被 409 关闭。
- 会话标识识别对齐上游规范列表（`conversation_id`、opencode、codeBuddy 等），Responses 与 Chat 两条路径一致；此前 Responses 只认两个头，粘性路由认得的客户端在准入处被判成无标识。
- 绑定表达到上界改为淘汰最旧空闲绑定，合成身份释放即回收、保留 1 小时；此前每个无标识请求占用一条 24 小时绑定，1 万条后硬拒到重启。
- 根线程首次写入即固定，同根兄弟线程不再覆盖它导致合成子会话 parent 指错。
- 429 的 `Retry-After` 改为「最早有根位空出」的实际秒数，不再统一回整窗口；400 不带 `Retry-After` 并给出可执行提示（`ClientMessage`），`session_capacity_requires_responses` 在其它入口不再被包装成可重试的 503。
### Design Rationale
- 投影层保证标识不跨账号泄漏，有明确价值；根上限与合成子会话挂载对上游计数的假设无任何证据（llm-access 原文档同样不承诺），因此默认关闭，出问题先关它而不是关整个模式。
- 本地策略不应产生客户端可见的失败：能换号就换号，能退回普通账号就退回，只在客户端确实需要改动时（缺稳定标识、HTTP 续接）返回 400 并说明原因。
- 跨 key 共享根会共享 prompt cache key 与上下文，隔离优先于上限精度：宁可让上限变成 key 维度，也不跨租户挂载。
### Notes & Caveats
- 部署后行为不变：没有账号处于 `capacity` 模式时完全等价于上游 0.2.5；已在 `capacity` 模式但未配 `max_root_sessions` 的账号变为只做投影，不再有根上限。
- 多 key 共享凭据时总暴露量为 key 数 × `max_root_sessions`，需要按 key 数下调。
- 曝光台账仍是进程内状态，重启清空：重启瞬间准入层认为所有根都是新的，要真正保证上限必须持久化，本版未做。
- 准入开启时无标识客户端会收到 400 `session_identity_required`（该账号被跳过，池内其它账号不受影响）；这是有意的契约变更，替代原先"先占一个根、下一个请求 429"的行为。
- 未合入 `maint/session-capacity` 的请求预算/队列控制（capacity.3，线上曾因 `codex_retry_budget_exhausted` 回退），仅取其中的错误整形修复。
- 验证：`go build ./...`、`go test ./...`、`go test -tags unit ./internal/handler ./internal/service`（含容量回归 `-race`）、前端 `check:i18n` + `vue-tsc --noEmit` + `vite build` + 账号组件 vitest 全部通过。未做真实账号 smoke test。

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
