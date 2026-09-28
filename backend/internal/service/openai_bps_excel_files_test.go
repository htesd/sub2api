package service

import (
	"bytes"
	"compress/zlib"
	"container/list"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// openAIBpsTestPDF 生成单页 PDF；extraObjs 追加额外对象（图片/字体标记），flate 控制内容流压缩。
func openAIBpsTestPDF(t *testing.T, content string, flate bool, extraObjs ...string) []byte {
	t.Helper()
	stream := []byte(content)
	dict := fmt.Sprintf("<< /Length %d >>", len(stream))
	if flate {
		var b bytes.Buffer
		w := zlib.NewWriter(&b)
		_, _ = w.Write(stream)
		_ = w.Close()
		stream = b.Bytes()
		dict = fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>", len(stream))
	}
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	pdf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	pdf.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>\nendobj\n")
	pdf.WriteString("4 0 obj\n" + dict + "\nstream\n")
	pdf.Write(stream)
	pdf.WriteString("\nendstream\nendobj\n5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n")
	for i, obj := range extraObjs {
		pdf.WriteString(fmt.Sprintf("%d 0 obj\n%s\nendobj\n", 6+i, obj))
	}
	pdf.WriteString("trailer\n<< /Root 1 0 R >>\n%%EOF\n")
	return pdf.Bytes()
}

const openAIBpsTestPDFContent = "BT /F1 24 Tf 72 720 Td (Quarterly report) Tj 0 -32 Td (The secret word is PELICAN.) Tj ET"

func TestOpenAIBpsExtractPDFText(t *testing.T) {
	text, ok := openAIBpsExtractPDFText(openAIBpsTestPDF(t, openAIBpsTestPDFContent, false))
	require.True(t, ok)
	require.Contains(t, text, "Quarterly report")
	require.Contains(t, text, "The secret word is PELICAN.")
	require.Contains(t, text, "report\nThe", "Td 换行应保留为行边界")

	text, ok = openAIBpsExtractPDFText(openAIBpsTestPDF(t, openAIBpsTestPDFContent, true))
	require.True(t, ok)
	require.Contains(t, text, "PELICAN")

	// 转义、八进制、十六进制、UTF-16BE BOM、TJ 数组。
	text, ok = openAIBpsExtractPDFText(openAIBpsTestPDF(t, `BT (a\(b\)c) Tj (\101\102) Tj <414243> Tj <FEFF4F60597D> Tj [(Hel) -20 (lo)] TJ ET`, true))
	require.True(t, ok)
	for _, want := range []string{"a(b)c", "AB", "ABC", "你好", "Hello"} {
		require.Contains(t, text, want)
	}

	// 字典里的字符串不是正文；没有显示串 → 失败。
	_, ok = openAIBpsExtractPDFText(openAIBpsTestPDF(t, "/Title (should not appear) /Foo 1 0 R", false))
	require.False(t, ok)
	_, ok = openAIBpsExtractPDFText([]byte("not a pdf (x) Tj"))
	require.False(t, ok)

	// 保真：图片 XObject（扫描件/图表）、Type0/CID 字体 → 翻译会丢内容，失败。
	_, ok = openAIBpsExtractPDFText(openAIBpsTestPDF(t, openAIBpsTestPDFContent, true, "<< /Type /XObject /Subtype /Image /Width 10 /Height 10 >>"))
	require.False(t, ok)
	_, ok = openAIBpsExtractPDFText(openAIBpsTestPDF(t, openAIBpsTestPDFContent, true, "<< /Type /Font /Subtype /Type0 /Encoding /Identity-H >>"))
	require.False(t, ok)
	// 标记藏在压缩的对象流里也要识别。
	var objStm bytes.Buffer
	w := zlib.NewWriter(&objStm)
	_, _ = w.Write([]byte("<< /Type /XObject /Subtype /Image >>"))
	_ = w.Close()
	hidden := "<< /Type /ObjStm /Filter /FlateDecode >>\nstream\n" + objStm.String() + "\nendstream"
	_, ok = openAIBpsExtractPDFText(openAIBpsTestPDF(t, openAIBpsTestPDFContent, true, hidden))
	require.False(t, ok)

	// 字形码乱码（Latin-1 控制区字符为主）→ 失败。
	_, ok = openAIBpsExtractPDFText(openAIBpsTestPDF(t, `BT <0012003A0045008100920093> Tj ET`, false))
	require.False(t, ok)
}

// 解压炸弹与流数量都在扫描期封顶。
func TestOpenAIBpsPDFLimits(t *testing.T) {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	_, _ = w.Write(bytes.Repeat([]byte("A"), 40<<20))
	_ = w.Close()
	out, ok := openAIBpsInflate(b.Bytes(), 1<<30)
	require.True(t, ok)
	require.Equal(t, openAIBpsPDFMaxStreamInflate, len(out))

	var many bytes.Buffer
	many.WriteString("%PDF-1.4\n")
	for i := 0; i < 5000; i++ {
		many.WriteString("stream\nBT (x) Tj ET\nendstream\n")
	}
	require.Len(t, openAIBpsPDFStreams(many.Bytes()), openAIBpsPDFMaxStreams)
}

func openAIBpsTestFilePart(filename, mediaType string, raw []byte) string {
	return fmt.Sprintf(`{"type":"input_file","filename":%q,"file_data":"data:%s;base64,%s"}`,
		filename, mediaType, base64.StdEncoding.EncodeToString(raw))
}

func TestOpenAIBpsFileEligibility(t *testing.T) {
	account := openAIBpsExcelTestAccount(map[string]any{OpenAIBpsExcelEnabledExtraKey: true})
	c := openAIBpsExcelResponsesContext()
	eligible := func(items ...string) bool {
		return openAIBpsExcelRequestEligible(context.Background(), c, account, openAIBpsImageBody(items...))
	}
	userWith := func(part string) string {
		return `{"type":"message","role":"user","content":[{"type":"input_text","text":"read"},` + part + `]}`
	}
	call := `{"type":"function_call","call_id":"f1","name":"read","arguments":"{}"}`
	textPDF := openAIBpsTestFilePart("report.pdf", "application/pdf", openAIBpsTestPDF(t, openAIBpsTestPDFContent, true))
	scanPDF := openAIBpsTestFilePart("scan.pdf", "application/pdf", openAIBpsTestPDF(t, openAIBpsTestPDFContent, true, "<< /Subtype /Image >>"))

	// 可翻译：文字型 PDF、纯文本文件；在 user 消息或工具输出里。
	require.True(t, eligible(userWith(textPDF)))
	require.True(t, eligible(`{"role":"user","content":[`+textPDF+`]}`))
	require.True(t, eligible(call, `{"type":"function_call_output","call_id":"f1","output":[`+textPDF+`]}`))
	require.True(t, eligible(userWith(openAIBpsTestFilePart("notes.md", "text/markdown", []byte("# hi\nORCHID-7")))))
	// 媒体类型缺省时按文件头/扩展名判定。
	require.True(t, eligible(userWith(fmt.Sprintf(`{"type":"input_file","filename":"x.bin","file_data":%q}`,
		base64.StdEncoding.EncodeToString(openAIBpsTestPDF(t, openAIBpsTestPDFContent, false))))))
	// 文本里提到 input_file 字样不算文件。
	require.True(t, eligible(`{"type":"message","role":"user","content":[{"type":"input_text","text":"the input_file part and \"file\" type"}]}`))

	// 翻译不了 → codex：扫描件、docx、非 UTF-8 文本、file_id/file_url、位置不支持。
	require.False(t, eligible(userWith(scanPDF)))
	require.False(t, eligible(call, `{"type":"function_call_output","call_id":"f1","output":[`+scanPDF+`]}`))
	require.False(t, eligible(userWith(openAIBpsTestFilePart("a.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", []byte("PK\x03\x04")))))
	require.False(t, eligible(userWith(openAIBpsTestFilePart("a.txt", "text/plain", []byte{0xff, 0xfe, 0x00}))))
	require.False(t, eligible(userWith(`{"type":"input_file","file_id":"file-1"}`)))
	require.False(t, eligible(userWith(`{"type":"input_file","file_url":"https://example.com/a.pdf"}`)))
	require.False(t, eligible(`{"type":"message","role":"assistant","content":[`+textPDF+`]}`))
	require.False(t, openAIBpsExcelRequestEligible(context.Background(), c, account,
		[]byte(`{"model":"gpt-6-astra","messages":[{"role":"user","content":[{"type":"file","file":{"file_data":"data:application/pdf;base64,JVBERi0="}}]}]}`)))
}

func TestRewriteOpenAIBodyForBpsExcelTranslatesFiles(t *testing.T) {
	textPDF := openAIBpsTestFilePart("report.pdf", "application/pdf", openAIBpsTestPDF(t, openAIBpsTestPDFContent, true))
	memo := openAIBpsTestFilePart("memo.md", "text/markdown", []byte("Meeting room: ORCHID-7"))
	body := openAIBpsImageBody(
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"what does it say"},`+textPDF+`,`+openAIBpsTestImage+`]}`,
		`{"type":"function_call","call_id":"f1","name":"read","arguments":"{}"}`,
		`{"type":"function_call_output","call_id":"f1","output":[`+memo+`]}`,
		`{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"read()"}`,
		`{"type":"custom_tool_call_output","call_id":"c1","output":[`+textPDF+`]}`,
	)
	rewritten, _, err := rewriteOpenAIBodyForBpsExcel(body, "scope-files")
	require.NoError(t, err)
	require.NotContains(t, string(rewritten), "input_file")
	require.NotContains(t, string(rewritten), "application/pdf;base64")

	input := gjson.GetBytes(rewritten, "input").Array()
	var userMsg, fnOut, customOut gjson.Result
	for _, item := range input {
		switch {
		case item.Get("role").String() == "user":
			userMsg = item
		case item.Get("call_id").String() == "f1" && item.Get("output").Exists():
			fnOut = item
		case item.Get("call_id").String() == "c1" && item.Get("output").Exists():
			customOut = item
		}
	}
	// user 消息：文本 + 文档块留在消息里，图片仍按规则拆进合成调用对。
	require.Equal(t, "what does it say", userMsg.Get("content.0.text").String())
	doc := userMsg.Get("content.1.text").String()
	require.True(t, strings.HasPrefix(doc, `<document filename="report.pdf" media_type="application/pdf"`), doc)
	require.Contains(t, doc, "The secret word is PELICAN.")
	require.Len(t, userMsg.Get("content").Array(), 2)
	require.Contains(t, string(rewritten), openAIBpsAttachmentToolName)
	// 工具输出里的文件同样翻译；custom 输出不带图就保持 custom 类型。
	require.Contains(t, fnOut.Get("output.0.text").String(), "ORCHID-7")
	require.Equal(t, "custom_tool_call_output", customOut.Get("type").String())
	require.Contains(t, customOut.Get("output.0.text").String(), "PELICAN")

	// 逐字节稳定（prompt cache 前缀不断）。
	again, _, err := rewriteOpenAIBodyForBpsExcel(body, "scope-files")
	require.NoError(t, err)
	require.Equal(t, gjson.GetBytes(rewritten, "input").Raw, gjson.GetBytes(again, "input").Raw)
}

// 缓存按总字节有界：超额按 LRU 淘汰，命中不重算。
func TestOpenAIBpsFileDocumentCacheBounded(t *testing.T) {
	cache := &openAIBpsFileDocumentCache{entries: map[[32]byte]*list.Element{}, order: list.New()}
	big := strings.Repeat("x", 900<<10) // 单条 < 1MB 文本上限
	for i := 0; i < 60; i++ {
		data := "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d%s", i, big)))
		_, ok := cache.translate("f.txt", data)
		require.True(t, ok)
	}
	require.LessOrEqual(t, cache.bytes, openAIBpsFileCacheMaxBytes)
	require.Less(t, cache.order.Len(), 60)
	require.Equal(t, cache.order.Len(), len(cache.entries))

	data := "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("hello"))
	first, ok := cache.translate("a.txt", data)
	require.True(t, ok)
	n := cache.order.Len()
	second, ok := cache.translate("a.txt", data)
	require.True(t, ok)
	require.Equal(t, first, second)
	require.Equal(t, n, cache.order.Len())
}
