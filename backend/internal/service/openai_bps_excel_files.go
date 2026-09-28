package service

import (
	"bytes"
	"compress/zlib"
	"container/list"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// bps 文件翻译（上行）：bps 在任何位置都不收 input_file（2026-09-28 直连实测
// 422），codex 上游原生支持 PDF。能无损翻译成文本的文件在请求侧改写成
// input_text 文档块发 bps；翻译不了、或翻译会丢内容的文件，整个请求留 codex。
//
// 下行不需要反向翻译：文档块只存在于发给 bps 的历史里，模型输出不引用它，
// 客户端下一轮照旧回带原始 input_file，网关每轮重新翻译（结果按内容哈希缓存，
// 逐字节一致，上游 prompt cache 前缀不断）。
//
// PDF 文本层抽取移植自 claude-all-in-one gw-cursor/src/pdf.rs（零新依赖：只解
// FlateDecode 内容流、取 Tj/TJ/'/" 显示串）。在它基础上多了"保真"判定——这里有
// 原生支持 PDF 的 codex 可回落，不必像 cursor 那样硬塞半截内容：
//   - 含图片 XObject（扫描件、图表、截图）→ 文本层丢视觉内容，留 codex；
//   - Type0/CID 字体（中文 Word 导出最常见）→ 字形编码抽出来是乱码，留 codex；
//   - 抽出文本可读字符占比过低 → 同上。

const (
	openAIBpsFileMaxDecodedBytes = 32 << 20 // 单文件解码后上限，超了留 codex
	openAIBpsFileMaxTextBytes    = 1 << 20  // 翻译后文本上限（约 25 万 token），超了留 codex
	openAIBpsPDFMaxStreams       = 512
	openAIBpsPDFMaxStreamInflate = 16 << 20
	openAIBpsPDFMaxTotalInflate  = 64 << 20
	openAIBpsFileCacheMaxBytes   = 32 << 20
)

// openAIBpsIsFilePart 报告内容部件是否是文件（Responses input_file / chat file）。
func openAIBpsIsFilePart(part map[string]any) bool {
	t, _ := part["type"].(string)
	t = strings.TrimSpace(t)
	return t == "input_file" || t == "file"
}

// openAIBpsFilePartText 把文件部件翻译成文档文本。ok=false 表示翻译不了或会丢内容。
func openAIBpsFilePartText(part gjson.Result) (string, bool) {
	if strings.TrimSpace(part.Get("type").String()) != "input_file" {
		return "", false // chat completions 的 file 部件不在 bps 入站路径上
	}
	if part.Get("file_id").Exists() || part.Get("file_url").Exists() {
		return "", false
	}
	data := part.Get("file_data")
	if data.Type != gjson.String || strings.TrimSpace(data.String()) == "" {
		return "", false
	}
	filename := strings.TrimSpace(part.Get("filename").String())
	return openAIBpsFileDocuments.translate(filename, data.String())
}

// openAIBpsDocumentBlock 生成注入给模型的文档块。
func openAIBpsDocumentBlock(filename, mediaType, text string) string {
	if filename == "" {
		filename = "attachment"
	}
	filename = strings.NewReplacer(`"`, "'", "\n", " ", "\r", " ").Replace(filename)
	return fmt.Sprintf("<document filename=%q media_type=%q note=\"text layer extracted by the gateway\">\n%s\n</document>", filename, mediaType, text)
}

// openAIBpsDecodeFileData 解析 file_data（data URL 或裸 base64），返回媒体类型与字节。
func openAIBpsDecodeFileData(filename, fileData string) (string, []byte, bool) {
	mediaType := ""
	payload := strings.TrimSpace(fileData)
	if strings.HasPrefix(payload, "data:") {
		comma := strings.IndexByte(payload, ',')
		if comma < 0 {
			return "", nil, false
		}
		meta := payload[len("data:"):comma]
		if !strings.HasSuffix(meta, ";base64") {
			return "", nil, false
		}
		mediaType = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(meta, ";base64")))
		payload = payload[comma+1:]
	}
	if base64.StdEncoding.DecodedLen(len(payload)) > openAIBpsFileMaxDecodedBytes+3 {
		return "", nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(payload); err != nil {
			return "", nil, false
		}
	}
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType = openAIBpsMediaTypeFromName(filename, raw)
	}
	return mediaType, raw, true
}

