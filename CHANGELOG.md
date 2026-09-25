## [0.2.8-session-capacity-antistorm.15] - 2026-09-25
### Features
- 新增账号级「Basispoints（Excel）上游」开关（账号编辑面板，OpenAI OAuth/Setup-Token 账号）。开启后，符合条件的请求改道 `bps.openai.com/basispoints/api/responses`（官方 ChatGPT Excel 加载项端点），不符合的仍在本账号走 Codex 上游。2026-09-25 用生产 team 与 pro 号实测：现有 OAuth access_token 直接可用，`gpt-6-astra` 真实返回 astra，无 safety-buffering 响应头、无降级路由。
- 改道按请求逐个判定资格：模型 ∈ {gpt-6-astra, gpt-5.6-sol/terra/luna}（含档位/日期后缀归一），未声明客户端 function/custom 工具，无图片输入，无结构化输出（text.format），无 service_tier；会话容量记账账号、spark 影子账号、compact 请求、WS 路径、API-Key 账号不参与改道。
- bps 请求体按白名单重建：`model_selection:"explicit"`（防止上游对模型 slug 做默认路由）、`store:false`、顶层 `reasoning_effort`（codex 的 max/ultra 档收敛到 xhigh，none/未知收敛到 medium）、`context_management` 默认 200K 压缩阈值、`metadata` 派生 task_id/turn_id/agent_iteration（内容寻址，同对话多轮与重试保持稳定）、`prompt_cache_key` 透传；`tools` / `previous_response_id` / `reasoning` 对象 / `service_tier` 等一律不带。
- 响应侧复用现有管道零改动：bps 总是返回 SSE（`stream:false` 也一样），非流式客户端由既有 `handleSSEToJSON` 折叠；usage 从 `response.completed` 正常提取计费；使用记录的 upstream_endpoint 标记为 `/basispoints/api/responses` 以区分真实出路。
- 前端账号编辑弹窗新增开关（en/zh 文案）；调度投影白名单保留 `openai_bps_excel_enabled`，保存即热更新。

### Design Rationale
- 线路形状依据 ghcp_proxy（github.com/Nonary/ghcp_proxy）`excel_upstream.py` 与当天实测探针（research/bps-prism-probe）：`model_selection:"explicit"` 是关键字段；`tools`、`previous_response_id` 会被 422；`gpt-6-sol/luna` 在 bps 上 403 `basispoints_model_access_changed`。
- 资格判定与改道放在 `buildUpstreamRequest`（responses / chat completions / messages 三条 OpenAI 路径的共同收口），一处改动覆盖三条路径；bps 请求跳过全部 codex 客户端身份链（originator / version / turn-state 门票 / 指纹收敛 / session 隔离 / 容量投影），与官方 Excel 加载项保持最小头集。
- 客户端工具不做提示词仿真、也不做 run_officejs 中继：带工具的请求留在 codex 上游，避免改写流式响应引入脆弱性。这是 v1 的明确边界，后续可按需迭代。
- 容量记账账号依赖 codex 上游的会话投影与回执做记账，bps 无法承载，直接排除而不是牺牲记账精度。
- prism.openai.com 路线（chatgpt-prism2api）同期验证不可行：prism 不接受 chatgpt OAuth token 当凭据（要独立浏览器登录换 `prism_oai_access_token`），且协议是 start+轮询、无原生工具通道，不适合本网关。

### Notes & Caveats
- bps 每个请求自带约 22k token 的服务端系统前缀（Excel persona + 工具目录），绝大部分命中上游缓存；usage 与计费按上游实际回报计入，input 会比同请求在 codex 上游高（缓存部分按缓存价）。
- bps 不使用 turn-state 门票：改道请求不注入、不采集；降智观察头（`X-Codex-Safety-Buffering-Faster-Model`）在 bps 上不存在，观察功能对其自然空转；降智阻断按响应模型判定，bps 实得模型与请求一致，不会误触发。
- 「不会 429」是网友说法，未做负载验证；bps 用量大概率同样计入订阅配额。
- `max_output_tokens` 等白名单外字段会被静默丢弃（不拦截、不报错），需要严格输出上限的请求不要走 bps 账号。
- 生产池内 plus 号 token 全部过期 41 天且无 refresh_token，本次未能验证 plus 档；team（business_prolite）与 pro 档已验证。
- 测试：新增 `openai_bps_excel_test.go`（开关解析、目录归一、effort 映射、资格判定、白名单重写、metadata 派生确定性、头部构造 7 个用例）；`go build ./...`、`go test ./internal/service/ ./internal/repository/` 通过；前端 vue-tsc 与 eslint 通过，vitest 333 个文件 2500 个用例全部通过。

