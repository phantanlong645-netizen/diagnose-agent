package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"olt-diagnostic-agent/internal/domain"
)

// web 工具移植自 paicli-go 的 internal/tools/web.go（web_search / web_fetch）。
// 配置与 paicli-go 一致：SEARXNG_BASE_URL、SERPAPI_API_KEY，缺省回退 DuckDuckGo。
// 两个工具都是只读外部查询，走 ReadOnly+Idempotent 注解，policy 会自动放行。

const (
	webRequestTimeout    = 30 * time.Second
	webSearchMaxBytes    = 2 << 20
	webFetchMaxBytes     = 5 << 20
	webSearchResultLimit = 10
	webFetchMaxChars     = 60000
	webFetchDefaultChars = 8000
)

type WebSearchRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"maxResults,omitempty"`
}

type WebSearchTool struct{}

func NewWebSearchTool() (*WebSearchTool, error) {
	return &WebSearchTool{}, nil
}

func (t *WebSearchTool) Definition() Definition {
	return Definition{
		Name:        "web_search",
		Description: "Search the public web for current information and return ranked text results with titles and URLs. Use it for product documentation, standards, known issues, release notes, or facts outside the local workspace. Read-only.",
	}
}

func (t *WebSearchTool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var input WebSearchRequest
	if err := json.Unmarshal(arguments, &input); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode arguments: %w", err)
	}
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" {
		return domain.PreparedCall{}, errors.New("query is required")
	}
	if input.MaxResults == 0 {
		input.MaxResults = 5
	}
	if input.MaxResults < 1 || input.MaxResults > webSearchResultLimit {
		return domain.PreparedCall{}, fmt.Errorf("maxResults must be between 1 and %d", webSearchResultLimit)
	}
	return domain.PreparedCall{
		Summary:     fmt.Sprintf("Search the web for %q", input.Query),
		Annotations: domain.ToolAnnotations{ReadOnly: true, Idempotent: true},
		Input:       input,
	}, nil
}

func (t *WebSearchTool) Execute(ctx context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	input, ok := call.Input.(WebSearchRequest)
	if !ok {
		return domain.ToolResult{}, errors.New("invalid prepared web search")
	}
	results, err := webSearch(ctx, input.Query, input.MaxResults)
	if err != nil {
		return domain.ToolResult{}, err
	}
	return domain.ToolResult{
		Summary: fmt.Sprintf("Web search returned results for %q", input.Query),
		Data:    map[string]any{"query": input.Query, "results": results},
	}, nil
}

type WebFetchRequest struct {
	URL      string `json:"url"`
	MaxChars int    `json:"maxChars,omitempty"`
}

type WebFetchTool struct{}

func NewWebFetchTool() (*WebFetchTool, error) {
	return &WebFetchTool{}, nil
}

func (t *WebFetchTool) Definition() Definition {
	return Definition{
		Name:        "web_fetch",
		Description: "Fetch a public http(s) web page and return its readable text. Use it to read documentation or articles referenced by web_search results. Only public http/https URLs are allowed.",
	}
}

func (t *WebFetchTool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var input WebFetchRequest
	if err := json.Unmarshal(arguments, &input); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode arguments: %w", err)
	}
	input.URL = strings.TrimSpace(input.URL)
	if input.URL == "" {
		return domain.PreparedCall{}, errors.New("url is required")
	}
	if err := validateWebURL(input.URL); err != nil {
		return domain.PreparedCall{}, err
	}
	if input.MaxChars == 0 {
		input.MaxChars = webFetchDefaultChars
	}
	if input.MaxChars < 1000 || input.MaxChars > webFetchMaxChars {
		return domain.PreparedCall{}, fmt.Errorf("maxChars must be between 1000 and %d", webFetchMaxChars)
	}
	return domain.PreparedCall{
		Summary:     "Fetch " + input.URL,
		Annotations: domain.ToolAnnotations{ReadOnly: true, Idempotent: true},
		Input:       input,
	}, nil
}

func (t *WebFetchTool) Execute(ctx context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	input, ok := call.Input.(WebFetchRequest)
	if !ok {
		return domain.ToolResult{}, errors.New("invalid prepared web fetch")
	}
	if err := validateWebURL(input.URL); err != nil {
		return domain.ToolResult{}, err
	}
	page, err := webGet(ctx, input.URL, webFetchMaxBytes)
	if err != nil {
		return domain.ToolResult{}, err
	}
	text := extractReadableText(string(page))
	truncated := len(text) > input.MaxChars
	if truncated {
		text = text[:input.MaxChars] + "\n...[truncated]..."
	}
	return domain.ToolResult{
		Summary: fmt.Sprintf("Fetched %d characters from %s", len(text), input.URL),
		Data:    map[string]any{"url": input.URL, "content": text, "truncated": truncated},
	}, nil
}

// webFetchAllowPrivate 仅测试使用：允许 web_fetch 访问回环/私网地址（httptest 依赖 127.0.0.1）。
// 生产路径恒为 false，web_fetch 只允许公网。
var webFetchAllowPrivate bool

// validateWebURL 拒绝非 http/https scheme 与非公网地址（SSRF 防护）。
// 相比 paicli-go 原版（仅拦 non-global-unicast），这里额外拦截 RFC1918/ULA 私网段：
// 诊断 Agent 的内网访问必须走 profile 绑定的 nbi_request/netconf_rpc，不允许绕道 web_fetch。
func validateWebURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("blocked url scheme: %s", u.Scheme)
	}
	if host := u.Hostname(); host != "" {
		if ip := net.ParseIP(host); ip != nil && !isPublicWebIP(ip) {
			return fmt.Errorf("blocked non-public ip: %s", host)
		}
	}
	return nil
}

func isPublicWebIP(ip net.IP) bool {
	if webFetchAllowPrivate {
		return ip.IsGlobalUnicast()
	}
	return ip.IsGlobalUnicast() && !ip.IsPrivate()
}

// webSearch 按 paicli-go 的优先级选择搜索后端：SearxNG > SerpAPI > DuckDuckGo。
// 配置每次执行时读取环境变量，避免进程内缓存旧配置。
func webSearch(ctx context.Context, query string, n int) (string, error) {
	if n <= 0 || n > webSearchResultLimit {
		n = 5
	}
	if base := strings.TrimSpace(os.Getenv("SEARXNG_BASE_URL")); base != "" {
		return searxngSearch(ctx, base, query, n)
	}
	if key := strings.TrimSpace(os.Getenv("SERPAPI_API_KEY")); key != "" {
		return serpAPISearch(ctx, key, query, n)
	}
	return duckDuckGoSearch(ctx, query, n)
}

type webSearchRow struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

func searxngSearch(ctx context.Context, base, query string, n int) (string, error) {
	u, _ := url.Parse(strings.TrimRight(base, "/") + "/search")
	q := u.Query()
	q.Set("q", query)
	q.Set("format", "json")
	u.RawQuery = q.Encode()
	data, err := webGet(ctx, u.String(), webSearchMaxBytes)
	if err != nil {
		return "", err
	}
	var out struct {
		Results []webSearchRow `json:"results"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	return formatWebSearch(out.Results, n), nil
}

