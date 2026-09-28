package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// Basispoints（OpenAI Excel 加载项后端，bps.openai.com）账号级上游选项。
//
// 背景：官方 ChatGPT Excel 加载项不走 chatgpt.com/backend-api/codex，而是走
// bps.openai.com/basispoints/api/responses（内部名 Basispoints）。2026-09-25 实测
// （research/bps-prism-probe，team 与 pro 号各一发）：同一套 ChatGPT OAuth access_token
// + chatgpt-account-id 即可调通；gpt-6-astra 真实返回 astra，无 safety-buffering 响应头，
// 无降级路由。本文件把该端点实现为账号级可选上游：开了开关的账号，兼容的请求改道 bps，
// 不兼容的请求（图片输入、结构化输出、service_tier、compact、影子账号等）仍走原
// codex 上游——资格判定是每个请求独立进行的，开关不是"全部流量"。
// 客户端 function/custom 工具经 input[].additional_tools 原生声明
// （openai_bps_excel_tools.go），仅原生 responses 入站挂 update_plan 归一与信封
// 兜底拦截；其余入站路径的带工具请求仍回落 codex。
//
// 线路形状依据 ghcp_proxy（github.com/Nonary/ghcp_proxy）的 excel_upstream.py 与实测：
// - model_selection:"explicit" 是关键字段，缺了上游会按默认路由处理模型 slug；
// - 请求体只放行白名单字段：tools / previous_response_id / reasoning 对象 / service_tier
//   等都会被上游 422 或 403，一律不带（顶层 tools 改放 input[].additional_tools）；
// - reasoning_effort 是顶层标量（low/medium/high/xhigh），codex 的 max/ultra 档在 bps
//   不存在，收敛到 xhigh，none/未知收敛到 medium；
// - 响应总是 SSE（即使 stream:false），非流式客户端由既有 handleSSEToJSON 折叠；
// - bps 不使用 turn-state 门票，也不回 X-Codex-Safety-Buffering-* 头，打票注入与
//   降智观察头在该端点上自然空转；降智阻断按响应模型判定的逻辑不受影响（bps 实得模型
//   与请求一致，不会误触发）。

const (
	openAIBpsExcelURL              = "https://bps.openai.com/basispoints/api/responses"
	openAIBpsExcelHost             = "bps.openai.com"
	openAIBpsExcelUpstreamEndpoint = "/basispoints/api/responses"

	// OpenAIBpsExcelEnabledExtraKey 是 accounts.extra 上的账号级开关键。
	OpenAIBpsExcelEnabledExtraKey = "openai_bps_excel_enabled"
)

// openAIBpsUsedContextKey 记录当前转发尝试是否走了 bps 上游，
// 供结果落库（upstream_endpoint 标记）与各路径的"构建后补丁"判断跳过 codex 装饰。
// 写入方是 buildUpstreamRequest 的 bps 挂点：每次构建显式覆盖（true/false），
// 防止 failover 换号后沿用上一轮尝试的标记。
const openAIBpsExcelUsedContextKey = "openai_bps_excel_used"

// openAIBpsExcelModels 是 bps 目前实际服务的模型。目录会变：gpt-6-sol / gpt-6-luna
// 在 2026-09-25 实测 403 basispoints_model_access_changed，2026-09-26 复测已开放
// 且实得模型与请求一致（probe4.py）。若上游再次收走，bps 会回 403，按普通上游
// 错误进入换号逻辑。
var openAIBpsExcelModels = map[string]bool{
	"gpt-6-astra":   true,
	"gpt-6-sol":     true,
	"gpt-6-luna":    true,
	"gpt-5.6-sol":   true,
	"gpt-5.6-terra": true,
	"gpt-5.6-luna":  true,
}