func openAIBpsMediaTypeFromName(filename string, raw []byte) string {
	if bytes.HasPrefix(raw, []byte("%PDF-")) {
		return "application/pdf"
	}
	switch strings.ToLower(path.Ext(filename)) {
	case ".pdf":
		return "application/pdf"
	case ".txt", ".log":
		return "text/plain"
	case ".md", ".markdown":
		return "text/markdown"
	case ".csv":
		return "text/csv"
	case ".json":
		return "application/json"
	case ".xml":
		return "application/xml"
	case ".html", ".htm":
		return "text/html"
	case ".yaml", ".yml":
		return "application/yaml"
	}
	return "application/octet-stream"
}

func openAIBpsIsTextMediaType(mediaType string) bool {
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml":
		return true
	}
	return false
}

// openAIBpsTranslateFileBytes 是不带缓存的翻译本体。
func openAIBpsTranslateFileBytes(filename, mediaType string, raw []byte) (string, bool) {
	var text string
	switch {
	case mediaType == "application/pdf":
		extracted, ok := openAIBpsExtractPDFText(raw)
		if !ok {
			return "", false
		}
		text = extracted
	case openAIBpsIsTextMediaType(mediaType):
		if !utf8.Valid(raw) {
			return "", false
		}
		text = strings.TrimSpace(string(raw))
		if text == "" {
			text = "(empty file)"
		}
	default:
		// docx/xlsx/图片型文件等：没有无损文本形态，留 codex。
		return "", false
	}
	if len(text) > openAIBpsFileMaxTextBytes {
		return "", false
	}
	return openAIBpsDocumentBlock(filename, mediaType, text), true
}

// openAIBpsFileDocumentCache 按 (文件名, file_data) 哈希缓存翻译结果：长会话里
// 同一份 PDF 每轮都随历史回带，资格判定与改写各要一次，不重复解压抽取。
// 失败结果也缓存（只占一个条目）。总字节有界，LRU 淘汰。
type openAIBpsFileDocumentCache struct {
	mu      sync.Mutex
	entries map[[32]byte]*list.Element
	order   *list.List
	bytes   int
}

type openAIBpsFileDocumentEntry struct {
	key  [32]byte
	text string
	ok   bool
}

var openAIBpsFileDocuments = &openAIBpsFileDocumentCache{
	entries: make(map[[32]byte]*list.Element),
	order:   list.New(),
}

func (c *openAIBpsFileDocumentCache) translate(filename, fileData string) (string, bool) {
	h := sha256.New()
	_, _ = io.WriteString(h, filename)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, fileData)
	var key [32]byte
	copy(key[:], h.Sum(nil))

	c.mu.Lock()
	if el, hit := c.entries[key]; hit {
		c.order.MoveToFront(el)
		entry := el.Value.(*openAIBpsFileDocumentEntry)
		c.mu.Unlock()
		return entry.text, entry.ok
	}
	c.mu.Unlock()

	text, ok := "", false
	if mediaType, raw, decoded := openAIBpsDecodeFileData(filename, fileData); decoded {
		text, ok = openAIBpsTranslateFileBytes(filename, mediaType, raw)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, hit := c.entries[key]; !hit {
		c.entries[key] = c.order.PushFront(&openAIBpsFileDocumentEntry{key: key, text: text, ok: ok})
		c.bytes += len(text)
		for c.bytes > openAIBpsFileCacheMaxBytes && c.order.Len() > 1 {
			oldest := c.order.Back()
			entry := oldest.Value.(*openAIBpsFileDocumentEntry)
			c.order.Remove(oldest)
			delete(c.entries, entry.key)
			c.bytes -= len(entry.text)
		}
	}
	return text, ok
}

// openAIBpsFilePartsTranslatable 检查部件数组里的每个文件部件都能翻译。
func openAIBpsFilePartsTranslatable(parts gjson.Result) bool {
	if !parts.IsArray() {
		return true
	}
	ok := true
	parts.ForEach(func(_, part gjson.Result) bool {
		t := strings.TrimSpace(part.Get("type").String())
		if t == "input_file" || t == "file" {
			_, ok = openAIBpsFilePartText(part)
		}
		return ok
	})
	return ok
}