## [0.2.8-session-capacity-antistorm.14] - 2026-09-24
### Features
- 新增「降智观察」开关（网关服务 → Codex 设置），和「降智阻断」分开：开观察只记录不阻断；开阻断一定同时观察。保存后 5 秒内热生效。
- 观察开启时，把上游响应头 `X-Codex-Safety-Buffering-Faster-Model` 的值写进使用记录新列 `usage_logs.upstream_faster_model`；后台使用记录页在模型列下显示「↳ 上游降级标记」，并可按「仅有标记 / 仅无标记」筛选（列表与统计接口参数 `upstream_downgrade_marked`）。
- 覆盖 Responses、passthrough、Chat Completions、Messages、WS→HTTP 桥五条 OpenAI 转发路径；换号重试时标记随新的转发尝试重置，记下的是最终成功那个账号的响应头。
### Design Rationale
- 用户要求先把这个响应头的数据记下来自行观察，再决定判据。09-23 实测新号老号都带这个头、值都是 gpt-5.6-luna，所以它目前不能单独当降级判据；按账号生成速度的判据留待看完数据后再做。
- 只观察不改任何转发、换号、冷却和计费行为；观察范围不受「降智阻断的模型名单」限制，所有模型都记。
### Notes & Caveats
- 新 migration `239x_add_usage_log_upstream_faster_model.sql`：可空、无默认值的新列，只改表定义不重写 usage_logs，不建索引（避免启动时扫大表）。按标记筛选依赖 created_at 范围缩小扫描量。
- 普通用户的使用记录不输出这个字段。

## [0.2.8-session-capacity-antistorm.13] - 2026-09-24
### Features
- 整体合并上游 v0.2.8（Wei-Shaw/sub2api，2026-09-23，本分支原基线为 09-15 的 0.2.5，落后 307 个提交）。对本站（只跑 OpenAI OAuth 号）有用的主要是：
  - 流式收到结束事件即结束，不再等上游断开连接；恢复 OpenAI HTTP/2 保活容错；关闭响应体前先取消响应尝试
  - 非高级调度下，续接会话的请求（带 previous_response_id）路由回原先处理它的账号
  - 临时不可调度状态只作用于当前账号；账号 RPM 配置保留在调度投影中
  - 清洗工具参数定义里的非法 null required、元组数组，减少上游 400
  - 流内失败按 error.status 分类；流式协议错误只发一次；剥离超长 input item ID
  - 计费：支持按推理力度配置倍率（取代原来只针对 max 档的倍率字段，239 号 migration 自动迁移旧配置）
- 其余上游新功能（OpenCode Go 用量窗口、Claude Code 版本号自动同步、月度备份归档、推广线下提现、Codex 积分与推荐邀请等）随合并一并带入，默认行为与上游一致。
### Design Rationale
- 选择整体合并而不是挑几个修复：冲突只有 25 个文件、44 处，最大一处 86 行；挑拣会让本分支和上游越离越远，下次合并更难。
- GPT-6 Sol/Luna：上游合并的正是本分支 .12 移植过的 PR #7509，逻辑相同。冲突处保留 .12 的三处改法——按规范名区分 Sol/Luna 的静态价卡、none 档同时认顶层 reasoning_effort、不在本地拦截 Chat-only 账号的"工具 + 推理"请求（上游 v0.2.8 会直接回 400，本分支删除该拦截并把对应测试改为"原样转发"）。
- 价卡 JSON 里两边各插了一份 gpt-6-sol/gpt-6-luna，git 没报冲突但留下了重复键，已删去一份（两份内容逐字相同）。
- 设置项冲突一律两边都保留：本分支的打票、降智阻断设置，加上上游新增的 Claude Code 版本号设置。账号更新 SQL 两边都加了列，合并后列顺序与读取顺序逐一对齐：本分支的 current_extra 在前，上游的三个 OpenCode Go 列在后。
- 上游去掉了 ModelPricing.MaxReasoningEffortMultiplier 和 Fable 5.1 专用的 max 档倍率，改为通用的按推理力度倍率；本分支跟随上游，删掉了冲突解决时带进来的那一行旧引用。
### Notes & Caveats
- 上线会新执行 3 个 migration（238b、239、240），都只改小表，不涉及 usage_logs。
- 测试：go test ./... 与 go test -tags=unit ./... 结果见提交说明；前端 vitest 333 个文件、2499 个用例全部通过（此前两个老失败已由上游修复），vue-tsc 与 eslint 通过。
- 降智观察（按账号生成速度判断）另在分支 wip/downgrade-observe 开发，不在本版本内。

