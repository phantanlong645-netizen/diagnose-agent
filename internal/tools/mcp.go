package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"olt-diagnostic-agent/internal/domain"
)

// mcp.go 把 paicli-go 的 MCP 能力（stdio/HTTP 双传输 + Schema 裁剪 + HITL 审批）
// 迁移到 OLT 工具链。MCP 工具是对外部系统的开放调用；未分类工具由
// policy.Evaluate 要求用户审批，只有本机策略明确分类的只读工具可自动取证。
//
// 传输层：
//   - stdio：拉起子进程，走 JSON-RPC 2.0（换行分隔），支持 args/env 变量展开；
//   - HTTP：POST JSON-RPC 2.0，保留协商出的会话/协议头并解析 JSON 或 SSE 响应。
//
// Schema 裁剪：MCP server 返回的 inputSchema 直接透传给模型会导致上下文膨胀，
// 这里做最小裁剪——缺失时补成 object、剥掉 $schema 这类元噪声，其余原样保留。

// mcpConfigFile 是 mcp.json 的顶层结构，与 paicli-go 的配置格式对齐。
type mcpConfigFile struct {
	MCPServers map[string]mcpServerConfig `json:"mcpServers"`
}

// mcpServerConfig 描述单个 MCP server 的启动方式：stdio 子进程（command/args/env）或
// HTTP 端点（url/headers），两者至少配置其一。
type mcpServerConfig struct {
	Command      string                   `json:"command"`
	Args         []string                 `json:"args"`
	URL          string                   `json:"url"`
	Headers      map[string]string        `json:"headers"`
	Env          map[string]string        `json:"env"`
	ToolPolicies map[string]mcpToolPolicy `json:"toolPolicies"`
}

// mcpToolPolicy is host-authoritative. Remote MCP annotations never grant a
// tool access to an isolated Team worker.
type mcpToolPolicy struct {
	Enabled    *bool    `json:"enabled,omitempty"`
	ReadOnly   bool     `json:"readOnly,omitempty"`
	Idempotent bool     `json:"idempotent,omitempty"`
	Sensitive  bool     `json:"sensitive,omitempty"`
	TeamRoles  []string `json:"teamRoles,omitempty"`
}

var validMCPTeamRoles = map[string]bool{
	"device": true, "platform": true, "source": true, "web": true, "correlator": true,
}

// mcpClient 抽象 stdio / HTTP 两种传输，Call 返回 result 字段的原始 JSON。
type mcpClient interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	Notify(ctx context.Context, method string, params any) error
	Close() error
}

type mcpProtocolClient interface {
	SetProtocolVersion(version string)
}

// MCPTool 是一个动态生成的工具，实现 Registry 的 Tool 接口。
// 它的 name 是 mcp__<server>__<remote>，执行时把参数透传给远端 tools/call。
type MCPTool struct {
	serverName  string
	remoteName  string
	localName   string
	description string
	schema      map[string]any
	annotations domain.ToolAnnotations
	teamRoles   []string
	client      mcpClient
}

// MCPToolInfo 是 agent.Engine 构建 Eino 动态工具所需的最小描述。
// 不暴露 client：Engine 通过 runner.Execute -> Registry -> MCPTool.Execute 执行，
// 这样审批、事件、evidence 记录都复用现有流水线。
type MCPToolInfo struct {
	Name        string                 `json:"name"`
	ServerName  string                 `json:"serverName"`
	RemoteName  string                 `json:"remoteName"`
	Description string                 `json:"description"`
	InputSchema map[string]any         `json:"inputSchema"`
	Annotations domain.ToolAnnotations `json:"annotations"`
	TeamRoles   []string               `json:"teamRoles,omitempty"`
}

func (t *MCPTool) ToolInfo() MCPToolInfo {
	return MCPToolInfo{
		Name: t.localName, ServerName: t.serverName, RemoteName: t.remoteName,
		Description: t.description, InputSchema: t.schema, Annotations: t.annotations,
		TeamRoles: append([]string(nil), t.teamRoles...),
	}
}

func (t *MCPTool) Definition() Definition {
	return Definition{Name: t.localName, Description: t.description}
}

func (t *MCPTool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var args map[string]any
	if err := json.Unmarshal(arguments, &args); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode MCP tool arguments: %w", err)
	}
	return domain.PreparedCall{
		Summary:     fmt.Sprintf("Call MCP tool %s.%s", t.serverName, t.remoteName),
		Annotations: t.annotations,
		Input:       args,
	}, nil
}

