# Codex session capacity（实验）

基于 v0.2.4，移植 llm-access 434a0a2 的滚动根 session 上限和同 key 子会话挂载策略。v0.2.5 起拆成两层，见下节。

账号编辑的「Codex 指纹收敛」选择 `capacity` 开启；默认关闭。它保留设备收敛，由容量控制独占 session/thread 的最终改写，不能同时使用旧 session/full 收敛。

## 两层结构（0.2.5-session-capacity.4 起）

**身份投影层**：选择 `capacity` 模式即生效。所有 session/thread/turn/window/cache 标识按 `(实际凭据 namespace, api key, 客户端原值)` 做确定性映射，turn-state 按哈希记归属，response 记发出账号。这一层只保证"标识不跨账号泄漏"，不限制任何数量，因此不会因为本地策略拒绝请求。

**根会话准入层**：只有显式配置 `extra.codex_session_capacity.max_root_sessions > 0` 才启用（默认 `0` = 关闭，对齐 llm-access 的默认值）。它限制窗口内**新根会话的暴露数量**，并在超限时把新对话挂成合成子会话。

上游是否对合成子会话按更少配额计数没有任何证据；这一层是未验证的策略，出问题优先关掉它（删掉 `max_root_sessions` 即可），投影层可以继续用。

准入层的行为约定：

- 上限只约束**新根暴露**。已曝光的既有会话继续说话不受限制，不会在第二轮被自己的上限打成 429。
- 活跃租约在窗口内占位；尚未发送的租约立即占位（避免并发双开新根）；空闲超过窗口的长连接让出根位。
- 根计数与宿主资格都是 `(凭据, api key)` 维度。多个 api key 共享同一凭据时，总暴露量是 key 数 × `max_root_sessions`，需要按 key 数下调该值。跨 key 挂载会共享根 session 与 prompt cache key，明确不做。
- 账号亲和是**偏好而非排他约束**：capacity 候选全部不可用时允许退回普通账号（日志 `codex_capacity_plain_fallback`），只有带 `previous_response_id` 的严格续接必须留在真正生成该响应的账号上。
- 准入开启时客户端必须提供稳定会话标识，无标识请求返回 400 `session_identity_required` 并给出接入提示；该账号会被跳过，池里还有其它账号时不影响调度。准入关闭时无标识请求按每个入站请求合成独立身份，用完即回收。
- 429 的 `Retry-After` 是"最早有根位空出"的实际秒数，不再统一回一个整窗口。400 不带 `Retry-After`。
- 绑定表上界（每凭据 10000）是内存保护而不是策略上限：达到上界淘汰最旧的空闲绑定，不会硬拒到重启。

准入开启后：先尝试常规调度全部健康账号，再考虑 overflow。只有同一 API key 的已发送根 session 可作宿主，合成子会话保持独立 thread，parent 指向宿主实际 thread。每根最多 8 个窗口内活跃或持有租约的合成子会话，每账号每窗口最多新建 32 个合成子会话。原生 subagent 继承父 session，不占合成子会话额度。未发送释放预留；发送失败也保留曝光计数；活跃请求固定根 session。

支持 HTTP Responses/SSE、compact 和 WS Responses。capacity 账号的 WS 使用现有 ctx_pool 逐帧规范化路径；账号配置为 off 时仍拒绝 WS；插件强制 HTTP bridge 或超大首帧需要 HTTP bridge 时明确拒绝，避免该路径删除续接状态。Messages、图片独立端点不会调度到 capacity 账号。原生 Responses 请求必须有稳定 session/thread（允许由 prompt_cache_key 推导）。请求内容、工具参数、加密内容不由容量策略改写。

Chat Completions 支持：入站 `/v1/chat/completions` 先做容量准入，再通过已有转换器请求上游 Responses，返回时转换为 Chat Completions JSON/SSE，客户端不必切换协议。会话标识按上游规范列表识别（`session-id`/`session_id`/`conversation_id`/opencode/codeBuddy 请求头、`client_metadata`、显式 `prompt_cache_key`），Responses 与 Chat 两条路径一致。无标识请求按每个入站请求合成独立身份（`s2a_req_*`，标记为 ephemeral），同次内部重试复用，不会把相同提示词合并成同一会话；准入开启时这类请求改为要求客户端补稳定标识。原生 subagent 和 `previous_response_id` 续接不生成替代身份，HTTP 续接仍拒绝。

所有客户端标识按实际凭据 namespace + API key 隔离。默认 prompt cache 跟根 session；显式独立 cache key 保留独立命名空间。响应元数据还原客户端标识，并登记 response 所属 key/thread/account。WS 新模式禁止自动删除 previous_response_id 重放；未知/跨线程的续接返回冲突。OAuth HTTP 上游不支持 previous_response_id，容量准入明确返回 400（session_continuation_requires_websocket）；HTTP 多轮请发送完整历史。

## 边界

- 换号时剥离已知属于旧账号的 `x-codex-turn-state`（头与 body 元数据）；仅保留有界哈希归属缓存，不保存原始 token。WS 缓存的 turn-state 和连接绑定按实际凭据隔离。**归属未知时按外来处理剥掉**（进程重启清空注册表、绑定过期、客户端换线程都属于这种情况）：把别的账号签发的 token 发给当前账号会直接触发上游会话链鉴权失败，剥掉最多损失一次前缀缓存命中。上游真的签发过之后该 token 才归本账号。
- WS 增量帧的一致性判断只看帧内**显式声明**的 `client_metadata`/turn metadata 里的 session/thread；`prompt_cache_key` 与请求头回退不参与比较，避免首帧身份来自请求头、后续帧只带 cache key 时被 409 关掉连接。合成身份的连接跳过该比较。
- WS 会话状态键是组合键：先取上游 0.2.5 的执行作用域（线程/分道，保证并发 subagent 不互相取消），再套 capacity 凭据命名空间（保证换号不复用旧账号的连接状态和 turn state）。

