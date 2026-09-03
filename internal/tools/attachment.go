package tools

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"olt-diagnostic-agent/internal/domain"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// maxTextInjectionBytes 是单个文本类附件注入到 goal 的最大字节数。
// 超过时保留头部 + 尾部各一半，中间用省略标记连接。
const maxTextInjectionBytes = 32 * 1024 // 32KB

// maxTotalInjectionBytes 是所有附件注入文本的总上限，防止 goal 过长撑爆上下文。
const maxTotalInjectionBytes = 128 * 1024 // 128KB

// ProcessAttachments 将附件列表转换成可注入到 diagnostic goal 的文本块。
// 文本类（text/*, json, csv, xml, yaml）和 PDF 会提取内容注入；
// 图片和其他二进制类型只记录文件名提示。
// 返回拼接好的注入文本（不含原始 goal），由调用方负责拼到 goal 前面。
func ProcessAttachments(attachments []domain.Attachment) (string, error) {
	if len(attachments) == 0 {
		return "", nil
	}

	var blocks []string
	totalInjected := 0

	for _, att := range attachments {
		// 解码 base64
		raw, err := base64.StdEncoding.DecodeString(att.Data)
		if err != nil {
			blocks = append(blocks, formatBinaryBlock(att.Filename, att.MimeType, int64(len(att.Data)), "base64 解码失败"))
			continue
		}

		originalSize := len(raw)
		var injection string

		switch {
		case isTextType(att.MimeType, att.Filename):
			injection = processText(att.Filename, raw)
		case isPDFType(att.MimeType, att.Filename):
			injection = processPDF(att.Filename, raw)
		case isZipType(att.MimeType, att.Filename):
			injection = processZip(att.Filename, raw)
		case isImageType(att.MimeType):
			injection = formatImageBlock(att.Filename, att.MimeType, int64(originalSize))
		default:
			injection = formatBinaryBlock(att.Filename, att.MimeType, int64(originalSize), "")
		}

		// 检查总注入量是否超限
		if totalInjected+len(injection) > maxTotalInjectionBytes {
			remaining := maxTotalInjectionBytes - totalInjected
			if remaining > 0 {
				injection = truncateText(injection, remaining)
			} else {
				blocks = append(blocks, fmt.Sprintf("[用户上传文件: %s]\n（总注入量已达上限，已跳略）\n[文件结束]", att.Filename))
				break
			}
		}

		blocks = append(blocks, injection)
		totalInjected += len(injection)
	}

	return strings.Join(blocks, "\n\n"), nil
}

// isTextType 判断是否为可直接读取的文本类型。
// 同时检查 MIME 类型和文件扩展名，因为浏览器可能对某些文件返回通用 MIME。
func isTextType(mimeType, filename string) bool {
	mt := strings.ToLower(strings.TrimSpace(mimeType))
	// 常见文本 MIME
	switch mt {
	case "text/plain", "text/csv", "text/xml", "text/yaml", "text/x-yaml",
		"application/json", "application/xml", "application/yaml",
		"application/x-yaml", "application/x-config",
		"text/markdown", "text/x-log", "text/html":
		return true
	}
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	// 按扩展名补充判断
	ext := strings.ToLower(filenameExt(filename))
	switch ext {
	case ".log", ".txt", ".json", ".csv", ".xml", ".yaml", ".yml",
		".conf", ".cfg", ".ini", ".properties", ".md", ".tsv", ".env":
		return true
	}
	return false
}

// isPDFType 判断是否为 PDF 文件。
func isPDFType(mimeType, filename string) bool {
	mt := strings.ToLower(strings.TrimSpace(mimeType))
	if mt == "application/pdf" {
		return true
	}
	return strings.ToLower(filenameExt(filename)) == ".pdf"
}

// isImageType 判断是否为图片文件。
func isImageType(mimeType string) bool {
	mt := strings.ToLower(strings.TrimSpace(mimeType))
	return strings.HasPrefix(mt, "image/")
}

// IsImageType 导出 isImageType，供 app.go 调用。
func IsImageType(mimeType string) bool { return isImageType(mimeType) }

// isZipType 判断是否为 zip 压缩包。
func isZipType(mimeType, filename string) bool {
	mt := strings.ToLower(strings.TrimSpace(mimeType))
	switch mt {
	case "application/zip", "application/x-zip", "application/x-zip-compressed":
		return true
	}
	return strings.ToLower(filenameExt(filename)) == ".zip"
}

// maxZipEntries 限制单个 zip 最多解压处理的文件数，防止 zip bomb。
const maxZipEntries = 50

// maxZipEntrySize 限制单个解压文件的最大字节数，防止解压出超大文件撑爆内存。
const maxZipEntrySize = 1024 * 1024 // 1MB

