package service

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
)

// bps 工具中继的流式拦截器。
//
// bps 的 SSE 事件序列里，一次 run_officejs 原生调用表现为：
//
//	response.output_item.added (item=function_call, name=run_officejs, arguments="")
//	response.function_call_arguments.delta × N
//	response.function_call_arguments.done
//	response.output_item.done (item=完整原生调用)
//	response.completed (response.output 里仍是原生调用)
//
// 拦截器在 output_item.added 认出受管条目后吞掉该条目的全部后续事件，到
// output_item.done 拿完整条目跑 openAIBpsExtractNativeToolCall：成功则合成一组
// 客户端形状的事件（added → delta(全量) → done → item.done）注入下行，并在
// response.completed 里把原生条目替换掉；失败则按原生形状原样补发（客户端会回
// "unsupported call: run_officejs"，请求侧下一轮翻译成协议重试指引，见
// openAIBpsNormalizedToolOutput）。事件与数据两行一体的 SSE 文档里，event: 行
// 先于 data: 行到达，拦截器对受管事件族的 event: 行做一行暂存，待 data 行裁决
// 后一并放行或丢弃。

// openAIBpsRelayManagedEventTypes 是拦截器需要裁决的事件族（其余事件一律直通）。
var openAIBpsRelayManagedEventTypes = map[string]bool{
	"response.output_item.added":             true,
	"response.output_item.done":              true,
	"response.function_call_arguments.delta": true,
	"response.function_call_arguments.done":  true,
	"response.custom_tool_call_input.delta":  true,
	"response.custom_tool_call_input.done":   true,
	"response.completed":                     true,
	"response.done":                          true,
	// 非成功终态的 response.output 同样可能内嵌原生 run_officejs 条目。
	"response.incomplete": true,
	"response.failed":     true,
}

type openAIBpsRelayAction int

const (
	// openAIBpsRelayPass 放行：data 行走正常处理（暂存的 event 行先补发）。
	openAIBpsRelayPass openAIBpsRelayAction = iota
	// openAIBpsRelaySwallow 吞掉：该 data 行与暂存的 event 行都不下行。
	openAIBpsRelaySwallow
	// openAIBpsRelayEmit 改写：吞掉原始行，改为逐行喂合成事件。
	openAIBpsRelayEmit
)

// openAIBpsBufferedNativeCall 是一个被吞掉的受管条目的缓冲状态。
type openAIBpsBufferedNativeCall struct {
	outputIndex int
	addedItem   map[string]any // output_item.added 里的条目骨架（arguments 未累积）
}

// openAIBpsToolStreamRelay 是一次 bps 流式转发的工具中继状态机。
type openAIBpsToolStreamRelay struct {
	specs map[string]openAIBpsClientToolSpec
	scope string // 原生调用回放缓存隔离域

	// onProgress 在吞掉受管条目的增量事件时回调：这些事件不进正常管道，
	// 首输出超时看不到进度，长参数工具调用会被误判为上游卡死而 failover。
	onProgress func()

	buffered map[string]*openAIBpsBufferedNativeCall // 原生条目 id → 缓冲
	// 已完成提取：原生条目 id 与 call_id 都索引到客户端形状条目，供 completed 事件改写。
	extracted map[string]map[string]any

	stashedEventLine string
	hasStash         bool
	bypass           bool // 合成事件回流处理时置位，防止拦截器再拦自己
}

func newOpenAIBpsToolStreamRelay(specs map[string]openAIBpsClientToolSpec, scope string) *openAIBpsToolStreamRelay {
	return &openAIBpsToolStreamRelay{
		specs:     specs,
		scope:     scope,
		buffered:  make(map[string]*openAIBpsBufferedNativeCall),
		extracted: make(map[string]map[string]any),
	}
}

// hookLine 是 processSSELine 的前置钩子；返回 true 表示该行已被拦截器处置
// （吞掉/暂存/改写后回流），调用方直接返回。feed 用于把合成行回流进正常处理。
func (r *openAIBpsToolStreamRelay) hookLine(line string, feed func(line string)) bool {
	if r == nil || r.bypass {
		return false
	}
	if eventType, ok := extractOpenAISSEEventLine(line); ok {
		if openAIBpsRelayManagedEventTypes[eventType] {
			// 受管事件族的 event: 行先暂存：是否拦截要等同文档的 data 行到达后裁决。
			r.stashedEventLine = line
			r.hasStash = true
			return true
		}
		return false
	}
	data, ok := extractOpenAISSEDataLine(line)
	if !ok {
		return false
	}
	action, emit := r.filterData(data)
	switch action {
	case openAIBpsRelaySwallow:
		r.dropStash()
		return true
	case openAIBpsRelayEmit:
		r.dropStash()
		r.bypass = true
		for _, emitLine := range emit {
			feed(emitLine)
		}
		r.bypass = false
		return true
	default:
		if r.hasStash {
			stashed := r.stashedEventLine
			r.dropStash()
			r.bypass = true
			feed(stashed)
			r.bypass = false
		}
		return false
	}
}

