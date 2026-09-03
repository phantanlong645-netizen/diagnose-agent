package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"olt-diagnostic-agent/internal/domain"
)

const (
	maxNBIResponseBytes                  = 2 * 1024 * 1024
	maxAccessConsoleLogArchiveBytes      = 64 * 1024 * 1024
	maxAccessConsoleLogEntryBytes        = 16 * 1024 * 1024
	maxAccessConsoleLogExtractedBytes    = 64 * 1024 * 1024
	maxAccessConsoleLogEvidenceBytes     = 64 * 1024
	maxAccessConsoleLogLineBytes         = 8 * 1024
	defaultAccessConsoleLogMatches       = 80
	maximumAccessConsoleLogMatches       = 500
	defaultAccessConsoleLogContextLines  = 2
	maximumAccessConsoleLogContextLines  = 10
	maximumAccessConsoleLogKeywordCount  = 12
	maximumAccessConsoleLogKeywordLength = 256
	accessConsoleLogDownloadPath         = "/nms/v1/log/download"
)

var accessConsoleLogNames = map[string]string{
	"alarm.log":        "alarm.log",
	"olt.log":          "olt.log",
	"ont.log":          "ont.log",
	"oss.log":          "oss.log",
	"syslog.log":       "syslog.log",
	"chain.log":        "chain.log",
	"panic.log":        "panic.log",
	"panicn.log":       "panicN.log",
	"job.log":          "job.log",
	"error.log":        "error.log",
	"northbound.log":   "northbound.log",
	"local_syslog.log": "local_syslog.log",
}

var defaultAccessConsoleLogNames = []string{
	"alarm.log", "olt.log", "ont.log", "oss.log", "syslog.log", "chain.log", "panic.log", "panicN.log", "job.log",
}

type NBIRequest struct {
	ProfileID string            `json:"profileId"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      string            `json:"body,omitempty"`
}

type NBIResponse struct {
	RequestURL     string              `json:"requestUrl"`
	StatusCode     int                 `json:"statusCode"`
	ContentType    string              `json:"contentType"`
	Headers        map[string][]string `json:"headers"`
	Body           string              `json:"body"`
	DurationMS     int64               `json:"durationMs"`
	Classification string              `json:"classification"`
	Successful     bool                `json:"successful"`
	Truncated      bool                `json:"truncated"`
	Redirects      []string            `json:"redirects,omitempty"`
}

type AccessConsoleLogsRequest struct {
	ProfileID    string   `json:"profileId"`
	LogNames     []string `json:"logNames,omitempty"`
	Keywords     []string `json:"keywords,omitempty"`
	ContextLines int      `json:"contextLines,omitempty"`
	MaxMatches   int      `json:"maxMatches,omitempty"`
}

type AccessConsoleLogFileSummary struct {
	Name         string `json:"name"`
	Bytes        int    `json:"bytes"`
	Lines        int    `json:"lines"`
	MatchedLines int    `json:"matchedLines"`
	Selected     bool   `json:"selected"`
}

type AccessConsoleLogExcerpt struct {
	File      string `json:"file"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Text      string `json:"text"`
}

type AccessConsoleLogsResponse struct {
	RequestURL      string                        `json:"requestUrl"`
	StatusCode      int                           `json:"statusCode"`
	ArchiveBytes    int                           `json:"archiveBytes"`
	ArchiveSHA256   string                        `json:"archiveSha256"`
	Keywords        []string                      `json:"keywords,omitempty"`
	Files           []AccessConsoleLogFileSummary `json:"files"`
	MissingLogNames []string                      `json:"missingLogNames,omitempty"`
	MatchedLines    int                           `json:"matchedLines"`
	Excerpts        []AccessConsoleLogExcerpt     `json:"excerpts"`
	Truncated       bool                          `json:"truncated"`
	DurationMS      int64                         `json:"durationMs"`
	Redirects       []string                      `json:"redirects,omitempty"`
}

type NBITool struct {
	targets TargetResolver
	client  *http.Client
	tokenMu sync.Mutex
	tokens  map[string]string
}

type AccessConsoleLogsTool struct {
	nbi *NBITool
}

type preparedAccessConsoleLogsRequest struct {
	Request AccessConsoleLogsRequest
	Target  domain.NBITarget
}