// openAIBpsFilePlacementSupported 报告请求里的文件是否都在可翻译位置（user 消息、
// function/custom 工具输出）且都能无损翻译。没有文件标记的请求按字节粗筛放行。
func openAIBpsFilePlacementSupported(body []byte) bool {
	if !bytes.Contains(body, []byte("input_file")) && !bytes.Contains(body, []byte(`"file"`)) {
		return true
	}
	if openAIBpsJSONHasFilePart(gjson.GetBytes(body, "messages")) {
		return false
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return true
	}
	supported := true
	input.ForEach(func(_, item gjson.Result) bool {
		itemType := strings.TrimSpace(item.Get("type").String())
		switch {
		case itemType == "function_call_output" || itemType == "custom_tool_call_output":
			supported = openAIBpsFilePartsTranslatable(item.Get("output"))
		case itemType == "message" || (itemType == "" && item.Get("role").Exists()):
			content := item.Get("content")
			if !openAIBpsJSONHasFilePart(content) {
				return true
			}
			role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
			supported = role == "user" && openAIBpsFilePartsTranslatable(content)
		default:
			supported = !openAIBpsJSONHasFilePart(item)
		}
		return supported
	})
	return supported
}

// openAIBpsTranslateFileParts 把部件数组里的文件部件换成 input_text 文档块。
// 资格判定已保证都能翻译；万一翻译失败（缓存被淘汰后重算不一致不可能，但防御）
// 留原部件，bps 会 422，由 .23 的拒收回落兜住。未变化时返回原切片。
func openAIBpsTranslateFileParts(parts any) (any, bool) {
	list, ok := parts.([]any)
	if !ok {
		return parts, false
	}
	var out []any
	for i, raw := range list {
		part, ok := raw.(map[string]any)
		if !ok || !openAIBpsIsFilePart(part) {
			if out != nil {
				out = append(out, raw)
			}
			continue
		}
		filename, _ := part["filename"].(string)
		fileData, _ := part["file_data"].(string)
		_, hasID := part["file_id"]
		_, hasURL := part["file_url"]
		text, translated := "", false
		if t, _ := part["type"].(string); strings.TrimSpace(t) == "input_file" && !hasID && !hasURL && fileData != "" {
			text, translated = openAIBpsFileDocuments.translate(strings.TrimSpace(filename), fileData)
		}
		if out == nil {
			out = make([]any, 0, len(list))
			out = append(out, list[:i]...)
		}
		if translated {
			out = append(out, map[string]any{"type": "input_text", "text": text})
		} else {
			out = append(out, raw)
		}
	}
	if out == nil {
		return parts, false
	}
	return out, true
}

// openAIBpsTranslateItemFiles 翻译 user 消息 content / 工具输出 output 里的文件部件。
func openAIBpsTranslateItemFiles(item map[string]any) map[string]any {
	key := "content"
	switch t, _ := item["type"].(string); t {
	case "function_call_output", "custom_tool_call_output":
		key = "output"
	}
	translated, changed := openAIBpsTranslateFileParts(item[key])
	if !changed {
		return item
	}
	cloned := openAIBpsShallowCopy(item)
	cloned[key] = translated
	return cloned
}

// ---- PDF 文本层抽取（移植自 gw-cursor/src/pdf.rs，外加保真判定） ----

// openAIBpsExtractPDFText 抽取 PDF 文本层；ok=false 表示抽不到、或抽出来会丢内容/乱码。
func openAIBpsExtractPDFText(pdf []byte) (string, bool) {
	if !bytes.HasPrefix(bytes.TrimLeft(pdf, " \t\r\n"), []byte("%PDF-")) {
		return "", false
	}
	if openAIBpsPDFLosesContent(pdf) {
		return "", false
	}
	var out strings.Builder
	inflatedTotal := 0
	for _, stream := range openAIBpsPDFStreams(pdf) {
		if out.Len() > openAIBpsFileMaxTextBytes {
			return "", false
		}
		data := stream.body
		if stream.flate {
			remain := openAIBpsPDFMaxTotalInflate - inflatedTotal
			if remain <= 0 {
				return "", false
			}
			inflated, ok := openAIBpsInflate(stream.body, remain)
			if !ok {
				continue
			}
			data = inflated
		}
		inflatedTotal += len(data)
		// 对象流（/Type /ObjStm）压缩了对象字典，保真判定要在解压后再看一次。
		if openAIBpsPDFLosesContent(data) {
			return "", false
		}
		openAIBpsCollectShownText(data, &out)
	}
	raw := out.String()
	// 可读性在压空白/去控制字符之前判定：字形码乱码的特征正是大量控制字符。
	if !openAIBpsMostlyReadable(raw) {
		return "", false
	}
	text := openAIBpsSqueezeText(raw)
	if utf8.RuneCountInString(text) < 2 {
		return "", false
	}
	return text, true
}