func serpAPISearch(ctx context.Context, key, query string, n int) (string, error) {
	u, _ := url.Parse("https://serpapi.com/search.json")
	q := u.Query()
	q.Set("q", query)
	q.Set("api_key", key)
	q.Set("num", fmt.Sprint(n))
	u.RawQuery = q.Encode()
	data, err := webGet(ctx, u.String(), webSearchMaxBytes)
	if err != nil {
		return "", err
	}
	var out struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic_results"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	rows := make([]webSearchRow, 0, len(out.Organic))
	for _, item := range out.Organic {
		rows = append(rows, webSearchRow{Title: item.Title, URL: item.Link, Content: item.Snippet})
	}
	return formatWebSearch(rows, n), nil
}

func duckDuckGoSearch(ctx context.Context, query string, n int) (string, error) {
	u, _ := url.Parse("https://duckduckgo.com/html/")
	q := u.Query()
	q.Set("q", query)
	u.RawQuery = q.Encode()
	data, err := webGet(ctx, u.String(), webSearchMaxBytes)
	if err != nil {
		return "", fmt.Errorf("web_search fallback failed; configure SERPAPI_API_KEY or SEARXNG_BASE_URL for a more reliable provider: %w", err)
	}
	return formatWebSearch(parseDuckDuckGoHTML(string(data)), n), nil
}

var webDuckTitleRE = regexp.MustCompile(`(?is)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
var webDuckSnippetRE = regexp.MustCompile(`(?is)<(?:a|div)[^>]+class="result__snippet"[^>]*>(.*?)</(?:a|div)>`)

func parseDuckDuckGoHTML(page string) []webSearchRow {
	indexes := webDuckTitleRE.FindAllStringSubmatchIndex(page, -1)
	rows := make([]webSearchRow, 0, len(indexes))
	for i, index := range indexes {
		if len(index) < 6 {
			continue
		}
		segmentEnd := len(page)
		if i+1 < len(indexes) {
			segmentEnd = indexes[i+1][0]
		}
		segment := page[index[1]:segmentEnd]
		row := webSearchRow{
			Title:   cleanWebSearchHTML(page[index[4]:index[5]]),
			URL:     decodeDuckDuckGoURL(html.UnescapeString(page[index[2]:index[3]])),
			Content: "",
		}
		if snippet := webDuckSnippetRE.FindStringSubmatch(segment); len(snippet) > 1 {
			row.Content = cleanWebSearchHTML(snippet[1])
		}
		if row.Title != "" && row.URL != "" {
			rows = append(rows, row)
		}
	}
	return rows
}

func decodeDuckDuckGoURL(raw string) string {
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if strings.Contains(u.Hostname(), "duckduckgo.com") {
		if target := u.Query().Get("uddg"); target != "" {
			if decoded, err := url.QueryUnescape(target); err == nil {
				return decoded
			}
			return target
		}
	}
	return raw
}

func cleanWebSearchHTML(fragment string) string {
	text := webTagRE.ReplaceAllString(fragment, " ")
	text = html.UnescapeString(text)
	return strings.Join(strings.Fields(text), " ")
}

func formatWebSearch(items []webSearchRow, n int) string {
	var b strings.Builder
	limit := n
	if len(items) < limit {
		limit = len(items)
	}
	for i := 0; i < limit; i++ {
		item := items[i]
		b.WriteString(fmt.Sprintf("%d. %s\n%s\n%s\n\n", i+1, item.Title, item.URL, item.Content))
	}
	if b.Len() == 0 {
		return "no results"
	}
	return strings.TrimSpace(b.String())
}

func webGet(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	if !webRateLimiter.allow() {
		return nil, errors.New("web request rate limit exceeded; slow down and retry later")
	}
	requestContext, cancel := context.WithTimeout(ctx, webRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "OLT-Diagnostic-Agent/1.0")
	// 使用共享的安全 client：DialContext 里会解析域名并固定公网 IP 拨号（防 DNS
	// rebinding），令牌桶在进入前已经限流。相比 http.DefaultClient，这里额外有 SSRF 防护。
	resp, err := webHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch failed: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}

var webScriptStyleRE = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>|<noscript[^>]*>.*?</noscript>`)
var webTagRE = regexp.MustCompile(`(?s)<[^>]+>`)
var webSpaceRE = regexp.MustCompile(`[ \t\r\f]+`)
var webBlankRE = regexp.MustCompile(`\n{3,}`)

