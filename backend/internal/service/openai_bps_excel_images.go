package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// bps 传图规则（2026-09-28 直连 bps 实测，research 探针 bpsimg.py）：
//
// bps 有视觉能力（gpt-6-astra 能正确识别红/绿/蓝纯色图、480×480 噪声图、JPEG），
// 但只在 function_call_output.output[] 里接受 input_image：
//   - message.content 里的 input_image（带不带 detail、1×1 还是 64×64）→ 422；
//   - custom_tool_call_output.output 里的 input_image → 422；
//   - function_call_output.output 里的 input_image → 200，模型看得到图；
//     对应调用是 custom_tool_call、是未声明的函数名、或请求里根本没有工具声明都行。
//
// 所以改写成 bps 能收的形状而不是把图片请求挡在外面：
//   - custom_tool_call_output 带图 → 条目类型改成 function_call_output（call_id、
//     output 原样，.21 以前的信封归一就是这个形状，生产跑了几天没有 422）；
//   - user 消息带图 → 消息只留文本部件，紧跟一对合成的 function_call /
//     function_call_output（名字 user_attachment），图片放进输出。call_id 由消息
//     内容与序号派生，同一会话每轮逐字节一致，不破坏上游 prompt cache 前缀。
// 其余位置的图片（assistant/developer 消息、file_id 引用、chat completions 的
// messages）没实测过，资格判定阶段留在 codex 上游。
//
// 文件（PDF 等 input_file）bps 不收：file_data 放在 user 消息、function/custom
// 工具输出、合成调用对里一律 422；file_url 会被 bps 当远程地址下载。文件改写成
// 文本文档块见 openai_bps_excel_files.go。

// openAIBpsAttachmentToolName 是承载用户消息图片的合成调用名。不在客户端工具目录里，
// 模型若照着历史调用它，会按目录外名字处理（客户端回 unsupported call → 可用工具指引）。
const openAIBpsAttachmentToolName = "user_attachment"

// openAIBpsIsImagePart 报告内容部件是否是图片。
func openAIBpsIsImagePart(part map[string]any) bool {
	if part == nil {
		return false
	}
	if t, _ := part["type"].(string); strings.TrimSpace(t) == "input_image" {
		return true
	}
	_, hasURL := part["image_url"]
	return hasURL
}

// openAIBpsImagePartRelayable 报告图片部件能否原样搬进 function_call_output：
// 只接受内联/远程 URL 字符串（Codex 发的都是 data:image/...;base64），file_id 引用
// bps 无从解析。
func openAIBpsImagePartRelayable(part gjson.Result) bool {
	url := part.Get("image_url")
	return url.Type == gjson.String && strings.TrimSpace(url.String()) != ""
}

// openAIBpsImagePlacementSupported 报告请求里的图片是否都在改写能处理的位置
// （user 消息、function/custom 工具输出）。没有图片标记的请求按字节粗筛直接放行。
func openAIBpsImagePlacementSupported(body []byte) bool {
	if !bytes.Contains(body, []byte("input_image")) && !bytes.Contains(body, []byte("image_url")) {
		return true
	}
	if openAIJSONValueMayContainImageInput(gjson.GetBytes(body, "messages")) {
		return false
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		// 字符串 input 里的 "input_image" 字样只是文本。
		return true
	}
	supported := true
	input.ForEach(func(_, item gjson.Result) bool {
		itemType := strings.TrimSpace(item.Get("type").String())
		switch {
		case itemType == "function_call_output" || itemType == "custom_tool_call_output":
			supported = openAIBpsImagePartsRelayable(item.Get("output"))
		case itemType == "message" || (itemType == "" && item.Get("role").Exists()):
			content := item.Get("content")
			if !openAIJSONValueMayContainImageInput(content) {
				return true
			}
			role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
			supported = role == "user" && openAIBpsImagePartsRelayable(content)
		default:
			supported = !openAIJSONValueMayContainImageInput(item)
		}
		return supported
	})
	return supported
}