// bps 出站身份头：与官方 Excel 加载项保持一致（ghcp excel_upstream.py 的默认集）。
var openAIBpsExcelClientHeaders = map[string]string{
	"x-basispoints-auth-mode":                             "chatgpt",
	"x-openai-internal-basispoints-client-agent-profile":  "excel",
	"x-openai-internal-basispoints-client-editor":         "excel",
	"x-openai-internal-basispoints-client-host":           "office",
	"x-openai-internal-basispoints-client-platform":       "excel",
	"x-openai-internal-basispoints-client-platform-class": "PC",
	"x-openai-internal-basispoints-client-product":        "basispoints-excel-plugin",
	"x-openai-internal-basispoints-client-runtime":        "desktop",
	"x-openai-internal-basispoints-office-host":           "Excel",
	"x-openai-internal-basispoints-office-platform":       "PC",
	"x-stainless-arch":                                    "unknown",
	"x-stainless-lang":                                    "js",
	"x-stainless-os":                                      "Unknown",
	"x-stainless-package-version":                         "6.31.0",
	"x-stainless-retry-count":                             "0",
	"x-stainless-runtime":                                 "browser:chrome",
}

// openAIBpsToolRelayContextKey 存放本次转发尝试的客户端工具目录（仅 responses 入站
// 且声明了工具时有值）；响应侧的 run_officejs 拦截改写按它还原客户端调用。
const openAIBpsToolRelayContextKey = "openai_bps_tool_relay"

// openAIBpsToolRelayContext 是一次 bps 转发尝试的工具中继上下文。
type openAIBpsToolRelayContext struct {
	specs map[string]openAIBpsClientToolSpec
	// scope 是原生调用回放缓存的隔离域（按 API Key），见 openAIBpsNativeCallScope。
	scope string
}

// setOpenAIBpsToolRelayContext 覆盖式写入工具目录：nil 表示本次尝试无中继，
// 同时清掉上一轮尝试（bps 失败 failover 回 codex）遗留的目录，防止误拦截。
func setOpenAIBpsToolRelayContext(c *gin.Context, specs map[string]openAIBpsClientToolSpec, scope string) {
	if c == nil {
		return
	}
	if len(specs) == 0 {
		c.Set(openAIBpsToolRelayContextKey, nil)
		return
	}
	c.Set(openAIBpsToolRelayContextKey, &openAIBpsToolRelayContext{specs: specs, scope: scope})
}

// openAIBpsNativeCallScope 返回原生调用回放缓存的隔离域：按 API Key 隔离，
// 不同租户即使构造出相同 call_id 也取不到彼此的原生条目。不按账号隔离——
// 同一对话 failover 换号后仍应命中逐字节回放。无 API Key（内部调用/测试）
// 时退化为公共域。
func openAIBpsNativeCallScope(c *gin.Context) string {
	return fmt.Sprintf("apikey:%d", getAPIKeyIDFromContext(c))
}

func openAIBpsToolRelayFromContext(c *gin.Context) *openAIBpsToolRelayContext {
	if c == nil {
		return nil
	}
	v, ok := c.Get(openAIBpsToolRelayContextKey)
	if !ok {
		return nil
	}
	relay, _ := v.(*openAIBpsToolRelayContext)
	return relay
}

func openAIBpsExcelUsed(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, ok := c.Get(openAIBpsExcelUsedContextKey)
	used, _ := v.(bool)
	return ok && used
}

// openAIBpsExcelEnabled 读取账号级 bps 上游开关，兼容 bool 与字符串写法，缺省关闭。
func openAIBpsExcelEnabled(account *Account) bool {
	if account == nil || account.Extra == nil {
		return false
	}
	raw, ok := account.Extra[OpenAIBpsExcelEnabledExtraKey]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "on", "yes":
			return true
		}
	}
	return false
}

