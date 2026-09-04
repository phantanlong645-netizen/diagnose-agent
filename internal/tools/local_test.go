package tools

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"olt-diagnostic-agent/internal/domain"
)

type fixedNETCONFTransport struct {
	response NETCONFRPCResponse
	err      error
}

type fixedToolTargetResolver struct {
	nbi   domain.NBITarget
	roots []string
}

func (r fixedToolTargetResolver) NBI(string) (domain.NBITarget, bool) {
	return r.nbi, r.nbi.BaseURL != ""
}

func (fixedToolTargetResolver) NETCONF(string) (domain.NETCONFTarget, bool) {
	return domain.NETCONFTarget{}, false
}

func (fixedToolTargetResolver) NETCONFEndpoints(string) ([]domain.NETCONFEndpoint, bool) {
	return nil, false
}

func (r fixedToolTargetResolver) WorkspaceRoots(string) ([]string, bool) {
	return r.roots, len(r.roots) > 0
}

func (t fixedNETCONFTransport) ExecuteRPC(context.Context, domain.NETCONFTarget, string, time.Duration) (NETCONFRPCResponse, error) {
	return t.response, t.err
}

func TestFilePatternMatchesRelativePathCaseInsensitive(t *testing.T) {
	root := `C:\workspace\access-console_repo`
	file := `C:\workspace\access-console_repo\server\internal\routers\nbi\provisioning.go`
	if !filePatternMatches(`server/**/PROVISIONING.GO`, root, file, "provisioning.go") {
		t.Fatal("expected relative path glob to match case-insensitively")
	}
	if !filePatternMatches(`server/**/PROVISIONING.GO`, root, `C:\workspace\access-console_repo\server\provisioning.go`, "provisioning.go") {
		t.Fatal("expected globstar to match zero directories")
	}
	if filePatternMatches(`client/**/*.tsx`, root, file, "provisioning.go") {
		t.Fatal("unexpected path glob match")
	}
}

func TestMatchingLineRanksExactAndMultiTermMatches(t *testing.T) {
	content := "status is available\nGET /northbound/onu/devices returns status\nstatus only"
	line, _, found := matchingLine(content, "GET onu devices")
	if !found || line != 2 {
		t.Fatalf("expected line 2, got line=%d found=%v", line, found)
	}
}

func TestRipgrepSearchKeepsDetailedDocumentMatches(t *testing.T) {
	arguments := strings.Join(buildRipgrepArgs(FileSearchRequest{
		Pattern: "REST_API_Doc_V0618.md",
		Query:   "POST /northbound/service/service_instances/:sn_ip",
	}, `C:\workspace`), " ")
	if !strings.Contains(arguments, "--max-count 10") {
		t.Fatalf("API document search still keeps only its first match: %s", arguments)
	}
}

func TestParseRipgrepLinePreservesAPIPathParameters(t *testing.T) {
	match, ok := parseRipgrepLine(`C:\workspace\REST_API_Doc_V0618.md:7255:### POST /northbound/service/service_instances/:sn_ip`, "POST")
	if !ok || match["path"] != `C:\workspace\REST_API_Doc_V0618.md` || match["line"] != "7255" ||
		match["snippet"] != "### POST /northbound/service/service_instances/:sn_ip" {
		t.Fatalf("failed to parse API document match containing a path parameter: %+v", match)
	}
}

func TestFileReadToolReadsFocusedLineRange(t *testing.T) {
	root := t.TempDir()
	document := filepath.Join(root, "REST_API_Doc_V0618.md")
	if err := os.WriteFile(document, []byte("toc\nsummary\nrequest\nbody\nresponse\nnext\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool, err := NewFileReadTool(fixedToolTargetResolver{roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	arguments, _ := json.Marshal(FileReadRequest{
		ProfileID: "lab",
		Path:      document,
		StartLine: 3,
		MaxLines:  2,
		MaxBytes:  1024,
	})
	prepared, err := tool.Prepare(arguments)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected file-read result: %T", result.Data)
	}
	if data["content"] != "request\nbody" || data["startLine"] != 3 || data["endLine"] != 4 || data["truncated"] != true {
		t.Fatalf("unexpected focused line range: %+v", data)
	}
}

