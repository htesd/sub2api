package service

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// bps 请求被上游拒收（400/422 请求体校验失败）时的同账号回落与诊断。
//
// 背景：antistorm.22 上线后，生产 Codex 流量在 bps 上出现大量
// "422: Invalid request body."（下游表现为 502 Upstream request failed）。
// 请求体校验失败与账号无关，旧逻辑既不 failover 也不回落，客户端每次重试都
// 撞同一堵墙。现在：同一请求只改道一次 bps，被拒收就在同账号上改走 codex
// 上游重发（codex 上游本来就能处理这类请求，只是没有 bps 的好处）。

const (
	// openAIBpsExcelRejectedContextKey 标记本请求的 bps 改写已被上游拒收，后续
	// 尝试（含同账号重发与 failover 换号）一律不再改道。
	openAIBpsExcelRejectedContextKey = "openai_bps_excel_rejected"
	// openAIBpsExcelBodyContextKey 保存本次尝试发往 bps 的请求体，供拒收诊断。
	openAIBpsExcelBodyContextKey = "openai_bps_excel_body"

	// openAIBpsRejectDumpDir 存在时才落盘被拒请求体（手动 mkdir 开启、删目录关闭）；
	// 请求体含用户对话内容，文件 0600，最多保留 openAIBpsRejectDumpLimit 个。
	openAIBpsRejectDumpDir   = "/app/data/bps-reject"
	openAIBpsRejectDumpLimit = 40
)

var openAIBpsRejectDumpCount atomic.Int64

func openAIBpsExcelRejected(c *gin.Context) bool {
	return c != nil && c.GetBool(openAIBpsExcelRejectedContextKey)
}

// openAIBpsExcelRejectStatus 报告上游状态码是否属于"请求体被拒收"：
// 这类错误换号也没用，应回落 codex 上游。鉴权/限流/5xx 仍走原有 failover。
func openAIBpsExcelRejectStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusUnprocessableEntity
}

// markOpenAIBpsExcelRejected 在 bps 尝试被拒收后调用：记诊断、置标记。
// 返回 true 表示调用方应在同账号上重发（本次尝试走了 bps 且是首次拒收）。
func markOpenAIBpsExcelRejected(c *gin.Context, account *Account, status int, respBody []byte) bool {
	if c == nil || !openAIBpsExcelUsed(c) || openAIBpsExcelRejected(c) || !openAIBpsExcelRejectStatus(status) {
		return false
	}
	c.Set(openAIBpsExcelRejectedContextKey, true)
	var sent []byte
	if v, ok := c.Get(openAIBpsExcelBodyContextKey); ok {
		sent, _ = v.([]byte)
	}
	accountName := ""
	if account != nil {
		accountName = account.Name
	}
	message := strings.TrimSpace(gjson.GetBytes(respBody, "error.message").String())
	logger.LegacyPrintf("service.openai_gateway",
		"[bps] upstream rejected request, fallback to codex upstream on same account (account: %s status=%d msg=%q): %s",
		accountName, status, truncateString(message, 300), openAIBpsBodyShapeSummary(sent))
	openAIBpsDumpRejectedBody(sent, status, message)
	return true
}

// openAIBpsBodyShapeSummary 只描述请求体结构，不含任何内容：顶层键、input 条目
// 类型序列（连续同类合并计数）与每类出现过的键、工具声明的类型/键。
func openAIBpsBodyShapeSummary(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return "body=unavailable"
	}
	var top []string
	gjson.ParseBytes(body).ForEach(func(key, _ gjson.Result) bool {
		top = append(top, key.String())
		return true
	})
	var seq []string
	lastType, run := "", 0
	flush := func() {
		if lastType != "" {
			seq = append(seq, fmt.Sprintf("%s*%d", lastType, run))
		}
	}
	keysByType := map[string]map[string]bool{}
	toolShapes := map[string]bool{}
	var walkTools func(tools gjson.Result, prefix string)
	walkTools = func(tools gjson.Result, prefix string) {
		tools.ForEach(func(_, tool gjson.Result) bool {
			var keys []string
			tool.ForEach(func(k, _ gjson.Result) bool {
				keys = append(keys, k.String())
				return true
			})
			sort.Strings(keys)
			toolShapes[prefix+tool.Get("type").String()+"{"+strings.Join(keys, ",")+"}"] = true
			if nested := tool.Get("tools"); nested.IsArray() {
				walkTools(nested, prefix+"  ")
			}
			return true
		})
	}
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		itemType := item.Get("type").String()
		if itemType == "" {
			itemType = "(none)"
		}
		if role := item.Get("role").String(); role != "" && itemType == "message" {
			itemType = "message:" + role
		}
		if itemType == lastType {
			run++
		} else {
			flush()
			lastType, run = itemType, 1
		}
		set := keysByType[itemType]
		if set == nil {
			set = map[string]bool{}
			keysByType[itemType] = set
		}
		item.ForEach(func(k, v gjson.Result) bool {
			set[k.String()] = true
			if k.String() == "content" && v.IsArray() {
				v.ForEach(func(_, part gjson.Result) bool {
					set["content[]."+part.Get("type").String()] = true
					return true
				})
			}
			return true
		})
		if itemType == "additional_tools" {
			walkTools(item.Get("tools"), "")
		}
		return true
	})
	flush()
	types := make([]string, 0, len(keysByType))
	for t := range keysByType {
		types = append(types, t)
	}
	sort.Strings(types)
	var keyDesc []string
	for _, t := range types {
		keys := make([]string, 0, len(keysByType[t]))
		for k := range keysByType[t] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		keyDesc = append(keyDesc, t+"{"+strings.Join(keys, ",")+"}")
	}
	shapes := make([]string, 0, len(toolShapes))
	for s := range toolShapes {
		shapes = append(shapes, s)
	}
	sort.Strings(shapes)
	return fmt.Sprintf("bytes=%d top=[%s] seq=[%s] keys=[%s] tools=[%s]",
		len(body), strings.Join(top, ","), strings.Join(seq, " "), strings.Join(keyDesc, " "), strings.Join(shapes, " "))
}

func openAIBpsDumpRejectedBody(body []byte, status int, message string) {
	if len(body) == 0 {
		return
	}
	if info, err := os.Stat(openAIBpsRejectDumpDir); err != nil || !info.IsDir() {
		return
	}
	n := openAIBpsRejectDumpCount.Add(1)
	if n > openAIBpsRejectDumpLimit {
		return
	}
	name := filepath.Join(openAIBpsRejectDumpDir, fmt.Sprintf("%s-%03d-%d.json", time.Now().Format("20060102T150405"), n, status))
	payload := append([]byte(fmt.Sprintf("// %s\n", strings.ReplaceAll(message, "\n", " "))), body...)
	if err := os.WriteFile(name, payload, 0o600); err != nil {
		logger.LegacyPrintf("service.openai_gateway", "[bps] reject dump failed: %v", err)
	}
}