// openAIBpsExcelCanonicalModel 把请求体里的上游模型名归一到 bps 目录名；
// 返回空串表示该模型不在 bps 服务目录内。codexVersionModelPrefixes 未覆盖
// gpt-6-astra 的档位/日期后缀（astra 的 effort 走 reasoning.effort 而不是模型名
// 后缀），这里对目录内四个模型统一做一次前缀+后缀折叠。
func openAIBpsExcelCanonicalModel(model string) string {
	if normalized, ok := normalizeKnownCodexModel(strings.TrimSpace(model)); ok && openAIBpsExcelModels[normalized] {
		return normalized
	}
	key := codexModelLookupKey(model)
	for base := range openAIBpsExcelModels {
		if key == base {
			return base
		}
		if suffix, cut := strings.CutPrefix(key, base+"-"); cut && isKnownCodexModelSuffix(suffix) {
			return base
		}
	}
	return ""
}

// openAIBpsExcelReasoningEffort 把 codex 推理档位映射到 bps 顶层 reasoning_effort。
// bps 只有 low/medium/high/xhigh 四档：max/ultra 收敛到 xhigh；none/minimal 与未知值
// 按 ghcp 的默认行为收敛到 medium。
func openAIBpsExcelReasoningEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "medium", "high", "xhigh":
		return strings.ToLower(strings.TrimSpace(effort))
	case "max", "ultra":
		return "xhigh"
	default:
		return "medium"
	}
}

// openAIBpsDroppableHostedToolTypes 是改道 bps 时允许静默丢弃的托管工具类型。
// 这些工具在 Codex CLI 请求里几乎每轮都带（web_search / tool_search 默认开启，
// image_generation 由分组桥接注入），排除它们等于让 Codex 流量永远走不了 bps；
// 目录提示词已明确告诉模型 web-search 等原生工具不可用。其余托管工具（mcp、
// local_shell、file_search、code_interpreter、computer_use 等）一旦丢弃就改变
// 请求能力，留在 codex 上游。
var openAIBpsDroppableHostedToolTypes = map[string]bool{
	"image_generation":   true,
	"web_search":         true,
	"web_search_preview": true,
	"tool_search":        true,
}

// openAIBpsRequestToolsRelayable 判定请求的工具声明能否在 bps 上成立：
// tool_choice 强制（required / 指定工具对象）不在 bps 词汇表内（字段会被丢弃，
// 无法保证模型一定调用）；目录外的托管工具不能静默丢弃。顶层 tools 与
// input[].additional_tools 两个载体都检查。tool_choice:"none" 时工具声明无效，
// 一律放行。
func openAIBpsRequestToolsRelayable(decoded map[string]any) bool {
	if decoded == nil {
		return true
	}
	switch choice := decoded["tool_choice"].(type) {
	case nil:
	case string:
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "none":
			return true
		case "", "auto":
		default:
			// required 等强制档：bps 无法保证模型一定发起调用。
			return false
		}
	default:
		// 对象形态（指定函数 / 指定托管工具，如图片接口的 image_generation）。
		return false
	}
	toolsRelayable := func(tools []any) bool {
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			toolType, _ := tool["type"].(string)
			toolType = strings.ToLower(strings.TrimSpace(toolType))
			switch toolType {
			case "function", "custom", "namespace":
			default:
				if !openAIBpsDroppableHostedToolTypes[toolType] {
					return false
				}
			}
		}
		return true
	}
	tools, _ := decoded["tools"].([]any)
	if !toolsRelayable(tools) {
		return false
	}
	for _, carrier := range openAIBpsAdditionalToolsCarriers(decoded["input"]) {
		nested, _ := carrier["tools"].([]any)
		if !toolsRelayable(nested) {
			return false
		}
	}
	return true
}