func TestCloseNETCONFSession10SendsCloseAndWaitsForOK(t *testing.T) {
	reply := `<rpc-reply xmlns="urn:ietf:params:xml:ns:netconf:base:1.0" message-id="diagnostic-close-session"><ok/></rpc-reply>`
	reader := bufio.NewReader(strings.NewReader(reply + netconf10Delimiter))
	var sent bytes.Buffer

	if err := closeNETCONFSession(&sent, reader, "1.0"); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"<close-session/>", `message-id="diagnostic-close-session"`, netconf10Delimiter} {
		if !strings.Contains(sent.String(), expected) {
			t.Fatalf("NETCONF 1.0 close request is missing %q: %s", expected, sent.String())
		}
	}
}

func TestCloseNETCONFSession11UsesChunkedFraming(t *testing.T) {
	reply := `<rpc-reply xmlns="urn:ietf:params:xml:ns:netconf:base:1.0" message-id="diagnostic-close-session"><ok/></rpc-reply>`
	framedReply := fmt.Sprintf("\n#%d\n%s\n##\n", len(reply), reply)
	reader := bufio.NewReader(strings.NewReader(framedReply))
	var sent bytes.Buffer

	if err := closeNETCONFSession(&sent, reader, "1.1"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sent.String(), "\n#") || !strings.HasSuffix(sent.String(), "\n##\n") {
		t.Fatalf("unexpected NETCONF 1.1 close framing: %q", sent.String())
	}
	if !strings.Contains(sent.String(), "<close-session/>") {
		t.Fatalf("NETCONF 1.1 close request is missing operation: %s", sent.String())
	}
}

func TestValidateNETCONFCloseReplyRejectsMismatchAndNonOK(t *testing.T) {
	for name, reply := range map[string]string{
		"message ID mismatch": `<rpc-reply xmlns="urn:ietf:params:xml:ns:netconf:base:1.0" message-id="other"><ok/></rpc-reply>`,
		"rpc error":           `<rpc-reply xmlns="urn:ietf:params:xml:ns:netconf:base:1.0" message-id="diagnostic-close-session"><rpc-error><error-tag>operation-failed</error-tag></rpc-error></rpc-reply>`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateNETCONFCloseReply([]byte(reply), netconfCloseMessageID); err == nil {
				t.Fatal("expected close-session reply validation to fail")
			}
		})
	}
}

func TestNETCONFToolPreservesBusinessReplyWhenCloseFails(t *testing.T) {
	tool := &NETCONFTool{transport: fixedNETCONFTransport{response: NETCONFRPCResponse{
		SessionID:  "1000",
		MessageID:  "business-1",
		Reply:      `<rpc-reply message-id="business-1"><data><status>up</status></data></rpc-reply>`,
		CloseError: "read close reply: EOF",
	}}}
	call := domain.PreparedCall{Input: preparedNETCONFRPC{
		Request: NETCONFRPCRequest{RPC: `<rpc message-id="business-1"><get><filter><status/></filter></get></rpc>`},
	}}

	result, err := tool.Execute(context.Background(), call)
	if err != nil {
		t.Fatalf("business result must survive a close-session warning: %v", err)
	}
	response, ok := result.Data.(NETCONFRPCResponse)
	if !ok || !strings.Contains(response.Reply, "<status>up</status>") {
		t.Fatalf("business reply was not preserved: %+v", result.Data)
	}
	if !strings.Contains(result.Message, "Do not repeat the business RPC") {
		t.Fatalf("cleanup warning does not prevent a business retry: %q", result.Message)
	}
	if result.Metadata["netconf.close_acknowledged"] != "false" {
		t.Fatalf("unexpected close acknowledgement metadata: %+v", result.Metadata)
	}
}