func extractHTMLText(page string) string {
	page = webScriptStyleRE.ReplaceAllString(page, " ")
	page = strings.ReplaceAll(page, "</p>", "</p>\n")
	page = strings.ReplaceAll(page, "<br>", "\n")
	page = strings.ReplaceAll(page, "<br/>", "\n")
	text := webTagRE.ReplaceAllString(page, " ")
	replacer := strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'")
	text = replacer.Replace(text)
	text = webSpaceRE.ReplaceAllString(text, " ")
	text = webBlankRE.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

// Readability 两阶段正文提取（paicli-go 迁移增强）。
// 相比 extractHTMLText 的朴素去标签，这里先按语义标签定位正文容器，
// 再对块级元素做链接密度评分，丢弃导航/目录/页脚等低信息密度区域。
// 两个阶段都失败时回退到全页清理，保证任何输入都有非空输出。

var (
	webArticleRE    = regexp.MustCompile(`(?is)<article[^>]*>(.*?)</article>`)
	webMainRE       = regexp.MustCompile(`(?is)<main[^>]*>(.*?)</main>`)
	webRoleMainRE   = regexp.MustCompile(`(?is)<(?:div|section)[^>]+role\s*=\s*["']main["'][^>]*>(.*?)</(?:div|section)>`)
	webContentDivRE = regexp.MustCompile(`(?is)<div[^>]+(?:id|class)\s*=\s*["'](?:content|main|article|post|entry-content|article-content|articleBody)["'][^>]*>(.*?)</div>`)
	webBlockRE      = regexp.MustCompile(`(?is)<(p|div|section|article|li|blockquote|h[1-6])(\s[^>]*)?>(.*?)</\1>`)
	webAnchorRE     = regexp.MustCompile(`(?is)<a[^>]*>(.*?)</a>`)
)

// extractReadableText 是 web_fetch 使用的正文提取入口。
// 阶段 1 语义标签优先；阶段 2 链接密度评分；否则回退整体清理。
func extractReadableText(page string) string {
	if text := semanticMainText(page); text != "" {
		return text
	}
	if text := scoreBlockDensity(page); text != "" {
		return text
	}
	return extractHTMLText(page)
}

// semanticMainText 定位 <article>/<main>/role="main"/常见内容容器的第一个非空匹配。
func semanticMainText(page string) string {
	for _, re := range []*regexp.Regexp{webArticleRE, webMainRE, webRoleMainRE, webContentDivRE} {
		match := re.FindStringSubmatch(page)
		if len(match) < 2 {
			continue
		}
		if text := extractHTMLText(match[1]); len([]rune(strings.TrimSpace(text))) >= 80 {
			return text
		}
	}
	return ""
}

// scoreBlockDensity 按块级元素切分页面，保留链接密度 < 50% 且文本足够长的块。
// 链接密度 = 块内 <a> 锚文本长度 / 块文本总长度，用于剔除导航、目录、页脚。
func scoreBlockDensity(page string) string {
	blocks := webBlockRE.FindAllStringSubmatch(page, -1)
	kept := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if len(block) < 4 {
			continue
		}
		raw := block[3]
		text := extractHTMLText(raw)
		textLength := len([]rune(strings.TrimSpace(text)))
		if textLength < 40 {
			continue
		}
		linkLength := 0
		for _, anchor := range webAnchorRE.FindAllStringSubmatch(raw, -1) {
			if len(anchor) > 1 {
				linkLength += len([]rune(strings.TrimSpace(extractHTMLText(anchor[1]))))
			}
		}
		if float64(linkLength)/float64(textLength) < 0.5 {
			kept = append(kept, text)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n\n"))
}