func NewNBITool(targets TargetResolver, client *http.Client) (*NBITool, error) {
	if targets == nil {
		return nil, errors.New("target resolver is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &NBITool{targets: targets, client: client, tokens: make(map[string]string)}, nil
}

func NewAccessConsoleLogsTool(nbi *NBITool) (*AccessConsoleLogsTool, error) {
	if nbi == nil {
		return nil, errors.New("NBI tool is required")
	}
	return &AccessConsoleLogsTool{nbi: nbi}, nil
}

func (t *AccessConsoleLogsTool) Definition() Definition {
	return Definition{
		Name:        "collect_access_console_logs",
		Description: "Download the current authenticated Access Console business-log bundle and return bounded redacted excerpts from selected known log files.",
	}
}

func (t *AccessConsoleLogsTool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var input AccessConsoleLogsRequest
	if err := json.Unmarshal(arguments, &input); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode arguments: %w", err)
	}
	input.ProfileID = strings.TrimSpace(input.ProfileID)
	target, err := t.nbi.nbiTarget(input.ProfileID)
	if err != nil {
		return domain.PreparedCall{}, err
	}
	input.LogNames, err = normalizeAccessConsoleLogNames(input.LogNames)
	if err != nil {
		return domain.PreparedCall{}, err
	}
	input.Keywords, err = normalizeAccessConsoleLogKeywords(input.Keywords)
	if err != nil {
		return domain.PreparedCall{}, err
	}
	if input.ContextLines == 0 {
		input.ContextLines = defaultAccessConsoleLogContextLines
	}
	if input.ContextLines < 0 || input.ContextLines > maximumAccessConsoleLogContextLines {
		return domain.PreparedCall{}, fmt.Errorf("contextLines must be between 0 and %d", maximumAccessConsoleLogContextLines)
	}
	if input.MaxMatches == 0 {
		input.MaxMatches = defaultAccessConsoleLogMatches
	}
	if input.MaxMatches < 1 || input.MaxMatches > maximumAccessConsoleLogMatches {
		return domain.PreparedCall{}, fmt.Errorf("maxMatches must be between 1 and %d", maximumAccessConsoleLogMatches)
	}
	requestURL, err := t.nbi.resolveURL(target, accessConsoleLogDownloadPath)
	if err != nil {
		return domain.PreparedCall{}, err
	}
	summary := "Collect current Access Console business logs"
	if len(input.Keywords) > 0 {
		summary += fmt.Sprintf(" using %d focused keyword(s)", len(input.Keywords))
	}
	previewKeywords := redactLocalText(strings.Join(input.Keywords, ", "))
	return domain.PreparedCall{
		Summary: summary,
		Preview: fmt.Sprintf("GET %s\nLogs: %s\nKeywords: %s", redactURL(requestURL), strings.Join(input.LogNames, ", "), previewKeywords),
		Annotations: domain.ToolAnnotations{
			ReadOnly:  true,
			OpenWorld: true,
		},
		Input: preparedAccessConsoleLogsRequest{Request: input, Target: target},
	}, nil
}

func normalizeAccessConsoleLogNames(values []string) ([]string, error) {
	if len(values) == 0 {
		return append([]string(nil), defaultAccessConsoleLogNames...), nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		canonical, exists := accessConsoleLogNames[normalized]
		if !exists {
			return nil, fmt.Errorf("unsupported Access Console log %q", value)
		}
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, canonical)
	}
	return result, nil
}