// openAIBpsImagePartsRelayable 检查部件数组里的每个图片部件都可搬运；
// 非数组（字符串输出/文本内容）没有图片部件。
func openAIBpsImagePartsRelayable(parts gjson.Result) bool {
	if !parts.IsArray() {
		return true
	}
	ok := true
	parts.ForEach(func(_, part gjson.Result) bool {
		if !part.IsObject() {
			return true
		}
		isImage := strings.TrimSpace(part.Get("type").String()) == "input_image" || part.Get("image_url").Exists()
		if isImage && !openAIBpsImagePartRelayable(part) {
			ok = false
		}
		return ok
	})
	return ok
}

// openAIBpsPartsHaveImage 报告部件数组是否含图片部件。
func openAIBpsPartsHaveImage(parts any) bool {
	list, ok := parts.([]any)
	if !ok {
		return false
	}
	for _, raw := range list {
		if part, ok := raw.(map[string]any); ok && openAIBpsIsImagePart(part) {
			return true
		}
	}
	return false
}

// openAIBpsImageSafeToolOutput 把带图的 custom_tool_call_output 改成
// function_call_output（bps 只在后者里收图）。其余条目原样返回。
func openAIBpsImageSafeToolOutput(item map[string]any) map[string]any {
	if itemType, _ := item["type"].(string); itemType != "custom_tool_call_output" {
		return item
	}
	if !openAIBpsPartsHaveImage(item["output"]) {
		return item
	}
	// 浅拷贝：只换 type，output（可能是 MB 级 base64）不必深拷贝。
	cloned := openAIBpsShallowCopy(item)
	cloned["type"] = "function_call_output"
	return cloned
}

func openAIBpsShallowCopy(item map[string]any) map[string]any {
	cloned := make(map[string]any, len(item))
	for k, v := range item {
		cloned[k] = v
	}
	return cloned
}

// openAIBpsSplitImageMessage 把带图的 user 消息拆成：只含文本部件的消息 + 合成
// user_attachment 调用对（图片在 function_call_output 里）。seq 是本请求内第几条
// 带图消息，与消息内容一起派生 call_id（同内容的两条消息也不会撞 id）。
// 消息不带图时原样返回单条。
func openAIBpsSplitImageMessage(item map[string]any, seq int) []any {
	role, _ := item["role"].(string)
	content, ok := item["content"].([]any)
	if !ok || !strings.EqualFold(strings.TrimSpace(role), "user") || !openAIBpsPartsHaveImage(content) {
		return []any{item}
	}
	kept := make([]any, 0, len(content))
	images := make([]any, 0, 1)
	for _, raw := range content {
		if part, ok := raw.(map[string]any); ok && openAIBpsIsImagePart(part) {
			images = append(images, part)
			continue
		}
		kept = append(kept, raw)
	}
	if len(kept) == 0 {
		kept = append(kept, map[string]any{
			"type": "input_text",
			"text": fmt.Sprintf("[Attached %d image(s); delivered by the %s result below.]", len(images), openAIBpsAttachmentToolName),
		})
	}
	message := openAIBpsShallowCopy(item)
	message["content"] = kept

	rendered, _ := json.Marshal(item)
	callID := "call_att_" + openAIBpsExcelSHA256(append([]byte(fmt.Sprintf("%d:", seq)), rendered...))[:24]
	return []any{
		message,
		map[string]any{
			"type":      "function_call",
			"call_id":   callID,
			"name":      openAIBpsAttachmentToolName,
			"arguments": fmt.Sprintf(`{"images":%d}`, len(images)),
		},
		map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  images,
		},
	}
}

// openAIBpsJSONHasFilePart 递归查找文件部件（只认对象 type，文本里的字样不算）。
func openAIBpsJSONHasFilePart(value gjson.Result) bool {
	found := false
	switch {
	case value.IsArray():
		value.ForEach(func(_, item gjson.Result) bool {
			found = openAIBpsJSONHasFilePart(item)
			return !found
		})
	case value.IsObject():
		switch strings.TrimSpace(value.Get("type").String()) {
		case "input_file", "file":
			return true
		}
		found = openAIBpsJSONHasFilePart(value.Get("content")) || openAIBpsJSONHasFilePart(value.Get("output"))
	}
	return found
}