// processZip 解压 zip 压缩包，遍历内部文件按类型处理（文本/PDF 提取内容，图片/二进制记文件名）。
// 拼装成带层级的注入块，格式如：
//
//	[用户上传压缩包: logs.zip]
//	[内部文件: error.log]
//	<文本内容>
//	[内部文件结束]
//	[压缩包结束，共 N 个文件]
func processZip(filename string, raw []byte) string {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return formatBinaryBlock(filename, "application/zip", int64(len(raw)), "zip 解析失败: "+err.Error())
	}

	var blocks []string
	processed := 0
	for _, file := range reader.File {
		if processed >= maxZipEntries {
			blocks = append(blocks, fmt.Sprintf("...（已达到最大解压文件数 %d，剩余文件已跳略）...", maxZipEntries))
			break
		}
		// 跳过目录
		if file.FileInfo().IsDir() {
			continue
		}
		// 限制单个文件大小，防止 zip bomb
		if file.UncompressedSize64 > maxZipEntrySize {
			blocks = append(blocks, fmt.Sprintf("[内部文件: %s]\n（解压后 %s，超过单文件上限 %s，已跳略）\n[内部文件结束]",
				file.Name, humanSize(int64(file.UncompressedSize64)), humanSize(maxZipEntrySize)))
			processed++
			continue
		}

		rc, err := file.Open()
		if err != nil {
			blocks = append(blocks, fmt.Sprintf("[内部文件: %s]\n（读取失败: %s）\n[内部文件结束]", file.Name, err.Error()))
			processed++
			continue
		}
		content, err := io.ReadAll(io.LimitReader(rc, maxZipEntrySize+1))
		rc.Close()
		if err != nil {
			blocks = append(blocks, fmt.Sprintf("[内部文件: %s]\n（读取失败: %s）\n[内部文件结束]", file.Name, err.Error()))
			processed++
			continue
		}

		// 按内部文件的扩展名判断类型并处理
		var entryBlock string
		switch {
		case isTextType("", file.Name):
			entryBlock = processZipEntryText(file.Name, content)
		case isPDFType("", file.Name):
			entryBlock = processZipEntryPDF(file.Name, content)
		case isImageType(fileNameMIME(file.Name)):
			entryBlock = fmt.Sprintf("[内部文件: %s]\n（图片，大小 %s）\n[内部文件结束]", file.Name, humanSize(int64(len(content))))
		default:
			entryBlock = fmt.Sprintf("[内部文件: %s]\n（二进制文件，大小 %s）\n[内部文件结束]", file.Name, humanSize(int64(len(content))))
		}
		blocks = append(blocks, entryBlock)
		processed++
	}

	header := fmt.Sprintf("[用户上传压缩包: %s]\n包含 %d 个文件：\n", filename, processed)
	body := strings.Join(blocks, "\n")
	footer := fmt.Sprintf("\n[压缩包结束，原始大小 %s]", humanSize(int64(len(raw))))
	return header + body + footer
}

// processZipEntryText 处理 zip 内的文本文件，截断后格式化为内部文件块。
func processZipEntryText(filename string, raw []byte) string {
	if !utf8.Valid(raw) {
		runes := make([]rune, 0, len(raw))
		for _, b := range raw {
			runes = append(runes, rune(b))
		}
		raw = []byte(string(runes))
	}
	text := truncateText(string(raw), maxTextInjectionBytes)
	return fmt.Sprintf("[内部文件: %s]\n%s\n[内部文件结束]", filename, text)
}

// processZipEntryPDF 处理 zip 内的 PDF 文件，提取文本后格式化为内部文件块。
func processZipEntryPDF(filename string, raw []byte) string {
	r, err := pdf.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return fmt.Sprintf("[内部文件: %s]\n（PDF 解析失败: %s）\n[内部文件结束]", filename, err.Error())
	}
	textReader, err := r.GetPlainText()
	if err != nil {
		return fmt.Sprintf("[内部文件: %s]\n（PDF 文本提取失败: %s）\n[内部文件结束]", filename, err.Error())
	}
	limited := io.LimitReader(textReader, maxTextInjectionBytes+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Sprintf("[内部文件: %s]\n（PDF 读取失败: %s）\n[内部文件结束]", filename, err.Error())
	}
	extracted := string(buf)
	if strings.TrimSpace(extracted) == "" {
		return fmt.Sprintf("[内部文件: %s]\n（PDF 未提取到文本，可能是扫描件）\n[内部文件结束]", filename)
	}
	if len(buf) > maxTextInjectionBytes {
		extracted = truncateText(extracted, maxTextInjectionBytes)
	}
	return fmt.Sprintf("[内部文件: %s]\n%s\n[内部文件结束]", filename, extracted)
}