func normalizeAccessConsoleLogKeywords(values []string) ([]string, error) {
	if len(values) > maximumAccessConsoleLogKeywordCount {
		return nil, fmt.Errorf("at most %d log keywords are allowed", maximumAccessConsoleLogKeywordCount)
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) > maximumAccessConsoleLogKeywordLength {
			return nil, fmt.Errorf("a log keyword exceeds %d bytes", maximumAccessConsoleLogKeywordLength)
		}
		key := strings.ToLower(value)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func (t *NBITool) Definition() Definition {
	return Definition{
		Name:        "nbi_request",
		Description: "Send an HTTP request to the configured Access Console northbound API. NBI POST/PUT/PATCH/DELETE calls require the current JWT to hold the single-holder write lease from POST /northbound/auth/permissions; GET/HEAD/OPTIONS do not.",
	}
}

func (t *NBITool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var input NBIRequest
	if err := json.Unmarshal(arguments, &input); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode arguments: %w", err)
	}
	input.ProfileID = strings.TrimSpace(input.ProfileID)
	target, err := t.nbiTarget(input.ProfileID)
	if err != nil {
		return domain.PreparedCall{}, err
	}
	input.Method = strings.ToUpper(strings.TrimSpace(input.Method))
	if input.Method == "" {
		input.Method = http.MethodGet
	}
	if strings.TrimSpace(input.Path) == "" {
		return domain.PreparedCall{}, errors.New("request path is required")
	}
	normalizedPath := "/" + strings.TrimLeft(strings.TrimSpace(input.Path), "/")
	if !strings.HasPrefix(normalizedPath, "/northbound/") {
		return domain.PreparedCall{}, errors.New("Access Console NBI path must start with /northbound/; refusing an unverified API path")
	}
	input.Path = normalizedPath
	requestURL, err := t.resolveURL(target, input.Path)
	if err != nil {
		return domain.PreparedCall{}, err
	}

	annotations := domain.ToolAnnotations{OpenWorld: true}
	switch input.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		annotations.ReadOnly = true
		annotations.Idempotent = true
	case http.MethodPut:
		annotations.Idempotent = true
	case http.MethodDelete:
		annotations.Destructive = true
		annotations.Idempotent = true
	case http.MethodPost, http.MethodPatch:
	default:
		return domain.PreparedCall{}, fmt.Errorf("unsupported HTTP method: %s", input.Method)
	}

	return domain.PreparedCall{
		Summary:     fmt.Sprintf("%s %s", input.Method, redactURL(requestURL)),
		Preview:     nbiPreview(input, requestURL),
		Annotations: annotations,
		Input: preparedNBIRequest{
			Request: input,
			Target:  target,
		},
	}, nil
}

func nbiPreview(input NBIRequest, requestURL *url.URL) string {
	preview := input.Method + " " + redactURL(requestURL)
	if len(input.Headers) > 0 {
		names := make([]string, 0, len(input.Headers))
		for name := range input.Headers {
			names = append(names, name)
		}
		sort.Strings(names)
		preview += "\nHeaders: " + strings.Join(names, ", ")
	}
	if strings.TrimSpace(input.Body) == "" {
		return preview
	}
	body := redactLocalText(string(redactJSONBody([]byte(input.Body))))
	return preview + "\n\n" + abbreviate(body, 8000)
}

func (t *NBITool) Execute(ctx context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	prepared, ok := call.Input.(preparedNBIRequest)
	if !ok {
		return domain.ToolResult{}, errors.New("invalid prepared NBI request")
	}
	input := prepared.Request
	requestURL, err := t.resolveURL(prepared.Target, input.Path)
	if err != nil {
		return domain.ToolResult{}, err
	}

	startedAt := time.Now()
	client, redirects, err := t.requestClient(prepared.Target, requestURL)
	if err != nil {
		return domain.ToolResult{}, err
	}
	var token string
	if isWritePermissionAcquisition(input) && prepared.Target.Username != "" {
		token, err = t.refreshAuthenticationToken(ctx, input.ProfileID, prepared.Target, client)
	} else {
		token, err = t.authenticationToken(ctx, input.ProfileID, prepared.Target, client)
	}
	if err != nil {
		return domain.ToolResult{}, err
	}
	response, body, err := t.send(ctx, client, input, requestURL, prepared.Target.TokenHeader, token)
	if err != nil {
		return domain.ToolResult{}, err
	}
	t.rememberRenewedToken(input.ProfileID, prepared.Target, response.Header.Get("newtoken"))
	if tokenAuthenticationFailed(response.StatusCode, body) && prepared.Target.Username != "" {
		t.forgetToken(input.ProfileID, prepared.Target, token)
		token, err = t.authenticationToken(ctx, input.ProfileID, prepared.Target, client)
		if err != nil {
			return domain.ToolResult{}, err
		}
		response, body, err = t.send(ctx, client, input, requestURL, prepared.Target.TokenHeader, token)
		if err != nil {
			return domain.ToolResult{}, err
		}
		t.rememberRenewedToken(input.ProfileID, prepared.Target, response.Header.Get("newtoken"))
	}
	truncated := len(body) > maxNBIResponseBytes
	if truncated {
		body = body[:maxNBIResponseBytes]
	}
	contentType := response.Header.Get("Content-Type")
	classification := classifyHTTPResponse(contentType, body)
	body = redactJSONBody(body)
	result := NBIResponse{
		RequestURL:     redactURL(requestURL),
		StatusCode:     response.StatusCode,
		ContentType:    contentType,
		Headers:        redactHTTPHeaders(response.Header),
		Body:           string(body),
		DurationMS:     time.Since(startedAt).Milliseconds(),
		Classification: classification,
		Successful:     response.StatusCode >= 200 && response.StatusCode < 300 && classification != "html-ui-fallback",
		Truncated:      truncated,
		Redirects:      *redirects,
	}

	return domain.ToolResult{
		Summary: fmt.Sprintf("NBI returned HTTP %d (%s)", response.StatusCode, classification),
		Data:    result,
		Metadata: map[string]string{
			"http.status_code":  fmt.Sprintf("%d", response.StatusCode),
			"http.content_type": contentType,
			"classification":    classification,
		},
	}, nil
}

