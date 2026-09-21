package openai

import (
	"strings"

	"github.com/tidwall/gjson"
)

// CompletedResponseEvidence 只接受完整的成功响应；done 本身仅代表结束。
// 显式非成功状态优先于事件名，用于自动迁移的完整响应验证。
func CompletedResponseEvidence(payload []byte, eventType string) bool {
	if !gjson.ValidBytes(payload) {
		return false
	}
	status := ""
	for _, field := range gjson.GetManyBytes(payload, "response.status", "status") {
		if !field.Exists() {
			continue
		}
		if field.Type != gjson.String || field.String() != "completed" {
			return false
		}
		status = field.String()
	}
	switch strings.TrimSpace(eventType) {
	case "response.completed":
		return true
	case "response.done", "":
		return status == "completed"
	default:
		return false
	}
}

// ConflictingResponseModels 同一消息内两处合法模型声明冲突也必须计入unknown。
func ConflictingResponseModels(payload []byte) bool {
	fields := gjson.GetManyBytes(payload, "response.model", "model")
	if fields[0].Type != gjson.String || fields[1].Type != gjson.String {
		return false
	}
	a, b := strings.TrimSpace(fields[0].String()), strings.TrimSpace(fields[1].String())
	return a != "" && b != "" && !strings.EqualFold(a, b)
}