func (r *openAIBpsToolStreamRelay) dropStash() {
	r.stashedEventLine = ""
	r.hasStash = false
}

// filterData 裁决一个 data 载荷（不含 "data: " 前缀）。
func (r *openAIBpsToolStreamRelay) filterData(data string) (openAIBpsRelayAction, []string) {
	if !gjson.Valid(data) {
		return openAIBpsRelayPass, nil
	}
	eventType := gjson.Get(data, "type").String()
	switch eventType {
	case "response.output_item.added":
		item := gjson.Get(data, "item")
		itemType := item.Get("type").String()
		if itemType != "function_call" && itemType != "custom_tool_call" {
			return openAIBpsRelayPass, nil
		}
		itemID := item.Get("id").String()
		name := item.Get("name").String()
		// 传输工具调用（待解信封）与和客户端同名的原生调用（如 update_plan，
		// 参数 schema 要归一）都缓冲到 done 再裁决；其余原生工具调用直通——
		// 客户端会以 "unsupported call" 报错，下一轮请求侧翻译成重试指引。
		// 名字解析与提取/completed 改写同一规则（剥 functions. 前缀、裸名唯一匹配），
		// 否则流式下发原生形状而 completed.output 却是客户端形状。
		if !openAIBpsIsTransportName(name) {
			if _, ok := openAIBpsResolveClientToolSpec(name, r.specs); !ok {
				return openAIBpsRelayPass, nil
			}
		}
		var skeleton map[string]any
		if err := json.Unmarshal([]byte(item.Raw), &skeleton); err != nil || skeleton == nil {
			return openAIBpsRelayPass, nil
		}
		r.buffered[itemID] = &openAIBpsBufferedNativeCall{
			outputIndex: int(gjson.Get(data, "output_index").Int()),
			addedItem:   skeleton,
		}
		return openAIBpsRelaySwallow, nil

	case "response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		itemID := gjson.Get(data, "item_id").String()
		if _, ok := r.buffered[itemID]; ok {
			// output_item.done 携带完整条目，增量事件不需要留存。
			if r.onProgress != nil {
				r.onProgress()
			}
			return openAIBpsRelaySwallow, nil
		}
		return openAIBpsRelayPass, nil

	case "response.output_item.done":
		itemID := gjson.Get(data, "item.id").String()
		buffered, ok := r.buffered[itemID]
		if !ok {
			return openAIBpsRelayPass, nil
		}
		delete(r.buffered, itemID)
		var fullItem map[string]any
		if err := json.Unmarshal([]byte(gjson.Get(data, "item").Raw), &fullItem); err != nil || fullItem == nil {
			return openAIBpsRelayPass, nil
		}
		return openAIBpsRelayEmit, r.finalizeNativeCall(buffered, fullItem)

	case "response.completed", "response.done", "response.incomplete", "response.failed":
		// 终态前仍在缓冲的条目（上游没发 output_item.done 就收尾）：客户端靠
		// output_item.done 取工具调用，不能只改 completed.output，先按终态里的
		// 完整条目补发事件序列。
		lines := r.flushBufferedFromTerminal(data)
		rewritten, changed := r.rewriteCompletedOutput(data)
		if !changed && len(lines) == 0 {
			return openAIBpsRelayPass, nil
		}
		if r.hasStash {
			lines = append(lines, r.stashedEventLine)
		}
		lines = append(lines, "data: "+rewritten)
		return openAIBpsRelayEmit, lines
	}
	return openAIBpsRelayPass, nil
}