func (t *MCPTool) Execute(ctx context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	args, ok := call.Input.(map[string]any)
	if !ok {
		return domain.ToolResult{}, fmt.Errorf("invalid prepared MCP tool input")
	}
	raw, err := t.client.Call(ctx, "tools/call", map[string]any{"name": t.remoteName, "arguments": args})
	if err != nil {
		return domain.ToolResult{}, err
	}
	var callResult struct {
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(raw, &callResult) == nil && callResult.IsError {
		return domain.ToolResult{}, fmt.Errorf("MCP tool %s.%s reported failure: %s", t.serverName, t.remoteName, flattenMCPContent(raw))
	}
	return domain.ToolResult{
		Summary: fmt.Sprintf("MCP tool %s.%s completed", t.serverName, t.remoteName),
		Data:    map[string]any{"result": flattenMCPContent(raw)},
	}, nil
}

// MCPManager 持有已连接的 MCP 客户端和生成的工具，负责生命周期收尾。
type MCPManager struct {
	clients []mcpClient
	tools   []*MCPTool
	status  domain.MCPStatus
}

// LoadMCP 从 configPath 读取 mcp.json 并连接所有 MCP server。单个 server 连接
// 失败只跳过，不影响其它 server 或应用启动。返回的 manager 永不为 nil。
func LoadMCP(ctx context.Context, configPath string) *MCPManager {
	manager := &MCPManager{status: domain.MCPStatus{ConfigPath: configPath}}
	servers, configured, err := loadMCPConfig(configPath)
	manager.status.Configured = configured
	if err != nil {
		manager.status.ConfigError = safeMCPStatusError(err)
		return manager
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg := servers[name]
		status := domain.MCPServerStatus{Name: name, Transport: mcpTransportName(cfg)}
		if err = validateMCPToolPolicies(cfg.ToolPolicies); err != nil {
			status.Error = safeMCPStatusError(err)
			manager.status.Servers = append(manager.status.Servers, status)
			continue
		}
		client, err := startMCPServer(ctx, cfg)
		if err != nil {
			status.Error = safeMCPStatusError(err)
			manager.status.Servers = append(manager.status.Servers, status)
			continue
		}
		probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
		if err = initializeMCP(probeCtx, client); err != nil {
			cancelProbe()
			status.Error = safeMCPStatusError(err)
			_ = client.Close()
			manager.status.Servers = append(manager.status.Servers, status)
			continue
		}
		serverTools, listErr := listMCPTools(probeCtx, name, cfg.ToolPolicies, client)
		cancelProbe()
		if listErr != nil {
			status.Error = safeMCPStatusError(listErr)
			_ = client.Close()
			manager.status.Servers = append(manager.status.Servers, status)
			continue
		}
		status.Connected = true
		status.ToolCount = len(serverTools)
		manager.clients = append(manager.clients, client)
		manager.tools = append(manager.tools, serverTools...)
		manager.status.Servers = append(manager.status.Servers, status)
	}
	return manager
}

func (m *MCPManager) Tools() []*MCPTool {
	if m == nil {
		return nil
	}
	return m.tools
}

// Status returns a defensive copy suitable for readiness/UI reporting.
func (m *MCPManager) Status() domain.MCPStatus {
	if m == nil {
		return domain.MCPStatus{}
	}
	status := m.status
	status.Servers = append([]domain.MCPServerStatus(nil), m.status.Servers...)
	return status
}

func (m *MCPManager) Close() {
	if m == nil {
		return
	}
	for _, client := range m.clients {
		_ = client.Close()
	}
}

func loadMCPConfig(configPath string) (map[string]mcpServerConfig, bool, error) {
	if configPath == "" {
		return nil, false, nil
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("read MCP configuration: %w", err)
	}
	var cfg mcpConfigFile
	if err = json.Unmarshal(content, &cfg); err != nil {
		return nil, true, fmt.Errorf("decode MCP configuration: %w", err)
	}
	merged := make(map[string]mcpServerConfig, len(cfg.MCPServers))
	for name, server := range cfg.MCPServers {
		expandMCPVars(&server)
		merged[name] = server
	}
	return merged, true, nil
}

// startMCPServer 按配置选择传输：配置了 command 走 stdio，否则配置了 url 走 HTTP，两者皆无则报错。
func startMCPServer(ctx context.Context, cfg mcpServerConfig) (mcpClient, error) {
	if strings.TrimSpace(cfg.Command) != "" {
		return startStdioMCP(ctx, cfg)
	}
	if strings.TrimSpace(cfg.URL) != "" {
		return &httpMCPClient{url: cfg.URL, headers: cfg.Headers, http: &http.Client{Timeout: 60 * time.Second}}, nil
	}
	return nil, fmt.Errorf("mcp server missing command or url")
}

func initializeMCP(ctx context.Context, client mcpClient) error {
	raw, err := client.Call(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "olt-diagnostic-agent", "version": "1.0.0"},
	})
	if err != nil {
		return fmt.Errorf("initialize MCP server: %w", err)
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(raw, &initialized) == nil && initialized.ProtocolVersion != "" {
		if protocolClient, ok := client.(mcpProtocolClient); ok {
			protocolClient.SetProtocolVersion(initialized.ProtocolVersion)
		}
	}
	if err = client.Notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return fmt.Errorf("notify MCP server initialized: %w", err)
	}
	return nil
}