func isWritePermissionAcquisition(input NBIRequest) bool {
	if input.Method != http.MethodPost {
		return false
	}
	reference, err := url.Parse(input.Path)
	return err == nil && reference.Path == "/northbound/auth/permissions"
}

func (t *AccessConsoleLogsTool) Execute(ctx context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	prepared, ok := call.Input.(preparedAccessConsoleLogsRequest)
	if !ok {
		return domain.ToolResult{}, errors.New("invalid prepared Access Console logs request")
	}
	requestURL, err := t.nbi.resolveURL(prepared.Target, accessConsoleLogDownloadPath)
	if err != nil {
		return domain.ToolResult{}, err
	}
	startedAt := time.Now()
	client, redirects, err := t.nbi.requestClient(prepared.Target, requestURL)
	if err != nil {
		return domain.ToolResult{}, err
	}
	token, err := t.nbi.authenticationToken(ctx, prepared.Request.ProfileID, prepared.Target, client)
	if err != nil {
		return domain.ToolResult{}, err
	}
	response, archive, err := sendAccessConsoleLogDownload(ctx, client, requestURL, prepared.Target.TokenHeader, token)
	if err != nil {
		return domain.ToolResult{}, err
	}
	t.nbi.rememberRenewedToken(prepared.Request.ProfileID, prepared.Target, response.Header.Get("newtoken"))
	if tokenAuthenticationFailed(response.StatusCode, archive) && prepared.Target.Username != "" {
		t.nbi.forgetToken(prepared.Request.ProfileID, prepared.Target, token)
		token, err = t.nbi.authenticationToken(ctx, prepared.Request.ProfileID, prepared.Target, client)
		if err != nil {
			return domain.ToolResult{}, err
		}
		response, archive, err = sendAccessConsoleLogDownload(ctx, client, requestURL, prepared.Target.TokenHeader, token)
		if err != nil {
			return domain.ToolResult{}, err
		}
		t.nbi.rememberRenewedToken(prepared.Request.ProfileID, prepared.Target, response.Header.Get("newtoken"))
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body := abbreviate(redactLocalText(string(redactJSONBody(archive))), 2000)
		return domain.ToolResult{}, fmt.Errorf("Access Console log download returned HTTP %d: %s", response.StatusCode, body)
	}
	if !zipSignature(archive) {
		classification := classifyHTTPResponse(response.Header.Get("Content-Type"), archive)
		body := abbreviate(redactLocalText(string(redactJSONBody(archive))), 2000)
		return domain.ToolResult{}, fmt.Errorf("Access Console log download returned %s instead of a ZIP archive: %s", classification, body)
	}

	result, err := analyzeAccessConsoleLogArchive(archive, prepared.Request)
	if err != nil {
		return domain.ToolResult{}, err
	}
	digest := sha256.Sum256(archive)
	result.RequestURL = redactURL(requestURL)
	result.StatusCode = response.StatusCode
	result.ArchiveBytes = len(archive)
	result.ArchiveSHA256 = fmt.Sprintf("%x", digest)
	result.DurationMS = time.Since(startedAt).Milliseconds()
	result.Redirects = append([]string(nil), (*redirects)...)

	return domain.ToolResult{
		Summary: fmt.Sprintf("Collected %d Access Console log excerpts from %d matching lines", len(result.Excerpts), result.MatchedLines),
		Data:    result,
		Metadata: map[string]string{
			"http.status_code": fmt.Sprintf("%d", response.StatusCode),
			"archive.bytes":    fmt.Sprintf("%d", len(archive)),
			"archive.sha256":   result.ArchiveSHA256,
			"matches":          fmt.Sprintf("%d", result.MatchedLines),
			"truncated":        fmt.Sprintf("%t", result.Truncated),
		},
	}, nil
}