// finalizeNativeCall 在 output_item.done 拿到完整原生条目后产出下行事件序列：
// 提取成功 → 客户端形状；失败 → 记一条带原因码的日志后按原生形状补发（自愈
// 回路见文件头注释）。
func (r *openAIBpsToolStreamRelay) finalizeNativeCall(buffered *openAIBpsBufferedNativeCall, fullItem map[string]any) []string {
	// 原生条目无论提取成败都按 call_id 缓存：成功时供下一轮逐字节回放；失败时
	// 客户端拿到的就是原生条目，回放命中同样逐字节一致。
	openAIBpsNativeCalls.Remember(r.scope, fullItem)
	extracted, reason := openAIBpsExtractNativeToolCallReason(fullItem, r.specs)
	if extracted == nil {
		// 只记键名不落载荷：下次再出新形态时直接从日志看懂模型的写法。
		envelopeName, envelopeKeys := "", ""
		if env := openAIBpsTransportEnvelope(fullItem); env != nil {
			envelopeName, _ = env["name"].(string)
			keys := make([]string, 0, len(env))
			for k := range env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			envelopeKeys = strings.Join(keys, ",")
		}
		logger.LegacyPrintf("service.openai_gateway",
			"[bps] tool relay extract failed: reason=%s native_name=%v envelope_name=%s envelope_keys=%s call_id=%v",
			reason, fullItem["name"], envelopeName, envelopeKeys, fullItem["call_id"])
		return r.nativePassthroughLines(buffered, fullItem)
	}
	// 流式期间的提取结果按 call_id / 原生 id 索引，供终态 output 改写复用。
	if callID, _ := fullItem["call_id"].(string); callID != "" {
		r.extracted[callID] = extracted
	}
	if nativeID, _ := fullItem["id"].(string); nativeID != "" {
		r.extracted[nativeID] = extracted
	}

	clientItem := cloneOpenAIBpsItem(extracted)
	if clientItem == nil {
		return r.nativePassthroughLines(buffered, fullItem)
	}
	clientItem["status"] = "completed"

	skeleton := cloneOpenAIBpsItem(buffered.addedItem)
	if skeleton == nil {
		skeleton = map[string]any{}
	}
	skeleton["id"] = extracted["id"]
	skeleton["call_id"] = extracted["call_id"]
	skeleton["name"] = extracted["name"]
	if ns, ok := extracted["namespace"]; ok {
		skeleton["namespace"] = ns
	}
	itemID, _ := extracted["id"].(string)
	var argsEvent, argsField, argsValue string
	if extracted["type"] == "custom_tool_call" {
		skeleton["type"] = "custom_tool_call"
		delete(skeleton, "arguments")
		skeleton["input"] = ""
		argsEvent = "response.custom_tool_call_input.done"
		argsField = "input"
		argsValue, _ = extracted["input"].(string)
	} else {
		skeleton["type"] = "function_call"
		delete(skeleton, "input")
		skeleton["arguments"] = ""
		argsEvent = "response.function_call_arguments.done"
		argsField = "arguments"
		argsValue, _ = extracted["arguments"].(string)
	}

	lines := make([]string, 0, 12)
	appendEvent := func(eventType string, payload map[string]any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		// 每个合成文档都要带 SSE 事件边界（空行），否则下游解析器会把相邻
		// 文档合并成一个事件块。
		lines = append(lines, "event: "+eventType, "data: "+string(raw), "")
	}
	appendEvent("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": buffered.outputIndex,
		"item":         skeleton,
	})
	appendEvent(argsEvent, map[string]any{
		"type":         argsEvent,
		"item_id":      itemID,
		"output_index": buffered.outputIndex,
		argsField:      argsValue,
	})
	appendEvent("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": buffered.outputIndex,
		"item":         clientItem,
	})
	return lines
}

// nativePassthroughLines 按原生形状补发被吞掉的条目（提取失败时的放行路径）。
func (r *openAIBpsToolStreamRelay) nativePassthroughLines(buffered *openAIBpsBufferedNativeCall, fullItem map[string]any) []string {
	itemID, _ := fullItem["id"].(string)
	lines := make([]string, 0, 9)
	appendEvent := func(eventType string, payload map[string]any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		lines = append(lines, "event: "+eventType, "data: "+string(raw), "")
	}
	appendEvent("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": buffered.outputIndex,
		"item":         buffered.addedItem,
	})
	if itemType, _ := fullItem["type"].(string); itemType == "custom_tool_call" {
		input, _ := fullItem["input"].(string)
		appendEvent("response.custom_tool_call_input.done", map[string]any{
			"type":         "response.custom_tool_call_input.done",
			"item_id":      itemID,
			"output_index": buffered.outputIndex,
			"input":        input,
		})
	} else {
		arguments, _ := fullItem["arguments"].(string)
		appendEvent("response.function_call_arguments.done", map[string]any{
			"type":         "response.function_call_arguments.done",
			"item_id":      itemID,
			"output_index": buffered.outputIndex,
			"arguments":    arguments,
		})
	}
	appendEvent("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": buffered.outputIndex,
		"item":         fullItem,
	})
	return lines
}