func listMCPTools(ctx context.Context, serverName string, policies map[string]mcpToolPolicy, client mcpClient) ([]*MCPTool, error) {
	raw, err := client.Call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("list MCP tools: %w", err)
	}
	var out struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode MCP tool list: %w", err)
	}
	tools := make([]*MCPTool, 0, len(out.Tools))
	for _, item := range out.Tools {
		if strings.TrimSpace(item.Name) == "" {
			continue
		}
		policy := policies[item.Name]
		if policy.Enabled != nil && !*policy.Enabled {
			continue
		}
		teamRoles := normalizedMCPTeamRoles(policy.TeamRoles)
		if !policy.ReadOnly || policy.Sensitive {
			teamRoles = nil
		}
		localName := "mcp__" + sanitizeMCPName(serverName) + "__" + sanitizeMCPName(item.Name)
		tools = append(tools, &MCPTool{
			serverName:  serverName,
			remoteName:  item.Name,
			localName:   localName,
			description: fmt.Sprintf("MCP tool %s.%s: %s", serverName, item.Name, strings.TrimSpace(item.Description)),
			schema:      sanitizeMCPSchema(item.InputSchema),
			annotations: domain.ToolAnnotations{ReadOnly: policy.ReadOnly, Idempotent: policy.Idempotent, OpenWorld: true, Sensitive: policy.Sensitive},
			teamRoles:   teamRoles,
			client:      client,
		})
	}
	return tools, nil
}

// stdioMCPClient 通过子进程 stdin/stdout 跑 JSON-RPC 2.0。
type stdioMCPClient struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan mcpResponse
	done      chan struct{}
	finish    sync.Once
	errMu     sync.Mutex
	readerErr error
	nextID    int64
	closed    atomic.Bool
}

type mcpResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  any             `json:"error"`
}

func startStdioMCP(ctx context.Context, cfg mcpServerConfig) (*stdioMCPClient, error) {
	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	go io.Copy(io.Discard, stderr)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024), 8*1024*1024)
	client := &stdioMCPClient{
		cmd: cmd, stdin: stdin, pending: make(map[string]chan mcpResponse), done: make(chan struct{}),
	}
	go client.readResponses(scanner)
	return client, nil
}

func (c *stdioMCPClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.closed.Load() {
		return nil, fmt.Errorf("mcp stdio client closed")
	}
	id := atomic.AddInt64(&c.nextID, 1)
	key := strconv.FormatInt(id, 10)
	responseCh := make(chan mcpResponse, 1)
	c.pendingMu.Lock()
	if c.closed.Load() {
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("mcp stdio client closed")
	}
	c.pending[key] = responseCh
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, key)
		c.pendingMu.Unlock()
	}()

	if err := c.writeMessage(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("mcp call timeout: %s", method)
	case <-c.done:
		return nil, c.readError()
	case resp := <-responseCh:
		if resp.Error != nil {
			errBytes, _ := json.Marshal(resp.Error)
			return nil, fmt.Errorf("mcp error: %s", errBytes)
		}
		return resp.Result, nil
	}
}