## [0.2.5-session-capacity-antistorm.12] - 2026-09-23
### Features
- 支持 `gpt-6-sol` / `gpt-6-luna`：模型列表与显示名、客户端写法归一（含 `-high` 等档位后缀和 `-openai-compact`）、Codex 模型清单（推理档位 low–max、默认 medium、Fast 档、图片输入）、`effort=max` 与 `reasoning.mode` 原样透传、Messages 兼容路径的 `prompt_cache_key` 注入。
- 计费三处价源都补齐（billing 兜底价、pricing 静态价卡、LiteLLM 价卡 JSON），按官方价：Sol 输入 $2 / 缓存读 $0.2 / 缓存写 $2.5 / 输出 $10，Luna 为 $0.1 / $0.01 / $0.125 / $0.5（每百万 token）；超过 272K 输入整条请求输入与缓存 ×2、输出 ×1.5；Fast ×2、Flex ×0.5。
- 推理档位不是 none 时删掉 `temperature` / `top_p` / `logprobs`，与 GPT-5 系列一致（none 的判断同时认 `reasoning.effort` 和顶层兼容写法 `reasoning_effort`）；Chat Completions 与 Messages 两条兼容路径改用映射后的上游模型做转换。
- API-Key 账号给 Codex 的模型清单对 Sol/Luna 同样关闭 Responses Lite；前端模型白名单、快捷映射按钮与 OpenCode 配置导出加入两个新模型。
### Design Rationale
- 移植自上游未合并的 PR #7509（Wei-Shaw/sub2api），只取 GPT-6 部分。该 PR 同时带 Claude Opus 5.5 支持，但那部分要改 Claude 协议转换，而本分支落后上游 287 个提交、这一块差异大，没有带。
- 新增系列判断 `isOpenAIGPT6Model`（Astra 或 Sol/Luna），只替换语义上属于"GPT-6 系列通用能力"的调用点；Astra 专用的 1.05M 窗口、`ultra` 档、官方 Codex 0.153 能力默认值、陈旧快照修复仍只认 Astra。改动前后用同一个临时探针对比，`gpt-6-astra` / `gpt-6` / `gpt-5.6-sol` 的归一、档位、窗口、Fast、价格逐字不变；假想的 `gpt-6-nova` 仍不被识别，没有匹配过宽。
- 没有搬 PR 里"只支持 Chat Completions 的账号 + 工具 + 推理时本地直接返回 400"那段：官方文档写 GPT-6 支持 Chat Completions 且没说有这个限制，本地拦截如果判断错就是误伤，交给上游报错更稳。
- pricing 静态价卡的 Sol/Luna 分支用归一化后的名字判断，而不是 PR 里的 `HasPrefix("gpt-6-sol")`：后者遇到大小写或别名写法会让 Sol 落到 Luna 的价（便宜 20 倍）。
- `needsOpenAICodexAstraVersion` 原本就按 `Contains("gpt-6")` 匹配、会命中 Sol/Luna，这次确认是想要的行为（GPT-6 系列都只对新版 Codex 开放）并加注释固化。
### Notes & Caveats
- Codex 模型清单里 Sol/Luna 的上下文窗口沿用 5.6 的离线模板（272000 / 最大 872000），不是 API 文档的 1.05M；账号从上游拿到的真实清单会覆盖这里的默认值。
- Sol/Luna 的 base instructions 仍用默认的最新 Codex 提示词（与 gpt-5.6-sol 相同），不是 Astra 专用那份。
- 打票（ticket）、降智阻断名单、官方账号 Codex 0.153 能力默认值都仍只针对 Astra，没有扩到 Sol/Luna。
- 上线前用 codex 做了三视角审查：采纳"顶层 reasoning_effort=none 时误删采样参数"并修复；"Batch 档未打五折"不采纳，本网关没有 Batch 接口，OpenAI 的 service_tier 也没有 batch 取值。
- 测试：新增 `openai_gpt6_sampling_test.go`（7 个场景）和 `billing_gpt6_sol_luna_test.go`（三个价源 × 271999/272000/272001 × 四种服务档位、渠道显式 0 价、显式 0 缓存写入、别名与大小写不串价）；`go test ./...` 全过；`go test -tags=unit ./...` 有 4 个失败，前端 vitest 全量有 2 个失败，这 6 个在 5ea0fbb59 上就已失败，与本次改动无关。