// rewriteCompletedOutput 改写 response.completed/.done 载荷：response.output 里的
// 原生工具调用替换为客户端形状（优先用流式期间已完成提取的结果，否则就地提取）。
// 返回改写后的载荷与是否发生变化。
func (r *openAIBpsToolStreamRelay) rewriteCompletedOutput(data string) (string, bool) {
	rewritten, changed := openAIBpsRewriteCompletedToolCalls([]byte(data), r.specs, r.extracted, r.scope)
	if !changed {
		return data, false
	}
	return string(rewritten), true
}

// openAIBpsRewriteCompletedToolCalls 改写一个完整 response 载荷（completed 事件或
// 非流式折叠后的 JSON 响应体）的 output 数组：原生 run_officejs / 同名原生调用
// 替换为客户端工具调用。knownExtracted 是流式期间已提取的映射（可为 nil）。
// scope 是回放缓存隔离域：就地提取的原生条目同样写入缓存。
func openAIBpsRewriteCompletedToolCalls(payload []byte, specs map[string]openAIBpsClientToolSpec, knownExtracted map[string]map[string]any, scope string) ([]byte, bool) {
	if len(specs) == 0 || !gjson.ValidBytes(payload) {
		return payload, false
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return payload, false
	}
	// 兼容两种外形：completed 事件 {"response":{"output":[...]}} 与裸响应 {"output":[...]}。
	container := decoded
	if nested, ok := decoded["response"].(map[string]any); ok {
		container = nested
	}
	output, ok := container["output"].([]any)
	if !ok || len(output) == 0 {
		return payload, false
	}
	changed := false
	for i, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := item["type"].(string)
		if itemType != "function_call" && itemType != "custom_tool_call" {
			continue
		}
		var clientItem map[string]any
		if knownExtracted != nil {
			if id, _ := item["id"].(string); id != "" {
				clientItem = knownExtracted[id]
			}
			if clientItem == nil {
				if callID, _ := item["call_id"].(string); callID != "" {
					clientItem = knownExtracted[callID]
				}
			}
		}
		if clientItem == nil {
			openAIBpsNativeCalls.Remember(scope, item)
			extracted, ok := openAIBpsExtractNativeToolCall(item, specs)
			if !ok {
				continue
			}
			clientItem = extracted
		}
		replacement := cloneOpenAIBpsItem(clientItem)
		if replacement == nil {
			continue
		}
		replacement["status"] = "completed"
		output[i] = replacement
		changed = true
	}
	if !changed {
		return payload, false
	}
	container["output"] = output
	raw, err := json.Marshal(decoded)
	if err != nil {
		return payload, false
	}
	return raw, true
}

// flushBufferedFromTerminal 为终态事件到达时仍在缓冲的条目补发事件序列：
// 在终态载荷的 response.output 里按 id 找完整条目走 finalizeNativeCall；
// 找不到的只能丢弃并记日志（客户端无从执行一个没有参数的调用）。
func (r *openAIBpsToolStreamRelay) flushBufferedFromTerminal(data string) []string {
	if len(r.buffered) == 0 {
		return nil
	}
	fullItems := map[string]map[string]any{}
	output := gjson.Get(data, "response.output")
	if !output.Exists() {
		output = gjson.Get(data, "output")
	}
	for _, raw := range output.Array() {
		id := raw.Get("id").String()
		if _, ok := r.buffered[id]; !ok {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(raw.Raw), &item); err == nil && item != nil {
			fullItems[id] = item
		}
	}
	ids := make([]string, 0, len(r.buffered))
	for id := range r.buffered {
		ids = append(ids, id)
	}
	// 按 output_index 顺序补发，保持客户端看到的条目次序。
	sort.Slice(ids, func(i, j int) bool {
		return r.buffered[ids[i]].outputIndex < r.buffered[ids[j]].outputIndex
	})
	var lines []string
	for _, id := range ids {
		buffered := r.buffered[id]
		delete(r.buffered, id)
		item, ok := fullItems[id]
		if !ok {
			logger.LegacyPrintf("service.openai_gateway",
				"[bps] tool relay dropped buffered item without output_item.done: item_id=%s output_index=%d", id, buffered.outputIndex)
			continue
		}
		lines = append(lines, r.finalizeNativeCall(buffered, item)...)
	}
	return lines
}
