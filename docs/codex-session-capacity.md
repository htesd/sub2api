# Codex session capacity（实验）

基于 v0.2.4，移植 llm-access 434a0a2 的滚动根 session 上限和同 key 子会话挂载策略。

账号编辑的「Codex 指纹收敛」选择 `capacity` 开启；默认关闭。它保留设备收敛，由容量控制独占 session/thread 的最终改写，不能同时使用旧 session/full 收敛。

默认滚动窗口 600 秒，最多 5 个根 session；先尝试常规调度全部健康账号，再考虑 overflow。只有同一 API key 的已发送根 session 可作宿主，合成子会话保持独立 thread，parent 指向宿主实际 thread。每根最多 8 个窗口内活跃或持有租约的合成子会话，每账号每窗口最多新建 32 个合成子会话。原生 subagent 继承父 session，不占合成子会话额度。未发送释放预留；发送失败也保留曝光计数；活跃请求固定根 session。

支持 HTTP Responses/SSE、compact 和 WS Responses。capacity 账号的 WS 使用现有 ctx_pool 逐帧规范化路径；账号配置为 off 时仍拒绝 WS；插件强制 HTTP bridge 或超大首帧需要 HTTP bridge 时明确拒绝，避免该路径删除续接状态。Messages、图片独立端点不会调度到 capacity 账号。原生 Responses 请求必须有稳定 session/thread（允许由 prompt_cache_key 推导）。请求内容、工具参数、加密内容不由容量策略改写。

`0.2.4-session-capacity.2-chat.1`（`fix/capacity-chat-compat`）补丁增加 Chat Completions 支持：入站 `/v1/chat/completions` 先做容量准入，再通过已有转换器请求上游 Responses，返回时转换为 Chat Completions JSON/SSE，客户端不必切换协议。推荐发送按对话独立、后续轮次稳定的 `session-id`；也支持已有会话请求头别名、`client_metadata` 和显式 `prompt_cache_key`。普通请求没有标识时按每个入站请求生成独立身份，同次内部重试复用。不会将相同提示词合并为同一会话，因此无标识客户端可能更快用满容量，且缓存复用会受影响。原生 subagent 和 `previous_response_id` 续接不生成替代身份，HTTP 续接仍拒绝。该分支基于 capacity.2，不包含已回退的请求预算策略；2026-09-11 已上线。

所有客户端标识按实际凭据 namespace + API key 隔离。默认 prompt cache 跟根 session；显式独立 cache key 保留独立命名空间。响应元数据还原客户端标识，并登记 response 所属 key/thread/account。WS 新模式禁止自动删除 previous_response_id 重放；未知/跨线程的续接返回冲突。OAuth HTTP 上游不支持 previous_response_id，容量准入明确返回 400（session_continuation_requires_websocket）；HTTP 多轮请发送完整历史。

## 边界

- 换号时剥离已知属于旧账号的 `x-codex-turn-state`（头与 body 元数据）；仅保留有界哈希归属缓存，不保存原始 token。WS 缓存的 turn-state 和连接绑定按实际凭据隔离。

- 从 `0.2.4-session-capacity.2` 起，普通请求（无 `previous_response_id`）的账号归属为优先选择：原账号无法使用时可在当前授权候选中换到其他 capacity 账号，按目标账号重新分配标识。旧账号的曝光计数和响应归属保留；严格续接仍只使用实际生成响应的账号。非 capacity 账号不参与这种迁移。
- 正在使用的映射根仍占用所属账号的容量；普通完整历史请求可以在其他账号独立建立映射，互不共享 response 状态。WS 首轮失败可故障转移；多轮连接失败需要外层换号时，以 `session_retry_requires_full_history` 关闭连接，避免重放错误的首轮内容。

- 单进程内存状态，保留 24 小时；重启清空。多实例不能共同保证上限。相同上游凭据的多个本地账号行必须统一启用和配置；不要混用新旧模式。
- 每凭据最多 10000 个保留绑定，每绑定最多 256 个 response ID，过旧或淘汰的续接返回 409。显式容量不足返回 429/Retry-After，配置非法返回 503。WS 已升级连接使用错误事件/关闭码。
- 改变既有账号的身份模式会改变上游标识。先从未使用的测试账号启用；现有对话继续使用原模式，不能保证跨模式延续旧 response ID。
- 这是代理侧 session 组织策略，没有证据保证上游把合成子会话计为更少配额或降低封控。测试验证协议与隔离，不证明封控效果。
- 可选 `extra.codex_session_capacity` 覆盖五个默认参数：`max_root_sessions`, `window_seconds`, `subagent_fallback_enabled`, `max_children_per_root`, `max_new_children_per_window`。数值须为正；停用请切换模式。

## 验证与部署

Go 回归覆盖并发准入、未发送回滚、发送曝光、窗口、子会话上限、真实父 thread、跨 key 隔离、续接归属、元数据还原、typed turn 引用、设备兼容和调度先普通后 overflow。前端构建包含 i18n 完整性与类型检查。真实账号仅做小请求 smoke test。

Chat 兼容补丁的 handler 回归带 `unit` build tag，在 `backend/` 运行 `go test -tags unit ./internal/handler ./internal/service -run 'Test(ChatCapacity|CodexCapacity)' -count=1`。覆盖 JSON/SSE、工具历史与图片、原生及合成子会话、无标识请求重试与换号、容量错误，以及换号后的 turn-state 延迟回传。全后端检查另运行 `go test ./...`；模拟上游验证不代表真实账号可用性。

部署新镜像前备份当前运行二进制、容器配置和数据库。原容器保留作为回滚。先验证候选实例，再将新连接导向候选；已建立的流留在原实例完成。切换和重启后必须检查实际版本及健康状态，不能依赖旧镜像标签。

## 维护分支

- 仓库：`htesd/sub2api`，长期维护分支：`maint/session-capacity`。
- 上游：`Wei-Shaw/sub2api`；初始基线为 v0.2.4，提交 `98d86915becae9fe9491a91ffc6defd5235c8d2b`。
- 首次功能提交：`3d04e8fb0`；对应自定义部署版本为 `0.2.4-session-capacity.1`。
- 后续先在维护分支合并选定的上游版本，重点检查调度准入、HTTP/WS 标识改写、续接归属和账号设置兼容性。运行后端完整测试、容量测试的 race 检查及前端构建，再制作带独立版本号的新镜像并验证切换。
- 官方在线升级会替换程序并覆盖本功能；维护版本应通过本分支构建、验证和部署。账号凭据、数据库备份及生产配置不进入仓库。

## Chat 兼容补丁发布记录（2026-09-11）

- 运行版本 `0.2.4-session-capacity.2-chat.1`，源码 `5511411f0`，基线 `8c7f5cd49`。
- 最终源码的后端全量测试、带 unit tag 的 Chat/容量回归通过。前端 i18n、类型检查及生产构建通过；已有 embed 测试依赖不存在的 logo.png，使用临时 PNG fixture 验证后移除，实际发布资源另在候选实例验证。
- 候选实例使用隔离数据库和 Redis 验证健康、页面及真实 SVG；切换后真实 gpt-5.5 的 Chat JSON、Chat SSE、Responses SSE 均成功完成。
- 保留 capacity.2 容器用于回滚；未调整账号身份/容量参数，未启用已回退的请求预算，未重启数据库或 Redis。切换替换 API 进程并清空进程内会话状态。