// openAIBpsPDFLosesContent 报告 PDF 片段里是否有文本层表达不了的内容：
// 图片 XObject（扫描件/图表/截图）或 Type0/CID 字体（字形编码，抽出来是乱码）。
func openAIBpsPDFLosesContent(data []byte) bool {
	for _, marker := range [][]byte{
		[]byte("/Subtype /Image"), []byte("/Subtype/Image"),
		[]byte("/Subtype /Type0"), []byte("/Subtype/Type0"),
		[]byte("/Identity-H"), []byte("/Identity-V"),
	} {
		if bytes.Contains(data, marker) {
			return true
		}
	}
	return false
}

type openAIBpsPDFStream struct {
	body  []byte
	flate bool
}

// openAIBpsPDFStreams 切出所有 stream…endstream 段（扫描期封顶 512 个），
// 记录其前方字典是否声明 /FlateDecode。
func openAIBpsPDFStreams(pdf []byte) []openAIBpsPDFStream {
	var out []openAIBpsPDFStream
	i := 0
	for len(out) < openAIBpsPDFMaxStreams {
		rel := bytes.Index(pdf[i:], []byte("stream"))
		if rel < 0 {
			break
		}
		kw := i + rel
		back := kw - 400
		if back < 0 {
			back = 0
		}
		flate := bytes.Contains(pdf[back:kw], []byte("/FlateDecode"))
		s := kw + len("stream")
		if s < len(pdf) && pdf[s] == '\r' {
			s++
		}
		if s < len(pdf) && pdf[s] == '\n' {
			s++
		}
		erel := bytes.Index(pdf[s:], []byte("endstream"))
		if erel < 0 {
			break
		}
		e := s + erel
		out = append(out, openAIBpsPDFStream{body: pdf[s:e], flate: flate})
		i = e + len("endstream")
	}
	return out
}

// openAIBpsInflate 解压单条流，上限取单流 16MB 与剩余总量预算的较小值。
func openAIBpsInflate(data []byte, remain int) ([]byte, bool) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	defer func() { _ = r.Close() }()
	limit := openAIBpsPDFMaxStreamInflate
	if remain < limit {
		limit = remain
	}
	out, _ := io.ReadAll(io.LimitReader(r, int64(limit)))
	// 截断/尾部损坏的流仍保留已解出的部分（与 pdf.rs 的 read_to_end 容错一致）。
	return out, len(out) > 0
}

// openAIBpsCollectShownText 取显示操作符（Tj/TJ/'/"）前的字符串实参。
func openAIBpsCollectShownText(data []byte, out *strings.Builder) {
	var pending []string
	i := 0
	for i < len(data) {
		c := data[i]
		switch {
		case c == '(':
			s, next := openAIBpsReadPDFLiteral(data, i+1)
			pending = append(pending, s)
			i = next
		case c == '<' && i+1 < len(data) && data[i+1] != '<':
			s, next := openAIBpsReadPDFHex(data, i+1)
			pending = append(pending, s)
			i = next
		case c == 'T' && i+1 < len(data) && (data[i+1] == 'j' || data[i+1] == 'J'):
			for _, s := range pending {
				out.WriteString(s)
			}
			pending = pending[:0]
			out.WriteByte(' ')
			i += 2
		case c == '\'' || c == '"':
			for _, s := range pending {
				out.WriteString(s)
			}
			pending = pending[:0]
			out.WriteByte('\n')
			i++
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'):
			// 换行类操作符（Td/TD/T*/ET）视作行边界，其余操作符丢弃之前的串。
			start := i
			for i < len(data) && ((data[i] >= 'A' && data[i] <= 'Z') || (data[i] >= 'a' && data[i] <= 'z')) {
				i++
			}
			switch string(data[start:i]) {
			case "Td", "TD", "ET":
				out.WriteByte('\n')
			}
			pending = pending[:0]
		default:
			i++
		}
	}
}

