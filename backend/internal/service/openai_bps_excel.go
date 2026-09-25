package service

import (
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
// 不兼容的请求（声明了客户端工具、图片输入、结构化输出、service_tier、容量记账账号等）
// 仍走原 codex 上游——资格判定是每个请求独立进行的，开关不是"全部流量"。
//
// 线路形状依据 ghcp_proxy（github.com/Nonary/ghcp_proxy）的 excel_upstream.py 与实测：
// - model_selection:"explicit" 是关键字段，缺了上游会按默认路由处理模型 slug；
// - 请求体只放行白名单字段：tools / previous_response_id / reasoning 对象 / service_tier
//   等都会被上游 422 或 403，一律不带；
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

// openAIBpsExcelUsedContextKey 记录当前转发尝试是否走了 bps 上游，
// 供结果落库（upstream_endpoint 标记）与各路径的"构建后补丁"判断跳过 codex 装饰。
const openAIBpsExcelUsedContextKey = "openai_bps_excel_used"

// openAIBpsExcelModels 是 bps 目前实际服务的模型（2026-09-25 实测：
// gpt-6-sol / gpt-6-luna 在 bps 上 403 basispoints_model_access_changed）。
var openAIBpsExcelModels = map[string]bool{
	"gpt-6-astra":   true,
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

func markOpenAIBpsExcelUsed(c *gin.Context) {
	if c != nil {
		c.Set(openAIBpsExcelUsedContextKey, true)
	}
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

// openAIBpsExcelRequestEligible 判定当前请求能否改道 bps。
// 只读 gin context、账号与最终请求体，不改变任何状态。
func openAIBpsExcelRequestEligible(c *gin.Context, account *Account, body []byte) bool {
	if account == nil || !account.UsesOpenAICodexProtocol() {
		return false
	}
	if !openAIBpsExcelEnabled(account) {
		return false
	}
	// compact 请求的上游是 /responses/compact，bps 没有对应端点。
	if isOpenAIResponsesCompactPath(c) {
		return false
	}
	// 容量记账账号依赖 codex 上游的会话投影与回执做记账，bps 无法承载。
	if _, enabled := account.codexCapacityPolicy(); enabled {
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
	// 客户端声明的 function/custom 工具会被 bps 422（请求体没有 tools 词汇）。
	// 唯一的例外是网关按分组配置注入的 image_generation 工具——它不构成客户端
	// 工具语义，剥离后请求仍是纯文本对话，可以改道（白名单重写也不会带 tools）。
	if openAIRequestBodyHasClientFunctionTools(body) {
		return false
	}
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

// openAIRequestBodyHasClientFunctionTools 判断请求是否声明了图像生成以外的客户端工具。
func openAIRequestBodyHasClientFunctionTools(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return false
	}
	hasClientTools := false
	tools.ForEach(func(_, tool gjson.Result) bool {
		toolType := strings.TrimSpace(tool.Get("type").String())
		if toolType != "" && toolType != "image_generation" {
			hasClientTools = true
			return false
		}
		return true
	})
	return hasClientTools
}

// rewriteOpenAIBodyForBpsExcel 把最终 codex 形状请求体重写为 bps 白名单形状。
// 调用前提是 openAIBpsExcelRequestEligible 已通过。
func rewriteOpenAIBodyForBpsExcel(body []byte) ([]byte, error) {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode request body for bps rewrite: %w", err)
	}

	out := map[string]any{
		"model":           openAIBpsExcelCanonicalModel(gjson.GetBytes(body, "model").String()),
		"model_selection": "explicit",
		"stream":          false,
		"store":           false,
	}
	if stream, ok := decoded["stream"].(bool); ok {
		out["stream"] = stream
	}
	input, hasInput := decoded["input"]
	if hasInput {
		out["input"] = input
	}
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
	out["metadata"] = deriveOpenAIBpsExcelMetadata(decoded, hasInput, conversationKey)

	return json.Marshal(out)
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