func sendAccessConsoleLogDownload(ctx context.Context, client *http.Client, requestURL *url.URL, tokenHeader, token string) (*http.Response, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create Access Console log download request: %w", err)
	}
	request.Header.Set(tokenHeader, token)
	response, err := client.Do(request)
	if err != nil {
		var unknownAuthority x509.UnknownAuthorityError
		if errors.As(err, &unknownAuthority) {
			return nil, nil, errors.New("NBI TLS certificate is not trusted; configure a custom CA file or enable Skip TLS verification only for an isolated lab target")
		}
		return nil, nil, fmt.Errorf("send Access Console log download request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAccessConsoleLogArchiveBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read Access Console log archive: %w", err)
	}
	if len(body) > maxAccessConsoleLogArchiveBytes {
		return nil, nil, fmt.Errorf("Access Console log archive exceeds %d bytes", maxAccessConsoleLogArchiveBytes)
	}
	return response, body, nil
}

func zipSignature(value []byte) bool {
	return len(value) >= 4 && value[0] == 'P' && value[1] == 'K' && (value[2] == 3 || value[2] == 5 || value[2] == 7) && (value[3] == 4 || value[3] == 6 || value[3] == 8)
}

func analyzeAccessConsoleLogArchive(archive []byte, request AccessConsoleLogsRequest) (AccessConsoleLogsResponse, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return AccessConsoleLogsResponse{}, fmt.Errorf("open Access Console log archive: %w", err)
	}
	selected := make(map[string]struct{}, len(request.LogNames))
	for _, name := range request.LogNames {
		selected[strings.ToLower(name)] = struct{}{}
	}
	found := make(map[string]struct{}, len(request.LogNames))
	keywords := make([]string, 0, len(request.Keywords))
	for _, keyword := range request.Keywords {
		keywords = append(keywords, strings.ToLower(keyword))
	}
	redactedKeywords := make([]string, 0, len(request.Keywords))
	for _, keyword := range request.Keywords {
		redactedKeywords = append(redactedKeywords, redactLocalText(keyword))
	}
	result := AccessConsoleLogsResponse{
		Keywords: redactedKeywords,
		Files:    make([]AccessConsoleLogFileSummary, 0, len(reader.File)),
		Excerpts: make([]AccessConsoleLogExcerpt, 0),
	}
	totalExtracted := uint64(0)
	evidenceBytes := 0
	remainingMatches := request.MaxMatches

	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		cleanName := path.Clean(strings.ReplaceAll(file.Name, "\\", "/"))
		if cleanName != path.Base(cleanName) || cleanName == "." || strings.HasPrefix(cleanName, "../") {
			return AccessConsoleLogsResponse{}, fmt.Errorf("Access Console log archive contains an unsafe entry name: %q", file.Name)
		}
		canonical, supported := accessConsoleLogNames[strings.ToLower(cleanName)]
		if !supported {
			return AccessConsoleLogsResponse{}, fmt.Errorf("Access Console log archive contains an unexpected entry: %q", file.Name)
		}
		if file.UncompressedSize64 > maxAccessConsoleLogEntryBytes {
			return AccessConsoleLogsResponse{}, fmt.Errorf("Access Console log entry %s exceeds %d bytes", canonical, maxAccessConsoleLogEntryBytes)
		}
		totalExtracted += file.UncompressedSize64
		if totalExtracted > maxAccessConsoleLogExtractedBytes {
			return AccessConsoleLogsResponse{}, fmt.Errorf("Access Console logs exceed %d extracted bytes", maxAccessConsoleLogExtractedBytes)
		}
		entry, openErr := file.Open()
		if openErr != nil {
			return AccessConsoleLogsResponse{}, fmt.Errorf("open Access Console log entry %s: %w", canonical, openErr)
		}
		content, readErr := io.ReadAll(io.LimitReader(entry, maxAccessConsoleLogEntryBytes+1))
		closeErr := entry.Close()
		if readErr != nil {
			return AccessConsoleLogsResponse{}, fmt.Errorf("read Access Console log entry %s: %w", canonical, readErr)
		}
		if closeErr != nil {
			return AccessConsoleLogsResponse{}, fmt.Errorf("close Access Console log entry %s: %w", canonical, closeErr)
		}
		if len(content) > maxAccessConsoleLogEntryBytes {
			return AccessConsoleLogsResponse{}, fmt.Errorf("Access Console log entry %s exceeds %d bytes", canonical, maxAccessConsoleLogEntryBytes)
		}
		if bytes.IndexByte(content, 0) >= 0 {
			return AccessConsoleLogsResponse{}, fmt.Errorf("Access Console log entry %s is binary", canonical)
		}

		lines := splitAccessConsoleLogLines(string(content))
		_, isSelected := selected[strings.ToLower(canonical)]
		summary := AccessConsoleLogFileSummary{Name: canonical, Bytes: len(content), Lines: len(lines), Selected: isSelected}
		if isSelected {
			found[strings.ToLower(canonical)] = struct{}{}
			matches := matchingAccessConsoleLogLines(lines, keywords)
			summary.MatchedLines = len(matches)
			result.MatchedLines += len(matches)
			for _, lineIndex := range matches {
				if remainingMatches == 0 {
					result.Truncated = true
					break
				}
				excerpt := accessConsoleLogExcerpt(canonical, lines, lineIndex, request.ContextLines)
				if evidenceBytes+len(excerpt.Text) > maxAccessConsoleLogEvidenceBytes {
					result.Truncated = true
					remainingMatches = 0
					break
				}
				result.Excerpts = append(result.Excerpts, excerpt)
				evidenceBytes += len(excerpt.Text)
				remainingMatches--
			}
		}
		result.Files = append(result.Files, summary)
	}

	for _, name := range request.LogNames {
		if _, exists := found[strings.ToLower(name)]; !exists {
			result.MissingLogNames = append(result.MissingLogNames, name)
		}
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Name < result.Files[j].Name })
	sort.Strings(result.MissingLogNames)
	return result, nil
}