## [0.2.5-session-capacity-antistorm.10] - 2026-09-21
### Features
- 新增降智阻断：上游把名单内模型（默认 gpt-6-astra）降级成更快的模型时，在任何响应体到达客户端之前判定，冷却该账号的该模型并换号；全部候选都被降级时给客户端可重试的 429 并说明原因。
- 三个可热更新设置项：`openai_codex_downgrade_block_enabled`（默认关闭）、`openai_codex_downgrade_block_minutes`（默认 30，1–1440）、`openai_codex_downgrade_block_models`（默认 gpt-6-astra），面板可改，保存即生效。
- 覆盖原生流式、非流式与 passthrough 流式三条路径；passthrough 的判定放在模型名改写之前，用上游原始模型名判断。
### Design Rationale
- 判据只能是「这一轮上游实际回了哪个模型」。当天实测：账号一旦进入 safety buffering，改请求形状（tools / reasoning effort / service_tier / originator，共 20 例）、换出口、注入合格票都无法恢复；而未进入该状态的账号用最简请求也能拿到目标模型。因此处置是冷却账号，不是重试或改写请求。
- 降级响应是 HTTP 200 + 完整 SSE + `response.completed`，既有失败判据全都发现不了它，客户端会静默收到降级结果——这正是必须在网关侧拦的原因。
- 判定点选在首输出暂存阶段（OpenAI 账号默认开启）：此时 `response.created` 尚未提交给客户端，可以安全换号，客户端全程无感；一旦语义字节已出站就不再换号。
- 冷却同时写进程内 transient 与持久化 `extra.model_rate_limits[模型]`，后者让重启与多实例都排除该账号，且只影响该模型。
- 换号错误必须是 error 而不是成功返回，否则调度器会在成功路径上把刚写的冷却清掉。
### Notes & Caveats
- 默认关闭；开启后每次命中都会消耗一次上游请求（降级响应已经产生），换号次数受既有 `MaxAccountSwitches`（默认 3）限制。
- 上游只在降级轮返回 `X-Codex-Safety-Buffering-Faster-Model`，该头只记日志，不单独作为判据。
- WS 路径（openai_ws_forwarder_v2 / ingress / http_bridge）与 `/v1/messages` 桥尚未接入同一判定。
- 单次降级不会立即触发进程内 transient 熔断（它按连续失败计数），生效冷却来自持久化限流。

## [0.2.5-session-capacity-antistorm.9] - 2026-09-21
### Features
- 修正自动迁移的判定顺序：无票时的首个请求只当作采票，不再用它的响应模型否决出口；能力证明改由「携刚采到的票在同一出口复验」提供。
- 原代理缺票时也先在原代理采票并复验，复验通过即留在原代理并落票（`current_proxy_ok`），只有原出口携票仍拿不到 Astra 才进入候选迁移。
### Design Rationale
- 线上实测：开启迁移的 4 个账号全部停在 `candidate_not_astra`。采票请求本身通常被上游降级（返回 luna），旧逻辑在采票阶段就要求 Astra，等于要求「先有票才可能拿票」，候选永远无法通过，迁移不可能发生。
- 判据仍是完整 SSE 的实际模型，只是把它挪到唯一有意义的位置——携票复验；因此「不以票长度或 HTTP 200 当能力证明」的约束没有放松。
- 优先原代理的语义因此才真正成立：先给原出口一次带票机会，避免把只是缺票的账号迁走造成无谓的出口抖动。
### Notes & Caveats
- 每轮上游请求上限从 3 次升到 4 次（原代理采票+复验、候选采票+复验）；地区判据挡下候选时只花前两次。
- `candidate_not_astra` 现在表示「候选携票复验拿到的仍是非 Astra 完整响应」，`candidate_ticket_invalid` 表示候选采票阶段没拿到合格票，前端结果码与文案不变。
- 上游若整体停止发放 Astra（例如手动停用该模型或账号无权限），所有出口都会判为不可用，此时不迁移是预期行为，不是故障。