func (c *stdioMCPClient) Notify(_ context.Context, method string, params any) error {
	return c.writeMessage(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *stdioMCPClient) writeMessage(message any) error {
	if c.closed.Load() {
		return fmt.Errorf("mcp stdio client closed")
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return fmt.Errorf("mcp stdio client closed")
	}
	_, err = c.stdin.Write(append(payload, '\n'))
	return err
}

func (c *stdioMCPClient) readResponses(scanner *bufio.Scanner) {
	for scanner.Scan() {
		var response mcpResponse
		if json.Unmarshal(scanner.Bytes(), &response) != nil || len(response.ID) == 0 {
			continue
		}
		key := mcpResponseID(response.ID)
		c.pendingMu.Lock()
		responseCh := c.pending[key]
		c.pendingMu.Unlock()
		if responseCh != nil {
			select {
			case responseCh <- response:
			default:
			}
		}
	}
	err := scanner.Err()
	if err == nil {
		err = errors.New("mcp server closed stdout")
	}
	c.stop(err)
}

func (c *stdioMCPClient) stop(err error) {
	c.finish.Do(func() {
		c.errMu.Lock()
		c.readerErr = err
		c.errMu.Unlock()
		c.closed.Store(true)
		close(c.done)
	})
}

func (c *stdioMCPClient) readError() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.readerErr != nil {
		return c.readerErr
	}
	return errors.New("mcp stdio client closed")
}

func (c *stdioMCPClient) Close() error {
	c.stop(errors.New("mcp stdio client closed"))
	c.writeMu.Lock()
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	c.writeMu.Unlock()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_, _ = c.cmd.Process.Wait()
	}
	return nil
}

// httpMCPClient 实现带会话头和 SSE 响应解析的 2025-era Streamable HTTP 子集。
type httpMCPClient struct {
	url             string
	headers         map[string]string
	http            *http.Client
	nextID          int64
	stateMu         sync.RWMutex
	sessionID       string
	protocolVersion string
}

func (c *httpMCPClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	return c.send(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}, true)
}

func (c *httpMCPClient) Notify(ctx context.Context, method string, params any) error {
	_, err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params}, false)
	return err
}

func (c *httpMCPClient) SetProtocolVersion(version string) {
	c.stateMu.Lock()
	c.protocolVersion = strings.TrimSpace(version)
	c.stateMu.Unlock()
}

func (c *httpMCPClient) send(ctx context.Context, message any, expectResponse bool) (json.RawMessage, error) {
	reqBody, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	c.stateMu.RLock()
	sessionID, protocolVersion := c.sessionID, c.protocolVersion
	c.stateMu.RUnlock()
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp http failed: %s", resp.Status)
	}
	if value := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); value != "" {
		c.stateMu.Lock()
		c.sessionID = value
		c.stateMu.Unlock()
	}
	if !expectResponse {
		return nil, nil
	}
	var response mcpResponse
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		response, err = decodeMCPHTTPStream(io.LimitReader(resp.Body, 8<<20))
	} else {
		var data []byte
		data, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err == nil {
			if len(bytes.TrimSpace(data)) == 0 {
				return nil, errors.New("MCP HTTP response body is empty")
			}
			response, err = decodeMCPHTTPResponse(data)
		}
	}
	if err != nil {
		return nil, err
	}
	if response.Error != nil {
		errBytes, _ := json.Marshal(response.Error)
		return nil, fmt.Errorf("mcp error: %s", errBytes)
	}
	return response.Result, nil
}

func (c *httpMCPClient) Close() error { return nil }

func decodeMCPHTTPResponse(data []byte) (mcpResponse, error) {
	var response mcpResponse
	if json.Unmarshal(data, &response) == nil && (len(response.Result) > 0 || response.Error != nil) {
		return response, nil
	}
	return decodeMCPHTTPStream(bytes.NewReader(data))
}