- 从 `0.2.4-session-capacity.2` 起，普通请求（无 `previous_response_id`）的账号归属为优先选择：原账号无法使用时可在当前授权候选中换到其他 capacity 账号，按目标账号重新分配标识。旧账号的曝光计数和响应归属保留；严格续接仍只使用实际生成响应的账号。非 capacity 账号不参与这种迁移。
- 正在使用的映射根仍占用所属账号的容量；普通完整历史请求可以在其他账号独立建立映射，互不共享 response 状态。WS 首轮失败可故障转移；多轮连接失败需要外层换号时，以 `session_retry_requires_full_history` 关闭连接，避免重放错误的首轮内容。

- 单进程内存状态，保留 24 小时（合成身份 1 小时，释放即回收）；重启清空。**重启会清掉曝光台账**，也就是重启瞬间准入层认为所有根都是新的，这是当前实现的已知缺口，需要持久化才能真正保证上限。多实例不能共同保证上限。相同上游凭据的多个本地账号行必须统一启用和配置；不要混用新旧模式。
- 每凭据最多 10000 个保留绑定（达到上界淘汰最旧的空闲绑定），每绑定最多 256 个 response ID，过旧或淘汰的续接返回 409。显式容量不足返回 429/Retry-After，配置非法返回 503。WS 已升级连接使用错误事件/关闭码。
- 改变既有账号的身份模式会改变上游标识。先从未使用的测试账号启用；现有对话继续使用原模式，不能保证跨模式延续旧 response ID。
- 这是代理侧 session 组织策略，没有证据保证上游把合成子会话计为更少配额或降低封控。测试验证协议与隔离，不证明封控效果。
- `extra.codex_session_capacity` 覆盖五个参数：`max_root_sessions`（默认 `0` = 只做身份投影）、`window_seconds`（600）、`subagent_fallback_enabled`（true）、`max_children_per_root`（8）、`max_new_children_per_window`（32）。除 `max_root_sessions` 可为 0 外数值须为正；只停准入把 `max_root_sessions` 删掉或设 0，连投影一起停用请切换指纹模式。
- 准入决策落日志（`slog`）：`codex_capacity_rejected`（错误码、状态、pass、overflow、根占用/上限、绑定数、窗口内新建子会话数、准入是否开启、是否合成身份）、`codex_capacity_mounted`、`codex_capacity_plain_fallback`、`codex_capacity_failover`、`codex_capacity_selection_failed`。排障先看这几条，不要靠客户端错误码猜。

## 验证与部署

Go 回归覆盖并发准入、未发送回滚、发送曝光、窗口、子会话上限、真实父 thread、跨 key 隔离、续接归属、元数据还原、typed turn 引用、设备兼容和调度先普通后 overflow。前端构建包含 i18n 完整性与类型检查。真实账号仅做小请求 smoke test。

Chat 兼容补丁的 handler 回归带 `unit` build tag，在 `backend/` 运行 `go test -tags unit ./internal/handler ./internal/service -run 'Test(ChatCapacity|CodexCapacity)' -count=1`。覆盖 JSON/SSE、工具历史与图片、原生及合成子会话、无标识请求重试与换号、容量错误，以及换号后的 turn-state 延迟回传。全后端检查另运行 `go test ./...`；模拟上游验证不代表真实账号可用性。

部署新镜像前备份当前运行二进制、容器配置和数据库。原容器保留作为回滚。先验证候选实例，再将新连接导向候选；已建立的流留在原实例完成。切换和重启后必须检查实际版本及健康状态，不能依赖旧镜像标签。

## 维护分支

- 仓库：`htesd/sub2api`，当前维护分支：`maint/session-capacity-025`（基线 v0.2.5，提交 `881f32026`）。上一代分支：`maint/session-capacity`（capacity.3，含已回退的请求预算控制，未合入本分支）、`fix/capacity-chat-compat`（capacity.2 + Chat 兼容，已合入）。
- 上游：`Wei-Shaw/sub2api`；初始基线为 v0.2.4，提交 `98d86915becae9fe9491a91ffc6defd5235c8d2b`。
- 首次功能提交：`3d04e8fb0`；对应自定义部署版本为 `0.2.4-session-capacity.1`。
- 后续先在维护分支合并选定的上游版本，重点检查调度准入、HTTP/WS 标识改写、续接归属和账号设置兼容性。运行后端完整测试、容量测试的 race 检查及前端构建，再制作带独立版本号的新镜像并验证切换。
- 官方在线升级会替换程序并覆盖本功能；维护版本应通过本分支构建、验证和部署。账号凭据、数据库备份及生产配置不进入仓库。

## Chat 兼容补丁发布记录（2026-09-11）

- 运行版本 `0.2.4-session-capacity.2-chat.1`，源码 `5511411f0`，基线 `8c7f5cd49`。
- 最终源码的后端全量测试、带 unit tag 的 Chat/容量回归通过。前端 i18n、类型检查及生产构建通过；已有 embed 测试依赖不存在的 logo.png，使用临时 PNG fixture 验证后移除，实际发布资源另在候选实例验证。
- 候选实例使用隔离数据库和 Redis 验证健康、页面及真实 SVG；切换后真实 gpt-5.5 的 Chat JSON、Chat SSE、Responses SSE 均成功完成。
- 保留 capacity.2 容器用于回滚；未调整账号身份/容量参数，未启用已回退的请求预算，未重启数据库或 Redis。切换替换 API 进程并清空进程内会话状态。