## [0.2.5-session-capacity-antistorm.8] - 2026-09-21
### Features
- 基于已发布的 antistorm.6 增加账号级 GPT-6 自动迁移代理开关，默认关闭，保存后热更新；账号列表及编辑面板显示最近验证/迁移结果。
- 原代理 GPT-6 验证失败后，从打票池中选择已登记的同地区代理，取得合格票且完整返回 Astra 后携票复验，成功才同步切换账号代理与票据。
### Design Rationale
- 以完整 SSE 的实际模型为依据，不将票据长度或 HTTP 200 当成能力证明；仅由 GPT-6 决定整个账号出口，其他模型不单独触发迁移。
- 事务内检查原/目标代理版本、账号配置、授权及迁移代际，同步影子账号与调度缓存；旧出口迟到回票不得覆盖新票。沿用现有业务 session 伪装，探测也应用同一指纹收敛函数。
### Notes & Caveats
- 不包含已弃用 .7 或工作区内未发布的模型自动暂停功能。需同时开启票据、后台主动打票及账号主动打票；关闭迁移不自动切回旧代理。
- 候选必须同时存在于代理管理和打票池；地区未知或跨地区时不自动迁移。每轮最多3个出口预检、1个候选采票及1次携票复验，失败沿用退避，健康路径每5分钟检查一次。
- 地区/IP稳定性来自 ChatGPT trace 前后观测，代理服务商仍需保证固定出口；不能保证已建立的业务连接瞬时切换，后续调度使用新代理。真实流量效果由用户上线后验收。
- 自动迁移的成功出口至少驻留10分钟；迁移事件另写结构化运行日志。健康但无合格新票时保留原代理并退避补票，不以长度驱动迁移。
- 开启后GPT-6池票必须在候选出口完成真实模型验证和携票复验，未通过不迁移、不用异地票替代就绪状态；其他模型只在当前业务代理补票，避免多模型争抢出口。名单内缺票的既有拒发策略继续生效。
- 每个启用账号健康时最多约12次GPT-6短探测/小时，空闲账号也会补首票。只有默认关闭的账号级开关显式开启后才产生这些请求。

## [0.2.5-session-capacity-antistorm.6] - 2026-09-20
### Features
- 主动打票每轮优先使用账号当前绑定代理，成功即结束；失败后最多选一个不同的独立池代理兜底，整轮失败仅累计一次退避。账号代理无需独立打票池即可获取首票。
- 票据持久化记录采集路径，账号列表和编辑面板按模型展示「原代理获取」「其他代理获取」「业务采集」「来源未知」，原代理编号或配置更新时间改变后显示「旧代理配置」。其他/旧代理使用醒目标记。
### Design Rationale
- 无票拒发的模型没有业务流量，首票必须由主动采集提供；维持名单模型豁免活跃窗口过滤。优先账号原代理减少不必要的跨出口探测，独立池只作为有界兜底。
- 探测使用排队后重新读取的代理与凭据；兜底前再次确认授权和账号代理未变，并使用最新代理池；按传输语义去重代理 URL。401/403/429 不通过换代理追加请求。
### Notes & Caveats
- 来源只记录最近保存票据的采集路径，不保存代理凭证，也不是跨出口尝试历史。没有来源字段的历史票显示未知。代理配置相同不保证公网 IP 相同，来源标记不证明封号原因。
- 保留全局/账号主动探测开关及原有票规格校验；无绑定代理且无池时不新增直连。主动探测仍只读取响应头，未增加实际响应模型校验。

## [0.2.5-session-capacity-antistorm.5] - 2026-09-20
### Features
- 系统设置 → 网关 → Codex 新增后台主动打票开关和无票拒发模型名单，保存后热更新，无需重启。
- 名单精确匹配出站模型，支持逗号/换行与显式清空；部署名单仅包含 `gpt-6-astra`，sol 无票正常转发。
- 管理端与运行时统一解析数据库覆盖后的代理池、主动探测许可与名单。
### Design Rationale
- 常驻控制循环在关闭状态不发送探测，运行中可热开启；名单与主动探测解耦，关闭探测不自动解除业务限制。
- 名单模型加入采集范围并豁免活跃窗口过滤，避免无票导致无法变活跃的自举死锁；仍尊重账号主动探测授权及调度状态。
- 设置缓存保存时发布新快照，以代际校验防止旧查询覆盖新设置；查询失败保留上次成功策略，冷启动无法确认策略时禁止主动探测并保守拦截受控模型。
### Notes & Caveats
- 热更新传播缓存为 5 秒，探测下一周期使用新设置；已发出的请求可完成。未配置名单的旧实例保留旧策略，显式空名单才表示全部放行。
- 无 schema 变更。门票总开关和账号退出票据机制仍保留原语义；关闭它们会退出门票注入及门禁。

## [0.2.5-session-capacity-antistorm.4] - 2026-09-19
### Features
- 门票机制新增**账号级开关**：`extra.codex_ticket_enabled`（缺键=继承全局，`false`=既不采集也不注入）与 `extra.codex_ticket_active_probe`（默认关闭，逐号 opt-in）。三个管理表单（编辑/新建/批量）都已接入，账号列表与编辑弹窗展示 `account_disabled` 状态。
- 主动探测在**取得并发槽之后**以最新账号状态复核授权。排队等待可能长达数十秒（信号量 + 前序探测的 attempt_timeout），期间管理员关掉开关的操作原先会被在飞任务绕过。
- 授权复核为 **fail-closed**：查询出错、账号已删除（`ErrAccountNotFound`）、开关关闭、不可调度，四种情况各记一个 reason 并放弃本轮。