// openAIBpsExcelRequestEligible 判定当前请求能否改道 bps。
// 只读 context、gin context、账号与最终请求体，不改变任何状态。
func openAIBpsExcelRequestEligible(ctx context.Context, c *gin.Context, account *Account, body []byte) bool {
	if account == nil || !account.UsesOpenAICodexProtocol() {
		return false
	}
	if !openAIBpsExcelEnabled(account) {
		return false
	}
	// 本请求的 bps 改写已被上游拒收（400/422）：后续尝试一律走 codex 上游。
	if openAIBpsExcelRejected(c) {
		return false
	}
	// 图片接口自建的 Responses 请求（强制 image_generation 工具）：调用方会在
	// buildUpstreamRequest 之后把 URL 改回 codex 端点，改道只会产出 URL/Host/
	// 请求体互相矛盾的请求；bps 也没有图像生成能力。
	if isOpenAIImagesSelfBuiltRequest(ctx) {
		return false
	}
	// compact 请求的上游是 /responses/compact，bps 没有对应端点。
	if isOpenAIResponsesCompactPath(c) {
		return false
	}
	// spark 影子账号的语义跟随本体账号与专用查询通道，不改道。
	if account.IsShadow() {
		return false
	}
	if openAIBpsExcelCanonicalModel(gjson.GetBytes(body, "model").String()) == "" {
		return false
	}
	// bps 无视觉能力（ghcp 本地能力表 vision:false），图片输入请求留在 codex 上游。
	if openAIRequestBodyMayContainImageInput(body) {
		return false
	}
	// 客户端 function/custom 工具经 additional_tools 原生声明，但 update_plan 归一
	// 与信封兜底只挂在原生 responses 入站的响应处理上；messages / chat completions
	// 入站的带工具请求仍留在 codex 上游。
	// tool_choice:"none" 与纯 image_generation 不产生中继目录，视为无工具。
	var decodedForTools map[string]any
	if err := json.Unmarshal(body, &decodedForTools); err != nil {
		return false
	}
	if len(openAIBpsClientToolSpecs(decodedForTools)) > 0 && !openAIBpsExcelToolRelayIngress(c) {
		return false
	}
	if !openAIBpsRequestToolsRelayable(decodedForTools) {
		return false
	}
	// previous_response_id 不参与判定：OAuth 账号的 HTTP 路径在构建请求前已统一
	// 删除该字段（forwardOpenAI 的 markPatchDelete），codex 上游同样收不到，
	// 回落 codex 不会更好。
	// 结构化输出（text.format）与 service_tier 在 bps 词汇表外，静默丢弃会改变
	// 请求语义，留在 codex 上游。
	formatType := strings.TrimSpace(gjson.GetBytes(body, "text.format.type").String())
	if formatType != "" {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(body, "service_tier").String()) != "" {
		return false
	}
	return true
}

// openAIBpsExcelToolRelayIngress 报告当前入站是否支持工具中继：只有原生
// responses 入站（/v1/responses、/responses、/backend-api/codex/responses 等以
// /responses 结尾的路径）的响应处理挂了 run_officejs 拦截改写。
func openAIBpsExcelToolRelayIngress(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	return strings.HasSuffix(c.Request.URL.Path, "/responses")
}

