package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"olt-diagnostic-agent/internal/domain"
)

var (
	ErrToolNotFound  = errors.New("tool not found")
	ErrDuplicateTool = errors.New("tool already registered")
)

type Definition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type Tool interface {
	Definition() Definition
	Prepare(arguments json.RawMessage) (domain.PreparedCall, error)
	Execute(ctx context.Context, call domain.PreparedCall) (domain.ToolResult, error)
}

type TargetResolver interface {
	NBI(profileID string) (domain.NBITarget, bool)
	NETCONF(profileID string) (domain.NETCONFTarget, bool)
	NETCONFEndpoints(profileID string) ([]domain.NETCONFEndpoint, bool)
	WorkspaceRoots(profileID string) ([]string, bool)
}

// Registry 是线程安全的工具注册表，按名称保存所有已注册工具。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry 创建一个空的工具注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

func (r *Registry) Register(tool Tool) error {
	definition := tool.Definition()
	if definition.Name == "" {
		return errors.New("tool name is required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[definition.Name]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateTool, definition.Name)
	}
	r.tools[definition.Name] = tool
	return nil
}

func (r *Registry) Definitions() []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	definitions := make([]Definition, 0, len(r.tools))
	for _, tool := range r.tools {
		definitions = append(definitions, tool.Definition())
	}
	sort.Slice(definitions, func(i, j int) bool {
		return definitions[i].Name < definitions[j].Name
	})
	return definitions
}

// Prepare 按名查找工具并解析参数，返回已装配的 PreparedCall（审批与执行由调用方负责）。
func (r *Registry) Prepare(call domain.ToolCall) (Tool, domain.PreparedCall, error) {
	r.mu.RLock()
	tool, exists := r.tools[call.Name]
	r.mu.RUnlock()
	if !exists {
		return nil, domain.PreparedCall{}, fmt.Errorf("%w: %s", ErrToolNotFound, call.Name)
	}

	prepared, err := tool.Prepare(call.Arguments)
	if err != nil {
		return nil, domain.PreparedCall{}, fmt.Errorf("prepare %s: %w", call.Name, err)
	}
	prepared.ToolCall = call
	return tool, prepared, nil
}

// Tool 按名称返回注册的工具，用于跨层访问特殊方法（如 ResetReads）。
func (r *Registry) Tool(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, exists := r.tools[name]
	return tool, exists
}

// MCPToolInfos 返回所有已注册 MCP 工具的最小描述（名称/描述/裁剪后的 schema），
// 供 agent.Engine 动态构建 Eino 工具。返回结果按名称排序保证稳定。
func (r *Registry) MCPToolInfos() []MCPToolInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	infos := make([]MCPToolInfo, 0)
	for _, tool := range r.tools {
		if mcpTool, ok := tool.(*MCPTool); ok {
			infos = append(infos, mcpTool.ToolInfo())
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos
}

// MCPToolInfosForRole returns only tools explicitly authorized by the local
// MCP policy for a read-only Team role. Remote server annotations are not used.
func (r *Registry) MCPToolInfosForRole(role string) []MCPToolInfo {
	role = strings.ToLower(strings.TrimSpace(role))
	infos := r.MCPToolInfos()
	filtered := make([]MCPToolInfo, 0, len(infos))
	for _, info := range infos {
		if !info.Annotations.ReadOnly || info.Annotations.Destructive || info.Annotations.Sensitive {
			continue
		}
		for _, allowedRole := range info.TeamRoles {
			if allowedRole == role {
				filtered = append(filtered, info)
				break
			}
		}
	}
	return filtered
}