### Design Rationale
- **为什么账号级开关只有"关"是实质性的**：后端把显式 `true` 与缺键同等对待，注入与采集的判定都是全局开关 AND 账号开关，所以"开启"不会越过全局总闸——它与"继承全局"的唯一差别是把意图显式记录下来。文案必须写明这点，否则管理员会以为单开一个账号就能在全局关闭时继续跑。
- **为什么主动探测默认关、而被动采集默认开**：被动采集不产生任何额外上游请求，state 来自账号自己的业务回合、走自己的出口 IP，上游侧看不到新行为。主动探测则引入合成流量、消耗配额、并可能跨 ASN 跳变。两者的风险面不同，不该共用一个默认值。
- **为什么授权复核选 fail-closed**：这是"要不要发出这发合成请求"的授权检查，与业务侧 `fail_closed` 配置（决定客户端请求要不要失败）是两件事。查不到即无法确认授权仍然有效；放弃一发后台探测的代价极低，误发的代价是对一个可能已被保护、甚至已被删除的账号动手。
- **为什么批量编辑没有"继承全局"档**：批量接口是 JSONB 顶层合并（`extra = COALESCE(extra,'{}') || payload`），删键只表示"本次不更新"，清不掉账号上已有的值，因此"回到继承"在批量语义下无法表达。批量路径一律落显式 boolean 哨兵值，这是 #6327 的同一条教训。
- **为什么 `account_disabled` 与 `ready` 正交而非第四个状态**：账号退出后库存票依然存在，`ready` 如实反映库存事实，只是不会被注入。合成一个状态会让管理员分不清"没票"和"有票但停用"。

### Notes & Caveats
- 账号级 `codex_ticket_active_probe` 仍需配置文件的 `harvest_loop_enabled` 总闸，两者都成立才会真正发起探测。该总闸未进 admin settings API，面板上不可见，已在文案中写明。
- 批量关闭总开关会连带把 `codex_ticket_active_probe` 落成 `false`。否则残留的 `true` 会在日后重新开启总开关时让主动打票自己恢复。
- 主动探测路径仍只校验状态码/长度/前缀，**不校验响应模型**，降智响应也可能被当成合格票存下（既有缺陷，非本次引入）。探测把 body 立即关闭不读 SSE，无法就地取模型身份。逐号开启主动打票时需知晓这一点。
- 票据对降智的因果效果**仍未验证**。需受控实验：挑一个可调度账号开票，对比其自身 astra mismatch 率的前后变化。
- WSv2 路径没有 turn-state 暂存与提交点，被动采集对它零覆盖。