func decodeMCPHTTPStream(reader io.Reader) (mcpResponse, error) {
	var response mcpResponse
	// Streamable HTTP may return one or more SSE events. The client only needs
	// the JSON-RPC response event for the request it just issued.
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024), 8*1024*1024)
	var eventData strings.Builder
	decodeEvent := func() bool {
		payload := strings.TrimSpace(eventData.String())
		eventData.Reset()
		return payload != "" && json.Unmarshal([]byte(payload), &response) == nil && (len(response.Result) > 0 || response.Error != nil)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if decodeEvent() {
				return response, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if eventData.Len() > 0 {
				eventData.WriteByte('\n')
			}
			eventData.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if decodeEvent() {
		return response, nil
	}
	if err := scanner.Err(); err != nil {
		return mcpResponse{}, err
	}
	return mcpResponse{}, errors.New("MCP HTTP response was neither JSON nor a JSON-RPC SSE event")
}

func mcpResponseID(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if unquoted, err := strconv.Unquote(trimmed); err == nil {
		return unquoted
	}
	return trimmed
}

func validateMCPToolPolicies(policies map[string]mcpToolPolicy) error {
	for toolName, policy := range policies {
		if strings.TrimSpace(toolName) == "" {
			return errors.New("MCP tool policy name is empty")
		}
		for _, role := range policy.TeamRoles {
			role = strings.ToLower(strings.TrimSpace(role))
			if !validMCPTeamRoles[role] {
				return fmt.Errorf("MCP tool %s has unsupported team role %q", toolName, role)
			}
		}
		if len(policy.TeamRoles) > 0 && !policy.ReadOnly {
			return fmt.Errorf("MCP tool %s grants team roles without readOnly=true", toolName)
		}
		if len(policy.TeamRoles) > 0 && policy.Sensitive {
			return fmt.Errorf("MCP tool %s cannot grant team roles while sensitive=true", toolName)
		}
	}
	return nil
}

func normalizedMCPTeamRoles(roles []string) []string {
	seen := make(map[string]bool, len(roles))
	normalized := make([]string, 0, len(roles))
	for _, role := range roles {
		role = strings.ToLower(strings.TrimSpace(role))
		if !validMCPTeamRoles[role] || seen[role] {
			continue
		}
		seen[role] = true
		normalized = append(normalized, role)
	}
	sort.Strings(normalized)
	return normalized
}

func mcpTransportName(cfg mcpServerConfig) string {
	if strings.TrimSpace(cfg.Command) != "" {
		return "stdio"
	}
	if strings.TrimSpace(cfg.URL) != "" {
		return "http"
	}
	return "unknown"
}

func safeMCPStatusError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 240 {
		message = message[:240] + "..."
	}
	return message
}

// expandMCPVars 替换 ${ENV_VAR} 形式的占位符，复用当前进程环境。
func expandMCPVars(cfg *mcpServerConfig) {
	repl := func(s string) string {
		for _, env := range os.Environ() {
			parts := strings.SplitN(env, "=", 2)
			if len(parts) == 2 {
				s = strings.ReplaceAll(s, "${"+parts[0]+"}", parts[1])
			}
		}
		return s
	}
	cfg.Command = repl(cfg.Command)
	cfg.URL = repl(cfg.URL)
	for i, arg := range cfg.Args {
		cfg.Args[i] = repl(arg)
	}
	for k, v := range cfg.Headers {
		cfg.Headers[k] = repl(v)
	}
	for k, v := range cfg.Env {
		cfg.Env[k] = repl(v)
	}
}

// sanitizeMCPName 把名称中的 - 与 . 替换为 _，保证生成的本地名称可直接用作标识符。
func sanitizeMCPName(s string) string {
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

// sanitizeMCPSchema 对 MCP inputSchema 做最小裁剪：空 schema 补成 object，
// 剥掉 $schema 元噪声，确保顶层 type 为 object。
func sanitizeMCPSchema(schema map[string]any) map[string]any {
	if len(schema) == 0 {
		return map[string]any{"type": "object"}
	}
	delete(schema, "$schema")
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	return schema
}

// flattenMCPContent 把 MCP tools/call 的 content 数组展开成纯文本。
func flattenMCPContent(raw json.RawMessage) string {
	var out struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
			Data     string `json:"data"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Content) == 0 {
		return string(raw)
	}
	var builder strings.Builder
	for _, item := range out.Content {
		if item.Type == "text" {
			builder.WriteString(item.Text)
			builder.WriteString("\n")
		} else {
			builder.WriteString("[" + item.Type + " content")
			if item.MimeType != "" {
				builder.WriteString(" " + item.MimeType)
			}
			if item.Data != "" {
				builder.WriteString(fmt.Sprintf(" base64=%d bytes", len(item.Data)))
			}
			builder.WriteString("]\n")
		}
	}
	return strings.TrimSpace(builder.String())
}