// rewriteOpenAIBodyForBpsExcel 把最终 codex 形状请求体重写为 bps 白名单形状。
// 调用前提是 openAIBpsExcelRequestEligible 已通过。返回重写后的请求体与本次
// 请求的客户端工具目录（无工具时为 nil，响应侧据此决定是否挂中继拦截）。
// scope 是原生调用回放缓存的隔离域（openAIBpsNativeCallScope）。
func rewriteOpenAIBodyForBpsExcel(body []byte, scope string) ([]byte, map[string]openAIBpsClientToolSpec, error) {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, nil, fmt.Errorf("decode request body for bps rewrite: %w", err)
	}
	specs := openAIBpsClientToolSpecs(decoded)

	out := map[string]any{
		"model":           openAIBpsExcelCanonicalModel(gjson.GetBytes(body, "model").String()),
		"model_selection": "explicit",
		"stream":          false,
		"store":           false,
	}
	if stream, ok := decoded["stream"].(bool); ok {
		out["stream"] = stream
	}
	// 历史条目先翻译（剥掉客户端私有元数据、滤掉裸 reasoning、清洗客户端自带的
	// additional_tools、信封调用逐字节回放），再前置序言。顶层 tools 字段 bps 拒收
	// （422），改放进 input 头部的 additional_tools 载体——bps 原生声明、模型直接
	// 调用（见 openai_bps_excel_tools.go 文件头）。顺序：工具声明 → 序言 → 历史，
	// 三段在同一会话内逐字节恒定，上游 prompt cache 前缀不断。
	translatedInput, _ := openAIBpsTranslateInputItems(decoded["input"], specs, scope)
	var head []any
	if len(specs) > 0 {
		if tools, ok := decoded["tools"].([]any); ok {
			if declarations := openAIBpsNativeToolDeclarations(tools); len(declarations) > 0 {
				head = append(head, openAIBpsAdditionalToolsItem(declarations))
			}
		}
	}
	head = append(head, openAIBpsMessageItem("developer", openAIBpsCatalogInstructions(specs)))
	if reminder := openAIBpsCatalogReminder(specs); reminder != "" {
		head = append(head, openAIBpsMessageItem("developer", reminder))
	}
	out["input"] = append(head, translatedInput...)
	if instructions, ok := decoded["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		out["instructions"] = instructions
	}

	effort := ""
	if reasoning, ok := decoded["reasoning"].(map[string]any); ok {
		if v, ok := reasoning["effort"].(string); ok {
			effort = v
		}
	}
	if effort == "" {
		if v, ok := decoded["reasoning_effort"].(string); ok {
			effort = v
		}
	}
	out["reasoning_effort"] = openAIBpsExcelReasoningEffort(effort)

	if cm, ok := decoded["context_management"].([]any); ok && len(cm) > 0 {
		out["context_management"] = cm
	} else {
		out["context_management"] = []any{
			map[string]any{"type": "compaction", "compact_threshold": float64(200000)},
		}
	}

	conversationKey := ""
	if pck, ok := decoded["prompt_cache_key"].(string); ok && strings.TrimSpace(pck) != "" {
		conversationKey = strings.TrimSpace(pck)
		out["prompt_cache_key"] = conversationKey
	}
	if conversationKey == "" {
		// 对话根指纹用翻译后的条目计算且在序言注入之前：Codex 每轮改写的私有
		// 元数据已被翻译剥掉，序言则逐字节恒定；用原始条目会把每轮都认成新会话。
		conversationKey = openAIBpsExcelConversationFingerprint(translatedInput)
	}
	// turn 指纹与 agent_iteration 仍按原始输入计算：翻译后的条目形状与客户端
	// 语义回合不再一一对应，而 Excel 端语义是"同一条用户消息内的工具执行保持
	// turn_id、只递增 agent_iteration"。
	_, hasInput := decoded["input"]
	out["metadata"] = deriveOpenAIBpsExcelMetadata(decoded, hasInput, conversationKey)

	rewritten, err := json.Marshal(out)
	if err != nil {
		return nil, nil, fmt.Errorf("encode bps request body: %w", err)
	}
	return rewritten, specs, nil
}