## [0.2.5-session-capacity-antistorm.3] - 2026-09-19
### Features
- 票据改为**被动采集为主路径**：从真实业务响应头捕获 `x-codex-turn-state`，仅在上游自报响应模型与出站模型一致（本回合未被降级）且无模型声明冲突时落库。零额外请求、走账号自己的业务出口 IP、state 来自一个已被证明未降级的真实回合。
- 长度判定从全局写死 292 改为**按账号套餐取值**：`pro=292`、`business/team=332`，支持 `extra.codex_ticket_plan`（pro|team）显式覆盖；未知套餐回落配置值。
- 后台主动探测循环默认关闭（`harvest_loop_enabled=false`）。采集器新增：尊重 `IsSchedulable()`、排除 free 套餐、活跃窗口过滤（`last_used_at`，默认 24h）、并发上限（默认 32）、按 (账号,模型) 隔离的指数退避（30s→900s）、代理池逗号分隔轮换与非合格长度烧号冷却（默认 600s）。
- 票据有效期锚定 Fernet 头部自述的签发时间（`raw[0]` 版本字节 + `raw[1:9]` 大端秒级时间戳），不再按本地捕获时刻起算。
- `fail_closed` 收口为**按账号判定**：只有被动采集 / 未配采集代理 / 账号 idle 或从未使用 / free 套餐——任一成立即放行，避免自举死锁。管理端 `Blocked` 展示复用同一谓词。
### Design Rationale
- 实测数据（111 个活跃 OAuth 账号，2026-09-18）推翻了上游三个假设。长度由套餐决定：Business 账号返回 200 且 Fernet 结构合法，只是长度天然 332，被写死的 292 判定丢弃，于是永远拿不到票——而它们恰是流量主力（单号 2770 请求 / 211M token）。
- 降智是可测的：`usage_logs.upstream_response_model` 显示近 24 小时 `gpt-6-astra` 有 **48.6%** 被换成 `gpt-5.6-luna`（14535/29929），`gpt-5.6-sol` 仅 0.1%。**长度只是筛选器，不能证明模型身份**——真正判据是响应模型。
- 主动探测用账号凭据从与业务出口不同的 IP（通常机房 IP）发合成请求，在上游看来同一账号周期性跨 ASN 跳变，本身即关联特征。这与反封控方向相反，故降级为兜底并默认关闭。
- `fail_closed` 的唯一成立前提是该账号确实会被一个可用票源探测到。前提不成立时它退化为自举死锁：没票→拒发→没有成功回合→`last_used_at` 不更新→永远拿不到票。降智可恢复，永久拒发不可。
- 配置归一化抽成导出纯函数 `NormalizeOpenAICodexTicketConfig`，运行时与展示层共用，避免「运行时放行而 UI 显示已暂停」这类分叉。
### Notes & Caveats
- **未证实**：持有票据能否真正降低降智率。7 个有票的 pro 号在观测窗口内零 astra 流量（13 个 pro 中 12 个被管理员停调度），passive 数据无重叠区。要证明需受控实验：挑一个可调度账号开票，对比其自身 astra mismatch 率变化。
- WSv2 路径没有 turn-state 暂存与提交点，被动采集对它零覆盖。本部署 115 个活跃 OpenAI OAuth 账号中 **112 个开了 WSv2**——这是 `fail_closed` 必须按账号放行的直接原因。
- free 套餐（64 个账号）被上游一律 400 拒绝，无 astra/sol 访问权限；另有 21 个账号 401（token 失效），属独立问题。
- 展示层的 `proxyConfigured` 只看 YAML 配置，看不到面板运行时设置的代理；代理只配在面板时 UI 会显示未阻断而运行时阻断（方向为宁可少报警）。
- 已知限制：谓词只能判「代理已配置」，无法判「代理可用」。配了连不通的代理时 `fail_closed` 仍生效。
- 未采纳的上游既有问题（非本次引入，需独立评估）：turn-state 明文存 `accounts.extra`、`storeOpenAICodexTicket` 先写内存后写库、探测 token 错误用 `zap.Error` 记录。
- 审查：经 kimi 与 gpt-5.6-sol 双向对抗审查共 6 轮，累计修复 11 项（含被动采集在流式主路径上完全失效、候选未绑定账号导致 failover 跨号污染、`lookupOpenAICodexTicket` 仍用全局 292、代理池在生产链路为死代码、门禁与落库 ctx 不同源、check-then-store 竞态、烧号误判好代理等），两侧最终均 APPROVE。
- 验证：`go build ./...`、`go vet ./...`、`go test ./...` 51 包全过零 FAIL。前端未改动。未做真实账号 smoke test。