func openAIBpsReadPDFLiteral(data []byte, i int) (string, int) {
	var buf []byte
	depth := 1
	for i < len(data) {
		c := data[i]
		switch c {
		case '\\':
			i++
			if i >= len(data) {
				return openAIBpsDecodePDFString(buf), i
			}
			switch e := data[i]; e {
			case 'n':
				buf = append(buf, '\n')
			case 'r':
				buf = append(buf, '\r')
			case 't':
				buf = append(buf, '\t')
			case '(', ')', '\\':
				buf = append(buf, e)
			default:
				if e >= '0' && e <= '7' {
					v, n := 0, 0
					for n < 3 && i < len(data) && data[i] >= '0' && data[i] <= '7' {
						v = v*8 + int(data[i]-'0')
						i++
						n++
					}
					i--
					buf = append(buf, byte(v&0xFF))
				} else {
					buf = append(buf, e)
				}
			}
			i++
		case '(':
			depth++
			buf = append(buf, '(')
			i++
		case ')':
			depth--
			i++
			if depth == 0 {
				return openAIBpsDecodePDFString(buf), i
			}
			buf = append(buf, ')')
		default:
			buf = append(buf, c)
			i++
		}
	}
	return openAIBpsDecodePDFString(buf), i
}

func openAIBpsReadPDFHex(data []byte, i int) (string, int) {
	var nibbles []byte
	for i < len(data) && data[i] != '>' {
		c := data[i]
		switch {
		case c >= '0' && c <= '9':
			nibbles = append(nibbles, c-'0')
		case c >= 'a' && c <= 'f':
			nibbles = append(nibbles, c-'a'+10)
		case c >= 'A' && c <= 'F':
			nibbles = append(nibbles, c-'A'+10)
		}
		i++
	}
	raw := make([]byte, 0, (len(nibbles)+1)/2)
	for j := 0; j < len(nibbles); j += 2 {
		b := nibbles[j] << 4
		if j+1 < len(nibbles) {
			b |= nibbles[j+1]
		}
		raw = append(raw, b)
	}
	if i < len(data) {
		i++
	}
	return openAIBpsDecodePDFString(raw), i
}

// openAIBpsDecodePDFString：UTF-16BE BOM 按 UTF-16 解，合法 UTF-8 按 UTF-8，否则 Latin-1。
func openAIBpsDecodePDFString(b []byte) string {
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		units := make([]uint16, 0, (len(b)-2)/2)
		for j := 2; j+1 < len(b); j += 2 {
			units = append(units, uint16(b[j])<<8|uint16(b[j+1]))
		}
		return string(utf16.Decode(units))
	}
	if utf8.Valid(b) {
		return string(b)
	}
	runes := make([]rune, len(b))
	for j, c := range b {
		runes[j] = rune(c)
	}
	return string(runes)
}

// openAIBpsSqueezeText 压连续空白（保留换行）、去控制字符。
func openAIBpsSqueezeText(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	lastSpace, pendingNewline := true, false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			if r == '\n' {
				pendingNewline = true
			}
			lastSpace = true
		case unicode.IsControl(r):
		default:
			if lastSpace && out.Len() > 0 {
				if pendingNewline {
					out.WriteByte('\n')
				} else {
					out.WriteByte(' ')
				}
			}
			out.WriteRune(r)
			lastSpace, pendingNewline = false, false
		}
	}
	return out.String()
}

// openAIBpsMostlyReadable：非空白字符里控制字符/替换符占比 ≤ 10%，否则多半是
// 字形码（CID）按单字节解出来的乱码。
func openAIBpsMostlyReadable(text string) bool {
	total, bad := 0, 0
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if unicode.IsControl(r) || r == utf8.RuneError || r == 0xFFFD {
			bad++
		}
	}
	return total > 0 && bad*10 <= total
}