func splitAccessConsoleLogLines(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.TrimSuffix(content, "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

func matchingAccessConsoleLogLines(lines, keywords []string) []int {
	if len(keywords) == 0 {
		result := make([]int, len(lines))
		for index := range lines {
			result[index] = index
		}
		return result
	}
	result := make([]int, 0)
	for index, line := range lines {
		lowerLine := strings.ToLower(line)
		for _, keyword := range keywords {
			if strings.Contains(lowerLine, keyword) {
				result = append(result, index)
				break
			}
		}
	}
	return result
}

func accessConsoleLogExcerpt(file string, lines []string, lineIndex, contextLines int) AccessConsoleLogExcerpt {
	start := lineIndex - contextLines
	if start < 0 {
		start = 0
	}
	end := lineIndex + contextLines + 1
	if end > len(lines) {
		end = len(lines)
	}
	visible := make([]string, 0, end-start)
	for _, line := range lines[start:end] {
		if len(line) > maxAccessConsoleLogLineBytes {
			line = line[:maxAccessConsoleLogLineBytes] + " [line truncated]"
		}
		visible = append(visible, redactLocalText(line))
	}
	return AccessConsoleLogExcerpt{
		File:      file,
		StartLine: start + 1,
		EndLine:   end,
		Text:      strings.Join(visible, "\n"),
	}
}

func (t *NBITool) authenticationToken(ctx context.Context, profileID string, target domain.NBITarget, client *http.Client) (string, error) {
	if target.Username == "" {
		return target.Token, nil
	}
	key := nbiTokenKey(profileID, target)
	t.tokenMu.Lock()
	defer t.tokenMu.Unlock()
	if token := t.tokens[key]; token != "" {
		return token, nil
	}
	token, err := t.login(ctx, target, client)
	if err != nil {
		return "", err
	}
	t.tokens[key] = token
	return token, nil
}

// refreshAuthenticationToken deliberately bypasses the cached JWT before a
// write-permission acquisition. Access Console can renew a near-expiry JWT in
// that request while binding its single-holder lease to the old request token;
// logging in first prevents the lease and the client's cached token diverging.
func (t *NBITool) refreshAuthenticationToken(ctx context.Context, profileID string, target domain.NBITarget, client *http.Client) (string, error) {
	key := nbiTokenKey(profileID, target)
	t.tokenMu.Lock()
	defer t.tokenMu.Unlock()
	token, err := t.login(ctx, target, client)
	if err != nil {
		return "", err
	}
	t.tokens[key] = token
	return token, nil
}

func (t *NBITool) login(ctx context.Context, target domain.NBITarget, client *http.Client) (string, error) {
	loginURL, err := t.resolveURL(target, "/northbound/auth/login")
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("create NBI login request: %w", err)
	}
	request.SetBasicAuth(target.Username, target.Password)
	response, body, err := sendNBIRequest(client, request)
	if err != nil {
		return "", fmt.Errorf("send NBI login request: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		redactedBody := abbreviate(redactLocalText(string(redactJSONBody(body))), 2000)
		return "", fmt.Errorf("NBI login returned HTTP %d: %s", response.StatusCode, redactedBody)
	}
	var result struct {
		Token string `json:"token"`
	}
	if err = json.Unmarshal(body, &result); err != nil {
		return "", errors.New("NBI login response is not valid JSON")
	}
	result.Token = strings.TrimSpace(result.Token)
	if result.Token == "" {
		return "", errors.New("NBI login response did not contain a token")
	}
	return result.Token, nil
}

func (t *NBITool) send(ctx context.Context, client *http.Client, input NBIRequest, requestURL *url.URL, tokenHeader, token string) (*http.Response, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, input.Method, requestURL.String(), bytes.NewBufferString(input.Body))
	if err != nil {
		return nil, nil, fmt.Errorf("create NBI request: %w", err)
	}
	for name, value := range input.Headers {
		request.Header.Set(name, value)
	}
	request.Header.Set(tokenHeader, token)
	if input.Body != "" && request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, body, err := sendNBIRequest(client, request)
	if err != nil {
		return nil, nil, fmt.Errorf("send NBI request: %w", err)
	}
	return response, body, nil
}

func sendNBIRequest(client *http.Client, request *http.Request) (*http.Response, []byte, error) {
	response, err := client.Do(request)
	if err != nil {
		var unknownAuthority x509.UnknownAuthorityError
		if errors.As(err, &unknownAuthority) {
			return nil, nil, errors.New("NBI TLS certificate is not trusted; configure a custom CA file or enable Skip TLS verification only for an isolated lab target")
		}
		return nil, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxNBIResponseBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read NBI response: %w", err)
	}
	return response, body, nil
}

func (t *NBITool) rememberRenewedToken(profileID string, target domain.NBITarget, token string) {
	token = strings.TrimSpace(token)
	if target.Username == "" || token == "" {
		return
	}
	t.tokenMu.Lock()
	t.tokens[nbiTokenKey(profileID, target)] = token
	t.tokenMu.Unlock()
}

func (t *NBITool) forgetToken(profileID string, target domain.NBITarget, token string) {
	key := nbiTokenKey(profileID, target)
	t.tokenMu.Lock()
	if t.tokens[key] == token {
		delete(t.tokens, key)
	}
	t.tokenMu.Unlock()
}

func nbiTokenKey(profileID string, target domain.NBITarget) string {
	credentials := sha256.Sum256([]byte(target.Username + "\x00" + target.Password))
	return fmt.Sprintf("%s\x00%s\x00%x", profileID, target.BaseURL, credentials)
}

func tokenAuthenticationFailed(statusCode int, body []byte) bool {
	if statusCode != http.StatusUnauthorized {
		return false
	}
	var response struct {
		Status int `json:"status"`
	}
	if json.Unmarshal(body, &response) != nil {
		return false
	}
	switch response.Status {
	case 20001, 20002, 20003, 20004:
		return true
	default:
		return false
	}
}

func (t *NBITool) requestClient(target domain.NBITarget, originalURL *url.URL) (*http.Client, *[]string, error) {
	client := *t.client
	redirects := make([]string, 0)
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("NBI redirect limit exceeded")
		}
		if !strings.EqualFold(request.URL.Host, originalURL.Host) || request.URL.Scheme != originalURL.Scheme {
			return errors.New("NBI redirect escaped the configured target")
		}
		redirects = append(redirects, redactURL(request.URL))
		return nil
	}
	if !target.InsecureTLS && strings.TrimSpace(target.CAFile) == "" {
		// NBI 目标都是内网 lab 设备，不走系统代理（HTTPS_PROXY/HTTP_PROXY）。
		// 代理无法路由 10.x.x.x 内网地址，会导致 TLS 握手超时。
		transport := directTransport()
		client.Transport = transport
		return &client, &redirects, nil
	}

	baseTransport := directTransport()
	if configured, ok := t.client.Transport.(*http.Transport); ok {
		baseTransport = configured.Clone()
		baseTransport.Proxy = nil
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if target.InsecureTLS {
		tlsConfig.InsecureSkipVerify = true //nolint:gosec -- explicit per-target lab setting
	} else {
		certificate, err := os.ReadFile(target.CAFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read NBI CA file: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(certificate) {
			return nil, nil, errors.New("NBI CA file does not contain a valid PEM certificate")
		}
		tlsConfig.RootCAs = roots
	}
	baseTransport.TLSClientConfig = tlsConfig
	client.Transport = baseTransport
	return &client, &redirects, nil
}

// directTransport 返回一个不走系统代理的 HTTP Transport。
// NBI 目标都是内网 lab 设备（10.x.x.x 等），系统代理（HTTPS_PROXY）无法路由
// 内网地址，会导致 TLS 握手超时。这里强制直连。
func directTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil, // 不走系统代理，直连内网设备
	}
}

