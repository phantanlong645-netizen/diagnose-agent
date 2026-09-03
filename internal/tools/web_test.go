package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSearchPrepareValidation(t *testing.T) {
	tool, err := NewWebSearchTool()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tool.Prepare(json.RawMessage(`{"query":""}`)); err == nil {
		t.Fatal("expected empty query to be rejected")
	}
	if _, err = tool.Prepare(json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected missing query to be rejected")
	}
	if _, err = tool.Prepare(json.RawMessage(`{"query":"x","maxResults":0}`)); err != nil {
		t.Fatalf("expected default maxResults, got %v", err)
	}
	if _, err = tool.Prepare(json.RawMessage(`{"query":"x","maxResults":11}`)); err == nil {
		t.Fatal("expected maxResults above 10 to be rejected")
	}
	prepared, err := tool.Prepare(json.RawMessage(`{"query":"ONU 注册失败 排查"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Annotations.ReadOnly || !prepared.Annotations.Idempotent || prepared.Annotations.Destructive {
		t.Fatalf("web_search must stay read-only: %+v", prepared.Annotations)
	}
}

func TestWebFetchPrepareValidation(t *testing.T) {
	tool, err := NewWebFetchTool()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"url":""}`,
		`{}`,
		`{"url":"ftp://example.com/doc"}`,
		`{"url":"file:///C:/windows/win.ini"}`,
		`{"url":"http://127.0.0.1:8080/admin"}`,
		`{"url":"http://169.254.169.254/latest/meta-data"}`,
		`{"url":"http://[::1]/metrics"}`,
		`{"url":"https://192.168.1.10/doc"}`, // RFC1918 私网也必须拦截
		`{"url":"https://example.com/doc","maxChars":999}`,
	} {
		if _, err = tool.Prepare(json.RawMessage(raw)); err == nil {
			t.Fatalf("expected %s to be rejected", raw)
		}
	}
	prepared, err := tool.Prepare(json.RawMessage(`{"url":"https://example.com/doc"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Annotations.ReadOnly || !prepared.Annotations.Idempotent {
		t.Fatalf("web_fetch must stay read-only: %+v", prepared.Annotations)
	}
}

func TestWebFetchExecuteExtractsReadableText(t *testing.T) {
	// httptest 监听在 127.0.0.1；仅测试打开私网开关，结束后恢复公网-only 策略。
	webFetchAllowPrivate = true
	defer func() { webFetchAllowPrivate = false }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><script>alert("x")</script><style>.a{}</style></head>
<body><h1>OLT 文档</h1><p>第一段&nbsp;内容</p><p>第二段<br/>换行</p></body></html>`))
	}))
	defer server.Close()

	tool, _ := NewWebFetchTool()
	prepared, err := tool.Prepare(json.RawMessage(`{"url":"` + server.URL + `/doc","maxChars":2000}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := result.Data.(map[string]any)
	content, _ := data["content"].(string)
	if !strings.Contains(content, "OLT 文档") || !strings.Contains(content, "第二段") {
		t.Fatalf("unexpected extracted content: %q", content)
	}
	if strings.Contains(content, "alert") || strings.Contains(content, ".a{}") {
		t.Fatalf("script/style must be stripped: %q", content)
	}
	if data["truncated"] == true {
		t.Fatal("small page must not be truncated")
	}
}

func TestWebFetchExecuteTruncatesLongPage(t *testing.T) {
	webFetchAllowPrivate = true
	defer func() { webFetchAllowPrivate = false }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<p>" + strings.Repeat("标准内容段落 ", 4000) + "</p>"))
	}))
	defer server.Close()

	tool, _ := NewWebFetchTool()
	prepared, err := tool.Prepare(json.RawMessage(`{"url":"` + server.URL + `","maxChars":2000}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := result.Data.(map[string]any)
	content, _ := data["content"].(string)
	if len(content) > 2000+len("\n...[truncated]...") {
		t.Fatalf("content not truncated to budget: %d", len(content))
	}
	if data["truncated"] != true {
		t.Fatal("expected truncated flag")
	}
}

func TestWebSearchExecuteViaSearxng(t *testing.T) {
	payload := `{"results":[{"title":"GPON 标准","url":"https://example.com/gpon","content":"ITU-T G.984 摘要"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" || r.URL.Query().Get("q") == "" {
			t.Errorf("unexpected searxng request: %s", r.URL)
		}
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()
	t.Setenv("SEARXNG_BASE_URL", server.URL)
	t.Setenv("SERPAPI_API_KEY", "")

	tool, _ := NewWebSearchTool()
	prepared, err := tool.Prepare(json.RawMessage(`{"query":"GPON ONU 注册"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := result.Data.(map[string]any)
	results, _ := data["results"].(string)
	if !strings.Contains(results, "https://example.com/gpon") || !strings.Contains(results, "ITU-T G.984") {
		t.Fatalf("unexpected search results: %q", results)
	}
}

func TestWebSearchExecuteReportsProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("SEARXNG_BASE_URL", server.URL)
	t.Setenv("SERPAPI_API_KEY", "")

	tool, _ := NewWebSearchTool()
	prepared, err := tool.Prepare(json.RawMessage(`{"query":"GPON"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tool.Execute(context.Background(), prepared); err == nil {
		t.Fatal("expected provider failure to surface as tool error")
	}
}

func TestParseDuckDuckGoHTML(t *testing.T) {
	page := `<a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Folt-doc&amp;rut=abc">OLT <b>文档</b></a>
	<a class="result__snippet">ITU-T G.984 系列标准说明</a>
	<a rel="nofollow" class="result__a" href="https://direct.example.com/page">Direct result</a>`
	rows := parseDuckDuckGoHTML(page)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].URL != "https://example.com/olt-doc" {
		t.Fatalf("uddg redirect not decoded: %q", rows[0].URL)
	}
	if rows[0].Title != "OLT 文档" || rows[0].Content != "ITU-T G.984 系列标准说明" {
		t.Fatalf("unexpected row: %+v", rows[0])
	}
	if rows[1].URL != "https://direct.example.com/page" {
		t.Fatalf("unexpected direct url: %q", rows[1].URL)
	}
}

func TestExtractHTMLTextDropsScriptAndTags(t *testing.T) {
	got := extractHTMLText(`<html><script>alert(1)</script><style>x{}</style><body><h1>标题</h1><p>Hello&nbsp;World</p></body></html>`)
	if strings.Contains(got, "alert") || strings.Contains(got, "<h1>") {
		t.Fatalf("html cleanup leaked tags/scripts: %q", got)
	}
	if !strings.Contains(got, "标题") || !strings.Contains(got, "Hello World") {
		t.Fatalf("html cleanup lost body text: %q", got)
	}
}

// 保证 web 工具能被 OLT Registry 注册并出现在定义列表里（接口契约回归）。
func TestWebToolsSatisfyRegistryContract(t *testing.T) {
	registry := NewRegistry()
	searchTool, err := NewWebSearchTool()
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register(searchTool); err != nil {
		t.Fatal(err)
	}
	fetchTool, err := NewWebFetchTool()
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register(fetchTool); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, definition := range registry.Definitions() {
		names[definition.Name] = true
	}
	if !names["web_search"] || !names["web_fetch"] {
		t.Fatalf("web tools missing from registry: %v", names)
	}
}