## [0.2.5-session-capacity-antistorm.2] - 2026-09-18
### Features
- 从上游 v0.2.6 摘取 Codex 门票系列（6 提交，PR #7315）：ChatGPT OAuth 账号按 `(账号, 模型)` 采集 292 长度的 `x-codex-turn-state`，存入 `account.extra` 的 `codex_turn_ticket:<model>`，业务请求出站时覆盖该请求头。默认关闭，开启后热生效；采集代理可独立配置（`harvest_proxy_url`，支持账号级 `codex_harvest_proxy_url` 覆盖），返回时掩码。
- 一并摘取 v0.2.6 其余 openai/codex 修复：上游 chat role 兼容（新增 `openai_chat_roles.go`）、response binding context 修复、paused OAuth 账号刷新候选、Codex manifest 重复解析。
- 门票有效期改为锚定票据自述的签发时间：新增 `decodeOpenAICodexTicketIssuedAt` 解析 Fernet 头部（`raw[0]` 版本字节 `0x80`、`raw[1:9]` 大端 uint64 秒级时间戳），`ExpiresAt = IssuedAt + ttl`。票据结构新增 `IssuedAt` 字段，采集日志带 `issued_at` 与 `capture_lag`。
- 采集器新增并发上限 `harvest_concurrency`（默认 32）与失败退避 `harvest_backoff_base_seconds` / `harvest_backoff_max_seconds`（默认 30s 起步、翻倍、15 分钟封顶、叠加 20% 正向抖动），退避按 `(账号, 模型)` 隔离，打票成功立即解除。采集周期日志新增 `backoff_skipped` 与 `concurrency`。
### Design Rationale
- 门票的作用是反降智，不是解锁模型。`x-codex-turn-state` 是上游签发的 Fernet 回合凭证，长度本身就是状态指示器：292 = 干净窗口，312 = 已降智（多出正好一个 AES 块），后者伴随 `Our servers are currently overloaded`。所以采集的关键前提是出口 IP 的新鲜度，而不是请求量。
- 采集探测只取响应头：`req.Close = true` + 专用 no-reuse transport，拿到 header 立即 `resp.Body.Close()`，SSE 流一个字节不读。
- 有效期必须按签发时间算。上游原实现用 `capturedAt + 3600s`，把探测往返与排队延迟白送成额外寿命，票据实际已死后仍会被注入，表现为上游的 `Encrypted content could not be ...`（密文轮换途中的报错）。头部解析失败、版本字节不符、或时间戳落在 `[now-ttl, now+2min]` 之外时回退到 `now`——宁可保守计时，也不要因为畸形头部把票判成永不过期或一出生就过期。
- 采集限流是对上游实现的有意偏离。原实现对每个 `(账号, 模型)` 无限起 goroutine，且早期提交里的 `HarvestMaxAttempts` / `HarvestFailStreak` 熔断在后续 hardening 中被删除，改成日志里的 `mode=continuous`——没有尝试上限。本部署 3941 个 active OAuth 账号 × 2 个默认模型 ≈ 7882 并发探测挤单个采集代理，且拿不到 292 的账号会按 `probe_interval`（默认 6s）永久重试。在"IP 新鲜度决定成败"的前提下，每 6 秒拿数千发请求锤同一个出口 IP 是烧掉它最快的方式，与该功能的目的直接相反。
### Notes & Caveats
- 门票默认关闭，未开启时全链路行为与 `antistorm.1` 完全一致——不采集、不注入。
- 门票与本分支的 capacity 模式是组合关系，不冲突：`guardOpenAICodexTurnStateEcho` 在 capacity 模式下早退（token 级归属由投影层负责），`applyOpenAICodexTicket` 在 forward / passthrough / messages / ws 四个注入点独立生效，注入时 `h.Set` 无条件覆盖客户端回带值，包括覆盖掉已降智的 312。
- 采集仅覆盖 `status=active` 且 OAuth-like 的非影子账号；本部署另有 10463 个 `error` 状态 OAuth 账号会被跳过。
- 探测 miss 日志里的 `len` 是关键诊断位：312 说明当前出口 IP 已被降智，不是网络故障。
- 尚未实现采集代理池化轮换。当前单 URL（账号级 → 全局设置 → 配置文件，取首个非空）意味着所有探测共享一个出口 IP；若代理形态是"每建新连接自动换出口 IP"的轮换网关，现有 `req.Close = true` + no-reuse transport 已足够，无需额外代码；若是一批固定 IP，则需要轮询与烧号冷却，本版未做。
- 未合并 v0.2.6 的非 openai/codex 内容（兑换记录分页、deepseek、antigravity、gemini、以及 `ouchihao` 那批 `codex/bug-*` 分支——后者的 "codex" 指用 Codex 撰写的 PR，内容是支付/TOTP/订单等 UI 修复，与 OpenAI Codex 无关）。基线仍是 v0.2.5，非完整 0.2.6 rebase。
- 验证：`go build ./...` 通过；干净环境（清除 `*_proxy` 环境变量）下 `go test ./...` 51 个包全过、零 FAIL。此前 `TestAliyunCaptchaVerifier_TransportError` 的失败已定位为代理环境变量污染，单跑在合并前后均通过。未做真实账号 smoke test，门票采集链路未经真实上游验证。
- 前端确有改动（门票系列带入 13 文件 / +223 行：管理端 292 打票开关与打票代理输入、账号页票据状态、中英文案、类型定义），并已随 Dockerfile 的 `frontend-builder` 阶段编入二进制（`pnpm run check:i18n && vue-tsc -b && vite build` → `-tags embed`），线上已用 `292 打票` / `codexTicketEnabledDesc` 等仅存在于前端的字符串在容器内核实。
- 前端测试：门票相关的 `AccountUsageCell.spec.ts` 与 `SettingsView.spec.ts` 共 88 个用例全过；全量 `vitest run` 为 2165 过 / 2 失败（284 文件中 282 过）。失败的 `ChannelMonitorView.grok.spec.ts` 与 `GroupsView.codexManifest.spec.ts` 在官方 v0.2.5、合并前基线、当前分支三处表现一致，属上游既有问题，且本分支未修改这两个文件。

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