type preparedNBIRequest struct {
	Request NBIRequest
	Target  domain.NBITarget
}

func (t *NBITool) nbiTarget(profileID string) (domain.NBITarget, error) {
	if profileID == "" {
		return domain.NBITarget{}, errors.New("profile ID is required")
	}
	target, exists := t.targets.NBI(profileID)
	if !exists {
		return domain.NBITarget{}, fmt.Errorf("NBI target not found for profile: %s", profileID)
	}
	baseURL, err := url.Parse(strings.TrimSpace(target.BaseURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return domain.NBITarget{}, errors.New("NBI base URL must include a scheme and host")
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return domain.NBITarget{}, errors.New("NBI base URL must use HTTP or HTTPS")
	}
	target.BaseURL = strings.TrimRight(baseURL.String(), "/")
	if target.TokenHeader == "" {
		target.TokenHeader = "token"
	}
	return target, nil
}

func (t *NBITool) resolveURL(target domain.NBITarget, pathValue string) (*url.URL, error) {
	pathValue = strings.ReplaceAll(pathValue, "{olt_ip}", url.PathEscape(target.OLTAddress))
	reference, err := url.Parse(pathValue)
	if err != nil {
		return nil, fmt.Errorf("parse request path: %w", err)
	}
	if reference.IsAbs() || reference.Host != "" {
		return nil, errors.New("NBI request path must be relative to the configured base URL")
	}
	baseURL, _ := url.Parse(target.BaseURL + "/")
	resolved := baseURL.ResolveReference(reference)
	if !strings.EqualFold(baseURL.Host, resolved.Host) || baseURL.Scheme != resolved.Scheme {
		return nil, errors.New("NBI request URL escaped the configured target")
	}
	return resolved, nil
}

func classifyHTTPResponse(contentType string, body []byte) string {
	contentType = strings.ToLower(contentType)
	trimmedBody := strings.ToLower(strings.TrimSpace(string(body)))
	if len(trimmedBody) == 0 {
		return "empty"
	}
	if strings.Contains(contentType, "text/html") || strings.HasPrefix(trimmedBody, "<!doctype html") || strings.HasPrefix(trimmedBody, "<html") {
		return "html-ui-fallback"
	}
	if strings.Contains(contentType, "json") || json.Valid(body) {
		return "json"
	}
	if strings.Contains(contentType, "xml") || strings.HasPrefix(trimmedBody, "<?xml") {
		return "xml"
	}
	return "text"
}

func redactHTTPHeaders(headers http.Header) map[string][]string {
	redacted := make(map[string][]string, len(headers))
	for name, values := range headers {
		if sensitiveName(name) {
			redacted[name] = []string{"[REDACTED]"}
		} else {
			redacted[name] = append([]string(nil), values...)
		}
	}
	return redacted
}

func redactURL(value *url.URL) string {
	copyValue := *value
	query := copyValue.Query()
	for name := range query {
		if sensitiveName(name) {
			query.Set(name, "[REDACTED]")
		}
	}
	copyValue.RawQuery = query.Encode()
	return copyValue.Redacted()
}

func redactJSONBody(body []byte) []byte {
	var value any
	if !json.Valid(body) || json.Unmarshal(body, &value) != nil {
		return body
	}
	redactJSONValue(value)
	redacted, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return body
	}
	return redacted
}

func redactJSONValue(value any) {
	switch item := value.(type) {
	case map[string]any:
		for name, child := range item {
			if sensitiveName(name) {
				item[name] = "[REDACTED]"
				continue
			}
			redactJSONValue(child)
		}
	case []any:
		for _, child := range item {
			redactJSONValue(child)
		}
	}
}

func sensitiveName(name string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, "-", ""), "_", ""))
	for _, marker := range []string{"authorization", "password", "passwd", "apikey", "secret", "token", "cookie"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}