func TestAccessConsoleLogsToolDownloadsFiltersAndReusesAuthentication(t *testing.T) {
	archive := testLogArchive(t, map[string]string{
		"olt.log": "2026-08-11 12:00:00.000001 [OLT] INFO source.go:10 - startup complete\n" +
			"2026-08-11 12:01:00.000002 [OLT] ERROR source.go:20 - delete AVC failed token=secret-value\n",
		"ont.log": "2026-08-11 12:02:00.000003 [ONT] INFO source.go:30 - unrelated ONU event\n",
	})
	var loginCalls atomic.Int32
	var downloadCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/northbound/auth/login":
			loginCalls.Add(1)
			username, password, ok := request.BasicAuth()
			if !ok || username != "operator" || password != "password" {
				http.Error(response, "bad credentials", http.StatusUnauthorized)
				return
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"token":"cached-jwt"}`))
		case "/nms/v1/log/download":
			downloadCalls.Add(1)
			if request.Header.Get("token") != "cached-jwt" {
				response.WriteHeader(http.StatusUnauthorized)
				_, _ = response.Write([]byte(`{"status":20001}`))
				return
			}
			response.Header().Set("Content-Type", "application/zip")
			_, _ = response.Write(archive)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	nbiTool, err := NewNBITool(fixedToolTargetResolver{nbi: domain.NBITarget{
		BaseURL:     server.URL,
		Username:    "operator",
		Password:    "password",
		TokenHeader: "token",
	}}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	logTool, err := NewAccessConsoleLogsTool(nbiTool)
	if err != nil {
		t.Fatal(err)
	}
	arguments, _ := json.Marshal(AccessConsoleLogsRequest{
		ProfileID:    "lab",
		LogNames:     []string{"olt.log"},
		Keywords:     []string{"AVC", "token=secret-value"},
		ContextLines: 1,
		MaxMatches:   20,
	})
	prepared, err := logTool.Prepare(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Annotations.ReadOnly || prepared.Annotations.Idempotent {
		t.Fatalf("logs must be read-only but fresh: %+v", prepared.Annotations)
	}
	if strings.Contains(prepared.Summary, "secret-value") || strings.Contains(prepared.Preview, "secret-value") {
		t.Fatalf("log keyword secret leaked into the prepared call: summary=%q preview=%q", prepared.Summary, prepared.Preview)
	}

	for range 2 {
		result, executeErr := logTool.Execute(context.Background(), prepared)
		if executeErr != nil {
			t.Fatal(executeErr)
		}
		logs, ok := result.Data.(AccessConsoleLogsResponse)
		if !ok {
			t.Fatalf("unexpected log result type: %T", result.Data)
		}
		if len(logs.Excerpts) != 1 || !strings.Contains(logs.Excerpts[0].Text, "delete AVC failed") {
			t.Fatalf("expected focused AVC excerpt, got %+v", logs.Excerpts)
		}
		if strings.Contains(logs.Excerpts[0].Text, "secret-value") || !strings.Contains(logs.Excerpts[0].Text, "[REDACTED]") {
			t.Fatalf("log secret was not redacted: %s", logs.Excerpts[0].Text)
		}
		if logs.ArchiveSHA256 == "" || logs.ArchiveBytes != len(archive) {
			t.Fatalf("archive metadata is incomplete: %+v", logs)
		}
		if strings.Contains(strings.Join(logs.Keywords, ","), "secret-value") {
			t.Fatalf("log keyword secret leaked into evidence: %+v", logs.Keywords)
		}
	}
	if loginCalls.Load() != 1 || downloadCalls.Load() != 2 {
		t.Fatalf("expected one login and two fresh downloads, got login=%d download=%d", loginCalls.Load(), downloadCalls.Load())
	}
}

func TestAccessConsoleLogsToolRejectsUnknownLogName(t *testing.T) {
	nbiTool, err := NewNBITool(fixedToolTargetResolver{nbi: domain.NBITarget{BaseURL: "https://127.0.0.1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logTool, err := NewAccessConsoleLogsTool(nbiTool)
	if err != nil {
		t.Fatal(err)
	}
	arguments, _ := json.Marshal(AccessConsoleLogsRequest{ProfileID: "lab", LogNames: []string{"../../secret"}})
	if _, err = logTool.Prepare(arguments); err == nil || !strings.Contains(err.Error(), "unsupported Access Console log") {
		t.Fatalf("expected log allowlist rejection, got %v", err)
	}
}

func TestNBIToolRefreshesJWTBeforeAcquiringWritePermission(t *testing.T) {
	var loginCalls atomic.Int32
	var leaseToken string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/northbound/auth/login":
			loginNumber := loginCalls.Add(1)
			response.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(response, `{"token":"jwt-%d"}`, loginNumber)
		case "/northbound/olt/devices":
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"data":[]}`))
		case "/northbound/auth/permissions":
			requestToken := request.Header.Get("token")
			leaseToken = requestToken
			// Reproduce the Access Console renewal race: an old cached JWT is
			// renewed in the permissions response while the lease remains bound
			// to the request token.
			if requestToken == "jwt-1" {
				response.Header().Set("newtoken", "jwt-2")
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"message":"You got the permission!"}`))
		case "/northbound/service/service_instances/device":
			response.Header().Set("Content-Type", "application/json")
			if request.Header.Get("token") != leaseToken {
				response.WriteHeader(http.StatusForbidden)
				_, _ = response.Write([]byte(`{"message":"Write window is occupied, only GET allowed"}`))
				return
			}
			_, _ = response.Write([]byte(`{"message":"write accepted"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	tool, err := NewNBITool(fixedToolTargetResolver{nbi: domain.NBITarget{
		BaseURL:     server.URL,
		Username:    "operator",
		Password:    "password",
		TokenHeader: "token",
	}}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	execute := func(method, path, body string) NBIResponse {
		t.Helper()
		arguments, marshalErr := json.Marshal(NBIRequest{ProfileID: "lab", Method: method, Path: path, Body: body})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		prepared, prepareErr := tool.Prepare(arguments)
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		result, executeErr := tool.Execute(context.Background(), prepared)
		if executeErr != nil {
			t.Fatal(executeErr)
		}
		responseData, ok := result.Data.(NBIResponse)
		if !ok {
			t.Fatalf("unexpected NBI result type: %T", result.Data)
		}
		return responseData
	}

	if response := execute(http.MethodGet, "/northbound/olt/devices", ""); response.StatusCode != http.StatusOK {
		t.Fatalf("initial read failed: %+v", response)
	}
	if response := execute(http.MethodPost, "/northbound/auth/permissions", `{"Duration":"600"}`); response.StatusCode != http.StatusOK {
		t.Fatalf("permission acquisition failed: %+v", response)
	}
	if response := execute(http.MethodPost, "/northbound/service/service_instances/device", `{}`); response.StatusCode != http.StatusOK {
		t.Fatalf("write did not reuse the lease JWT: %+v", response)
	}
	if loginCalls.Load() != 2 || leaseToken != "jwt-2" {
		t.Fatalf("expected a fresh JWT before permission acquisition, logins=%d leaseToken=%q", loginCalls.Load(), leaseToken)
	}
}

type recordingMCPClient struct {
	results       map[string]json.RawMessage
	calls         []string
	notifications []string
	protocol      string
}

func (c *recordingMCPClient) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	c.calls = append(c.calls, method)
	return c.results[method], nil
}

