## [0.2.5-session-capacity-antistorm.1] - 2026-09-16
### Features
- OAuth 429 同号重试改为有界预算：`SameAccountRetryMax = 3`，退避 1s→2s→4s（上限 8s）并叠加正向抖动。此前 2 分钟窗口内不受次数限制，单个请求能对同一个已限流账号连打十几次。
- 上游 `Retry-After` 不再被截断到 8 秒。等待塞不进剩余窗口的重试由 handler 侧直接拒绝，而不是先睡一个被腰斩的时长再打。
- 新增 429 处置分类 `openAIOAuth429RetryAfter`：`Retry-After` 超过 `retryAfterSameAccountWaitBudget`（5s）时按它冷却账号并换号，不再当瞬时抖动做同号重试。
- 全平台补齐 `Retry-After` → 账号级冷却：响应体没给重置时间但上游给了 `Retry-After` 时按它冷却，不再落到秒级兜底猜测（`429_retry_after`）。
- 账号级限流写入改为单调延长：`SetRateLimited` 委托 `SetRateLimitedIfLater`，只有显式恢复能清除。
- 新增 `gateway.openai_oauth_concurrency_ceiling`（默认 16，0 = 关闭）：OpenAI OAuth 账号的上游并发天花板，同时兜住"未配置并发"和"配得过大"两种账号。
### Fixes
- 一次成功的账号测试不再自动清除 rate-limit / overload / temp-unsched / model-rate-limit。探测走最小请求，与真实流量的模型、上下文长度、并发都不同，它只能说明"这一个请求通过了"。管理员仍可通过 `POST /accounts/:id/recover-state` 显式恢复。
- `Retry-After` 停车时长收口到 15 分钟（`retryAfterMaxAccountPark`）：此前完全信任响应头，一个 `Retry-After: 86400` 就能把整池账号停一天。
### Design Rationale
- 这批改动不减少封控本身，减少的是网关自己制造的重试放大。线上日志里 `pool_mode_same_account_retry` 4093 次、`overloaded` 670 次，同号无界重试 + 被截断的 `Retry-After` 是可验证的放大源；`.ban-investigation.md` 里"重试级联"同样排在成因第 3 位。
- 短 `Retry-After`（≤5s）保留同号重试，是对参考实现的有意偏离：无条件换号会丢掉 prompt cache 亲和，并把同一会话线程摊到更多账号上，反关联意义上更差。为 1 秒钟的等待放弃亲和不值得。因此短等待原地消化（退避不短于 `Retry-After`），只有长等待才停号换号。
- 并发天花板与 `codex_session_capacity` 的 `max_root_sessions` 是两个正交维度：前者限制"同时在飞的请求数"（连接池定容与调度负载权重），后者限制"对上游暴露的根会话数"（身份投影）。默认隔离是 `account_proxy`，`Concurrency<=0` 等于不限制 `maxConnsPerHost`，因此"未配置"恰好是最危险的取值。
- 天花板只影响连接池尺寸与负载权重，不参与计费与路由；API Key 账号是用户自带额度、由上游按 key 计量，不在网关侧收敛。
### Notes & Caveats
- 唯一客户端可见的行为变化：长 `Retry-After` 的 429 现在更快换号或更快返回，不再出现"等 40 秒后成功"。作为交换，账号被按上游要求的时长停用而不是继续挨打。
- `SetRateLimited` 单调化影响所有平台的全部调用点。副作用是短窗口不能再覆盖长窗口——这是有意的，代价是误判的长冷却只能靠显式恢复清除。
- 天花板对存量账号立即生效（运行时计算，不写库、无迁移）。并发确实需要超过 16 的部署请显式调大 `gateway.openai_oauth_concurrency_ceiling`。
- 未移植参考实现的健康度自动隔离（10 分钟错误率 ≥50% → 停 30 分钟）：它需要新的错误率统计面，且与既有 `TempUnschedulable`、配额阈值停用、runtime block 三套机制的优先级需要先理清，本版不做。
- 未移植 TLS 指纹预设与强制 HTTP/1.1。uTLS dialer 与 `Profile` 上游本就有，参考实现只是加了两个写死的预设并默认挂给新账号；其 transport 显式关闭 HTTP/2，能消灭 h2 `INTERNAL_ERROR`，但一个自称 Node/Codex 的客户端只谈 `http/1.1` 本身就是可辨识特征，需要 A/B 才能判断净收益。
- 验证：`go build ./...`、`go test ./...`、`go test -tags unit ./...`、容量与 429 回归 `-race` 全部通过。`go test -tags unit ./internal/service` 中 `TestValidateCreateParams_CheckModeMatrix/quota_probe_requires_primary_model` 失败，需要 DNS，在干净基线上同样失败。`-race` 下 `gin.SetMode` 的 6 处 data race 同样在基线复现，属既有测试脚手架问题。前端未改动，未跑前端构建。未做真实账号 smoke test。

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