// fileNameMIME 根据文件扩展名推测一个简单的 MIME 类型，用于 zip 内部文件的图片判断。
func fileNameMIME(filename string) string {
	ext := strings.ToLower(filenameExt(filename))
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	case ".webp":
		return "image/webp"
	default:
		return ""
	}
}

// processText 处理文本类附件：直接读取 UTF-8 内容，超长时截断保留头尾。
func processText(filename string, raw []byte) string {
	// 尝试 UTF-8 解码验证
	if !utf8.Valid(raw) {
		// 非 UTF-8，尝试 Latin-1 转 UTF-8
		runes := make([]rune, 0, len(raw))
		for _, b := range raw {
			runes = append(runes, rune(b))
		}
		raw = []byte(string(runes))
	}

	text := string(raw)
	text = truncateText(text, maxTextInjectionBytes)
	return formatTextBlock(filename, text, int64(len(raw)))
}

// processPDF 处理 PDF 附件：用 ledongthuc/pdf 提取纯文本。
func processPDF(filename string, raw []byte) string {
	// ledongthuc/pdf 需要从 reader 读取，用 bytes.Reader 模拟文件
	reader := bytes.NewReader(raw)
	r, err := pdf.NewReader(reader, int64(len(raw)))
	if err != nil {
		return formatBinaryBlock(filename, "application/pdf", int64(len(raw)), "PDF 解析失败: "+err.Error())
	}

	// Reader.GetPlainText 返回全部页面的纯文本 io.Reader
	textReader, err := r.GetPlainText()
	if err != nil {
		return formatBinaryBlock(filename, "application/pdf", int64(len(raw)), "PDF 文本提取失败: "+err.Error())
	}

	// 最多读取 maxTextInjectionBytes + 1 字节（多读 1 字节用于判断是否截断）
	limited := io.LimitReader(textReader, maxTextInjectionBytes+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return formatBinaryBlock(filename, "application/pdf", int64(len(raw)), "PDF 读取失败: "+err.Error())
	}

	extracted := string(buf)
	if strings.TrimSpace(extracted) == "" {
		return formatBinaryBlock(filename, "application/pdf", int64(len(raw)), "PDF 未提取到文本（可能是扫描件）")
	}

	// 如果读到了 maxTextInjectionBytes+1 字节，说明原文更长，需要截断
	if len(buf) > maxTextInjectionBytes {
		extracted = truncateText(extracted, maxTextInjectionBytes)
	}
	return formatPDFBlock(filename, extracted, int64(len(raw)))
}

// truncateText 将文本截断到 maxBytes，保留头部和尾部各一半。
// 中间插入省略标记，标明省略了多少字节。
func truncateText(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	half := maxBytes / 2
	head := text[:half]
	// 从 half 往回找到完整 UTF-8 字符边界
	for len(head) > 0 && !utf8.RuneStart(head[len(head)-1]) {
		head = head[:len(head)-1]
	}
	tail := text[len(text)-half:]
	// 从 tail 开头找到完整 UTF-8 字符边界
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	omitted := len(text) - len(head) - len(tail)
	return fmt.Sprintf("%s\n...（省略 %d 字节）...\n%s", head, omitted, tail)
}

// formatTextBlock 格式化文本附件的注入块。
func formatTextBlock(filename, text string, originalSize int64) string {
	return fmt.Sprintf("[用户上传文件: %s]\n%s\n[文件结束，原始大小 %s]",
		filename, text, humanSize(originalSize))
}

// formatPDFBlock 格式化 PDF 附件的注入块。
func formatPDFBlock(filename, extractedText string, originalSize int64) string {
	return fmt.Sprintf("[用户上传文件: %s]\n%s\n[文件结束，原始 PDF 大小 %s]",
		filename, extractedText, humanSize(originalSize))
}

// formatImageBlock 格式化图片附件的注入块（图片已通过多模态消息附加，模型可直接查看）。
func formatImageBlock(filename, mimeType string, size int64) string {
	return fmt.Sprintf("[用户上传图片: %s]\n（图片类型 %s，大小 %s，已附加到消息中，模型可直接查看）\n[图片结束]",
		filename, mimeType, humanSize(size))
}

// formatBinaryBlock 格式化二进制附件的注入块。
func formatBinaryBlock(filename, mimeType string, size int64, note string) string {
	suffix := ""
	if note != "" {
		suffix = "，备注: " + note
	}
	return fmt.Sprintf("[用户上传文件: %s]\n（二进制文件 %s，大小 %s%s）\n[文件结束]",
		filename, mimeType, humanSize(size), suffix)
}

// humanSize 将字节数格式化为人类可读的大小。
func humanSize(bytes int64) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%dB", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
	}
}

// filenameExt 从文件名中提取扩展名（含点），返回小写。
func filenameExt(filename string) string {
	idx := strings.LastIndex(filename, ".")
	if idx < 0 {
		return ""
	}
	return filename[idx:]
}