func (c *recordingMCPClient) Notify(_ context.Context, method string, _ any) error {
	c.notifications = append(c.notifications, method)
	return nil
}

func (c *recordingMCPClient) SetProtocolVersion(version string) { c.protocol = version }
func (c *recordingMCPClient) Close() error                      { return nil }

func TestInitializeMCPSendsInitializedNotification(t *testing.T) {
	client := &recordingMCPClient{results: map[string]json.RawMessage{
		"initialize": json.RawMessage(`{"protocolVersion":"2025-03-26"}`),
	}}
	if err := initializeMCP(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 1 || client.calls[0] != "initialize" {
		t.Fatalf("unexpected calls: %v", client.calls)
	}
	if len(client.notifications) != 1 || client.notifications[0] != "notifications/initialized" {
		t.Fatalf("initialized notification missing: %v", client.notifications)
	}
	if client.protocol != "2025-03-26" {
		t.Fatalf("negotiated protocol was not retained: %q", client.protocol)
	}
}

func TestMCPToolPoliciesAreHostAuthoritativeForTeamRoles(t *testing.T) {
	disabled := false
	client := &recordingMCPClient{results: map[string]json.RawMessage{
		"tools/list": json.RawMessage(`{"tools":[
			{"name":"unclassified","description":"unknown safety","inputSchema":{"type":"object"}},
			{"name":"lookup_ont","description":"read inventory","inputSchema":{"type":"object"}},
			{"name":"browser_control","description":"can click","inputSchema":{"type":"object"}},
			{"name":"disabled_tool","description":"off","inputSchema":{"type":"object"}}
		]}`),
	}}
	policies := map[string]mcpToolPolicy{
		"lookup_ont":    {ReadOnly: true, Idempotent: true, TeamRoles: []string{"platform"}},
		"disabled_tool": {Enabled: &disabled},
	}
	loaded, err := listMCPTools(context.Background(), "inventory", policies, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 3 {
		t.Fatalf("expected three enabled tools, got %d", len(loaded))
	}
	registry := NewRegistry()
	for _, tool := range loaded {
		if err = registry.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	platform := registry.MCPToolInfosForRole("platform")
	if len(platform) != 1 || platform[0].RemoteName != "lookup_ont" {
		t.Fatalf("unexpected platform MCP tools: %+v", platform)
	}
	if got := registry.MCPToolInfosForRole("source"); len(got) != 0 {
		t.Fatalf("unclassified MCP tool leaked into source role: %+v", got)
	}
	_, prepared, err := registry.Prepare(domain.ToolCall{Name: "mcp__inventory__unclassified", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Annotations.ReadOnly || !prepared.Annotations.OpenWorld {
		t.Fatalf("unclassified tool must remain approval-gated open-world: %+v", prepared.Annotations)
	}
}

func TestValidateMCPToolPoliciesRejectsUnsafeTeamGrant(t *testing.T) {
	err := validateMCPToolPolicies(map[string]mcpToolPolicy{
		"click": {TeamRoles: []string{"web"}},
	})
	if err == nil || !strings.Contains(err.Error(), "readOnly=true") {
		t.Fatalf("expected unsafe team grant rejection, got %v", err)
	}
}

func TestLoadMCPReportsHTTPStatusAndUsesNegotiatedSession(t *testing.T) {
	var initialized atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var message struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Errorf("decode MCP request: %v", err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		switch message.Method {
		case "initialize":
			response.Header().Set("Mcp-Session-Id", "session-1")
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`))
		case "notifications/initialized":
			if request.Header.Get("Mcp-Session-Id") != "session-1" || request.Header.Get("MCP-Protocol-Version") != "2025-03-26" {
				t.Errorf("notification missing negotiated headers: session=%q protocol=%q", request.Header.Get("Mcp-Session-Id"), request.Header.Get("MCP-Protocol-Version"))
			}
			initialized.Store(true)
			response.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if !initialized.Load() {
				t.Error("tools/list arrived before initialized notification")
			}
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = response.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"tools\":[{\"name\":\"lookup_ont\",\"inputSchema\":{\"type\":\"object\"}}]}}\n\n"))
		default:
			t.Errorf("unexpected MCP method %q", message.Method)
			response.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "mcp.json")
	configuration, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"inventory": map[string]any{
			"url": server.URL,
			"toolPolicies": map[string]any{"lookup_ont": map[string]any{
				"readOnly": true, "idempotent": true, "teamRoles": []string{"platform"},
			}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, configuration, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := LoadMCP(context.Background(), configPath)
	defer manager.Close()
	status := manager.Status()
	if !status.Configured || status.ConfigError != "" || len(status.Servers) != 1 {
		t.Fatalf("unexpected MCP status: %+v", status)
	}
	if !status.Servers[0].Connected || status.Servers[0].ToolCount != 1 || status.Servers[0].Transport != "http" {
		t.Fatalf("unexpected MCP server status: %+v", status.Servers[0])
	}
	if len(manager.Tools()) != 1 || len(manager.Tools()[0].teamRoles) != 1 || manager.Tools()[0].teamRoles[0] != "platform" {
		t.Fatalf("unexpected discovered MCP tools: %+v", manager.Tools())
	}
}

func TestStdioMCPClientPairsConcurrentResponsesByID(t *testing.T) {
	serverRequests, clientInput := io.Pipe()
	clientOutput, serverResponses := io.Pipe()
	client := &stdioMCPClient{
		stdin: clientInput, pending: make(map[string]chan mcpResponse), done: make(chan struct{}),
	}
	responseScanner := bufio.NewScanner(clientOutput)
	go client.readResponses(responseScanner)
	defer func() {
		_ = client.Close()
		_ = serverRequests.Close()
		_ = serverResponses.Close()
	}()

	go func() {
		scanner := bufio.NewScanner(serverRequests)
		requests := make([]map[string]any, 0, 2)
		for scanner.Scan() {
			var request map[string]any
			if json.Unmarshal(scanner.Bytes(), &request) == nil {
				requests = append(requests, request)
			}
			if len(requests) == 2 {
				for index := len(requests) - 1; index >= 0; index-- {
					response, _ := json.Marshal(map[string]any{
						"jsonrpc": "2.0", "id": requests[index]["id"],
						"result": map[string]any{"method": requests[index]["method"]},
					})
					_, _ = serverResponses.Write(append(response, '\n'))
				}
				return
			}
		}
	}()

	results := make(map[string]string, 2)
	var resultMu sync.Mutex
	var wait sync.WaitGroup
	for _, method := range []string{"first", "second"} {
		method := method
		wait.Add(1)
		go func() {
			defer wait.Done()
			raw, err := client.Call(context.Background(), method, map[string]any{})
			if err != nil {
				t.Errorf("%s call failed: %v", method, err)
				return
			}
			var result struct {
				Method string `json:"method"`
			}
			if err = json.Unmarshal(raw, &result); err != nil {
				t.Errorf("decode %s result: %v", method, err)
				return
			}
			resultMu.Lock()
			results[method] = result.Method
			resultMu.Unlock()
		}()
	}
	wait.Wait()
	if results["first"] != "first" || results["second"] != "second" {
		t.Fatalf("responses were mispaired: %+v", results)
	}
}

func testLogArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
