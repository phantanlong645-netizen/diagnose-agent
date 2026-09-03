package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"olt-diagnostic-agent/internal/domain"
)

// mcp.go 把 paicli-go 的 MCP 能力（stdio/HTTP 双传输 + Schema 裁剪 + HITL 审批）
// 迁移到 OLT 工具链。MCP 工具是对外部系统的开放调用，注册进 Registry 时统一
// 标 OpenWorld，policy.Evaluate 会要求用户显式审批后才执行。
//
// 传输层：
//   - stdio：拉起子进程，走 JSON-RPC 2.0（换行分隔），支持 args/env 变量展开；
//   - HTTP：直接 POST JSON-RPC 2.0 到远端 URL（Streamable HTTP 的 stateless 子集）。
//
// Schema 裁剪：MCP server 返回的 inputSchema 直接透传给模型会导致上下文膨胀，
// 这里做最小裁剪——缺失时补成 object、剥掉 $schema 这类元噪声，其余原样保留。

// mcpConfigFile 是 mcp.json 的顶层结构，与 paicli-go 的配置格式对齐。
type mcpConfigFile struct {
	MCPServers map[string]mcpServerConfig `json:"mcpServers"`
}

type mcpServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Env     map[string]string `json:"env"`
}

// mcpClient 抽象 stdio / HTTP 两种传输，Call 返回 result 字段的原始 JSON。
type mcpClient interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	Close() error
}

// MCPTool 是一个动态生成的工具，实现 Registry 的 Tool 接口。
// 它的 name 是 mcp__<server>__<remote>，执行时把参数透传给远端 tools/call。
type MCPTool struct {
	serverName  string
	remoteName  string
	localName   string
	description string
	schema      map[string]any
	client      mcpClient
}

// MCPToolInfo 是 agent.Engine 构建 Eino 动态工具所需的最小描述。
// 不暴露 client：Engine 通过 runner.Execute -> Registry -> MCPTool.Execute 执行，
// 这样审批、事件、evidence 记录都复用现有流水线。
type MCPToolInfo struct {
	Name        string
	Description string
	InputSchema map[string]any
}

func (t *MCPTool) ToolInfo() MCPToolInfo {
	return MCPToolInfo{Name: t.localName, Description: t.description, InputSchema: t.schema}
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
		Summary: fmt.Sprintf("Call MCP tool %s.%s", t.serverName, t.remoteName),
		// OpenWorld 让 policy 走审批；不标 ReadOnly，因为 MCP 工具可能改变外部状态。
		Annotations: domain.ToolAnnotations{OpenWorld: true},
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
	return domain.ToolResult{
		Summary: fmt.Sprintf("MCP tool %s.%s completed", t.serverName, t.remoteName),
		Data:    map[string]any{"result": flattenMCPContent(raw)},
	}, nil
}

// MCPManager 持有已连接的 MCP 客户端和生成的工具，负责生命周期收尾。
type MCPManager struct {
	clients []mcpClient
	tools   []*MCPTool
}

// LoadMCP 从 configPath 读取 mcp.json 并连接所有 MCP server。单个 server 连接
// 失败只跳过，不影响其它 server 或应用启动。返回的 manager 永不为 nil。
func LoadMCP(ctx context.Context, configPath string) *MCPManager {
	manager := &MCPManager{}
	servers := loadMCPConfig(configPath)
	for name, cfg := range servers {
		client, err := startMCPServer(ctx, cfg)
		if err != nil {
			continue
		}
		if err = initializeMCP(ctx, client); err != nil {
			_ = client.Close()
			continue
		}
		tools := listMCPTools(ctx, name, client)
		if len(tools) == 0 {
			_ = client.Close()
			continue
		}
		manager.clients = append(manager.clients, client)
		manager.tools = append(manager.tools, tools...)
	}
	return manager
}

func (m *MCPManager) Tools() []*MCPTool {
	if m == nil {
		return nil
	}
	return m.tools
}

func (m *MCPManager) Close() {
	if m == nil {
		return
	}
	for _, client := range m.clients {
		_ = client.Close()
	}
}

func loadMCPConfig(configPath string) map[string]mcpServerConfig {
	if configPath == "" {
		return nil
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var cfg mcpConfigFile
	if json.Unmarshal(content, &cfg) != nil {
		return nil
	}
	merged := make(map[string]mcpServerConfig, len(cfg.MCPServers))
	for name, server := range cfg.MCPServers {
		expandMCPVars(&server)
		merged[name] = server
	}
	return merged
}

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
	_, err := client.Call(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "olt-diagnostic-agent", "version": "1.0.0"},
	})
	return err
}

func listMCPTools(ctx context.Context, serverName string, client mcpClient) []*MCPTool {
	raw, err := client.Call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil
	}
	var out struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	tools := make([]*MCPTool, 0, len(out.Tools))
	for _, item := range out.Tools {
		if strings.TrimSpace(item.Name) == "" {
			continue
		}
		localName := "mcp__" + sanitizeMCPName(serverName) + "__" + sanitizeMCPName(item.Name)
		tools = append(tools, &MCPTool{
			serverName:  serverName,
			remoteName:  item.Name,
			localName:   localName,
			description: fmt.Sprintf("MCP tool %s.%s: %s", serverName, item.Name, strings.TrimSpace(item.Description)),
			schema:      sanitizeMCPSchema(item.InputSchema),
			client:      client,
		})
	}
	return tools
}

// stdioMCPClient 通过子进程 stdin/stdout 跑 JSON-RPC 2.0。
type stdioMCPClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	mu      sync.Mutex
	nextID  int64
	closed  atomic.Bool
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
	return &stdioMCPClient{cmd: cmd, stdin: stdin, scanner: scanner}, nil
}

func (c *stdioMCPClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return nil, fmt.Errorf("mcp stdio client closed")
	}
	id := atomic.AddInt64(&c.nextID, 1)
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := c.stdin.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	type response struct {
		ID     int64           `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("mcp call timeout: %s", method)
		default:
		}
		if !c.scanner.Scan() {
			return nil, fmt.Errorf("mcp server closed stdout")
		}
		var resp response
		if json.Unmarshal(c.scanner.Bytes(), &resp) != nil {
			continue
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			errBytes, _ := json.Marshal(resp.Error)
			return nil, fmt.Errorf("mcp error: %s", errBytes)
		}
		return resp.Result, nil
	}
}

func (c *stdioMCPClient) Close() error {
	c.closed.Store(true)
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_, _ = c.cmd.Process.Wait()
	}
	return nil
}

type httpMCPClient struct {
	url     string
	headers map[string]string
	http    *http.Client
	nextID  int64
}

func (c *httpMCPClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	reqBody, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp http failed: %s: %s", resp.Status, string(data))
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if err = json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		errBytes, _ := json.Marshal(out.Error)
		return nil, fmt.Errorf("mcp error: %s", errBytes)
	}
	return out.Result, nil
}

func (c *httpMCPClient) Close() error { return nil }

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