// deriveOpenAIBpsExcelMetadata 生成 bps 需要的任务/回合并发标识。
// 标识全部派生自请求内容（不引入随机数）：同一对话的每一轮共享 task_id，
// 同一轮的重试共享 turn_id，与官方 Excel 加载项的语义一致（ghcp 同款推导）。
func deriveOpenAIBpsExcelMetadata(decoded map[string]any, hasInput bool, conversationKey string) map[string]any {
	metadata := map[string]any{}
	// 保留客户端自带的标量 metadata（截断保护与 ghcp 一致：key 64、值 512）。
	if raw, ok := decoded["metadata"].(map[string]any); ok {
		for key, value := range raw {
			switch v := value.(type) {
			case string:
				if len(key) <= 64 {
					metadata[key] = truncateString(v, 512)
				}
			case float64, bool:
				if len(key) <= 64 {
					metadata[key] = fmt.Sprintf("%v", v)
				}
			}
		}
	}

	inputItems, _ := decoded["input"].([]any)
	if conversationKey == "" {
		conversationKey = openAIBpsExcelConversationFingerprint(inputItems)
	}
	turnFingerprint, iteration := openAIBpsExcelTurnState(decoded, inputItems)

	if _, ok := metadata["agent_iteration"]; !ok {
		metadata["agent_iteration"] = strconv.Itoa(iteration)
	}
	if _, ok := metadata["task_id"]; !ok {
		metadata["task_id"] = uuid.NewSHA1(uuid.NameSpaceURL, []byte("sub2api/bps-excel/"+conversationKey)).String()
	}
	if _, ok := metadata["turn_id"]; !ok {
		metadata["turn_id"] = uuid.NewSHA1(uuid.NameSpaceURL, []byte("sub2api/bps-excel/"+conversationKey+"/turn/"+turnFingerprint)).String()
	}
	return metadata
}

// openAIBpsExcelSHA256 返回字节串的 sha256 十六进制摘要。
func openAIBpsExcelSHA256(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// openAIBpsExcelConversationFingerprint 用首个 input 项的稳定哈希标识对话根。
func openAIBpsExcelConversationFingerprint(inputItems []any) string {
	for _, item := range inputItems {
		if rendered, err := json.Marshal(item); err == nil {
			return openAIBpsExcelSHA256(rendered)
		}
	}
	return "anonymous"
}

// openAIBpsExcelTurnState 计算回合指纹与 agent_iteration：
// Excel 加载项在同一条用户消息的任意多次工具执行间保持 turn_id 不变，只有
// agent_iteration 递增（ghcp _agent_turn_state 的语义复刻）。
func openAIBpsExcelTurnState(decoded map[string]any, inputItems []any) (string, int) {
	if raw, ok := decoded["input"].(string); ok {
		return openAIBpsExcelSHA256([]byte(raw)), 1
	}
	if len(inputItems) == 0 {
		return "anonymous", 1
	}
	lastUserIndex := -1
	for index, item := range inputItems {
		if m, ok := item.(map[string]any); ok {
			if role, _ := m["role"].(string); strings.EqualFold(strings.TrimSpace(role), "user") {
				lastUserIndex = index
			}
		}
	}
	turnPrefix := inputItems
	if lastUserIndex >= 0 {
		turnPrefix = inputItems[:lastUserIndex+1]
	} else {
		turnPrefix = inputItems[:1]
	}
	fingerprint := "anonymous"
	if rendered, err := json.Marshal(turnPrefix); err == nil {
		fingerprint = openAIBpsExcelSHA256(rendered)
	}
	iteration := 1
	if lastUserIndex >= 0 {
		for _, item := range inputItems[lastUserIndex+1:] {
			if m, ok := item.(map[string]any); ok {
				if itemType, _ := m["type"].(string); itemType == "function_call_output" || itemType == "custom_tool_call_output" {
					iteration++
				}
			}
		}
	}
	return fingerprint, iteration
}

// applyOpenAIBpsExcelHeaders 设置 bps 端点要求的身份与客户端头。
// Authorization 已由 buildOpenAIAuthenticationHeaders 设置；chatgpt-account-id 由
// resolveAndSetOpenAIChatGPTAccountHeaders 设置，这里镜像出 x-openai-account-id。
func applyOpenAIBpsExcelHeaders(header http.Header, account *Account, isStream bool) {
	if accountID := strings.TrimSpace(header.Get("chatgpt-account-id")); accountID != "" {
		header.Set("x-openai-account-id", accountID)
	}
	for key, value := range openAIBpsExcelClientHeaders {
		header.Set(key, value)
	}
	header.Set("origin", "https://"+openAIBpsExcelHost)
	if isStream {
		header.Set("accept", "text/event-stream")
	} else {
		header.Set("accept", "application/json")
	}
	header.Set("content-type", "application/json")
}
