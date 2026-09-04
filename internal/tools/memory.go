package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"olt-diagnostic-agent/internal/domain"
)

// memoryScopeGlobal 是跨 profile 共享的记忆作用域。与"global"字符串比较时忽略大小写，
// 其余任何非空值都视为某个 profile ID，表示该记忆只属于该目标。
const memoryScopeGlobal = "global"

// MemoryEntry 是长期记忆中的一条持久化事实。与 per-conversation 的 domain.ContextFact
// 不同，这里没有 conversation 归属，只有 global / profile 两个作用域，从而跨会话存活。
type MemoryEntry struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"` // "global" 或 profile ID
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
}

// MemoryStore 把 paicli-go 的长期记忆能力移植进 OLT 工具链：JSON 文件持久化 +
// 关键词检索。作用域语义：global 条目对所有 profile 可见，profile 条目仅对
// 对应 profile 可见。检索时两者合并返回，按时间倒序截断。
type MemoryStore struct {
	path    string
	mu      sync.RWMutex
	entries []MemoryEntry
}

func NewMemoryStore(path string) (*MemoryStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("memory storage path is required")
	}
	store := &MemoryStore{path: path}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

// Save 追加一条记忆。内容去空白后为空则跳过；scope 为空视为 global。相同
// scope + 内容的条目不会重复追加，避免长期运行后无限膨胀。
func (m *MemoryStore) Save(content, scope string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	scope = normalizeMemoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.entries {
		if entry.Scope == scope && entry.Content == content {
			return nil
		}
	}
	m.entries = append(m.entries, MemoryEntry{
		ID:        "mem-" + time.Now().UTC().Format("20060102150405.000000000"),
		Scope:     scope,
		Content:   content,
		CreatedAt: time.Now().UTC(),
	})
	return m.persist()
}

// Relevant 返回与 query 相关的记忆。检索范围 = global 条目 + 当前 scope 条目。
// query 为空时返回最近条目；否则按"子串命中 或 至少两个查询词命中"过滤。
func (m *MemoryStore) Relevant(query, scope string, limit int) []MemoryEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 {
		limit = 8
	}
	scope = normalizeMemoryScope(scope)
	needle := strings.ToLower(strings.TrimSpace(query))
	matched := make([]MemoryEntry, 0)
	for _, entry := range m.entries {
		if entry.Scope != memoryScopeGlobal && entry.Scope != scope {
			continue
		}
		content := strings.ToLower(entry.Content)
		if needle == "" || strings.Contains(content, needle) || memoryOverlap(needle, content) {
			matched = append(matched, entry)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].CreatedAt.After(matched[j].CreatedAt) })
	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched
}

func (m *MemoryStore) load() error {
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &m.entries)
}

func (m *MemoryStore) persist() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, data, 0o600)
}

func normalizeMemoryScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" || strings.EqualFold(scope, memoryScopeGlobal) {
		return memoryScopeGlobal
	}
	return scope
}

// memoryOverlap 是 paicli-go 的关键词重叠判定：查询里至少两个长度 >=2 的词都
// 命中内容才算相关，用于"查询是一句话"而不是一个精确词时的召回。
func memoryOverlap(query, content string) bool {
	words := strings.Fields(query)
	if len(words) == 0 {
		return false
	}
	hits := 0
	for _, word := range words {
		if len([]rune(word)) >= 2 && strings.Contains(content, word) {
			hits++
		}
	}
	return hits >= 2
}

// MemorySaveRequest 是 memory_save 工具的入参。scope 省略时默认保存到当前 profile。
type MemorySaveRequest struct {
	ProfileID string `json:"profileId"`
	Content   string `json:"content"`
	Scope     string `json:"scope,omitempty"` // "global" 或留空表示当前 profile
}

type MemorySaveTool struct {
	store *MemoryStore
}

func NewMemorySaveTool(store *MemoryStore) (*MemorySaveTool, error) {
	if store == nil {
		return nil, errors.New("memory store is required")
	}
	return &MemorySaveTool{store: store}, nil
}

func (t *MemorySaveTool) Definition() Definition {
	return Definition{
		Name:        "memory_save",
		Description: "Persist one durable fact to long-term cross-conversation memory. Use it to remember a verified route, RPC recipe, error signature, target-specific quirk, or conclusion that will be useful in a future diagnostic session. Scope to the current profile by default, or pass scope=global for facts that apply to every target.",
	}
}

func (t *MemorySaveTool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var input MemorySaveRequest
	if err := json.Unmarshal(arguments, &input); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode arguments: %w", err)
	}
	input.ProfileID = strings.TrimSpace(input.ProfileID)
	if input.ProfileID == "" {
		return domain.PreparedCall{}, errors.New("profileId is required")
	}
	input.Content = strings.TrimSpace(input.Content)
	if input.Content == "" {
		return domain.PreparedCall{}, errors.New("content is required")
	}
	if len(input.Content) > 4000 {
		return domain.PreparedCall{}, errors.New("memory content exceeds 4000 characters")
	}
	if strings.EqualFold(strings.TrimSpace(input.Scope), memoryScopeGlobal) {
		input.Scope = memoryScopeGlobal
	} else {
		input.Scope = input.ProfileID
	}
	return domain.PreparedCall{
		Summary: fmt.Sprintf("Remember a fact for scope %s", input.Scope),
		Preview: abbreviate(input.Content, 2000),
		// memory_save 只维护 agent 的本地长期记忆，不改变所选目标设备或工作区，
		// 因此按只读操作处理（policy 自动放行，不触发审批）。
		Annotations: domain.ToolAnnotations{ReadOnly: true},
		Input:       input,
	}, nil
}

func (t *MemorySaveTool) Execute(_ context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	input, ok := call.Input.(MemorySaveRequest)
	if !ok {
		return domain.ToolResult{}, errors.New("invalid prepared memory save")
	}
	if err := t.store.Save(input.Content, input.Scope); err != nil {
		return domain.ToolResult{}, fmt.Errorf("save memory: %w", err)
	}
	return domain.ToolResult{
		Summary: "Saved fact to long-term memory",
		Message: "The fact is now available to future diagnostic sessions via memory_search.",
		Metadata: map[string]string{
			"scope":   input.Scope,
			"content": abbreviate(input.Content, 500),
		},
	}, nil
}

// MemorySearchRequest 是 memory_search 工具的入参。query 为空时返回最近记忆。
type MemorySearchRequest struct {
	ProfileID string `json:"profileId"`
	Query     string `json:"query"`
	Limit     int    `json:"limit,omitempty"`
}

type MemorySearchTool struct {
	store *MemoryStore
}

func NewMemorySearchTool(store *MemoryStore) (*MemorySearchTool, error) {
	if store == nil {
		return nil, errors.New("memory store is required")
	}
	return &MemorySearchTool{store: store}, nil
}

func (t *MemorySearchTool) Definition() Definition {
	return Definition{
		Name:        "memory_search",
		Description: "Search long-term cross-conversation memory by keyword. Returns the most recent matching facts from the current profile plus global facts. Use it before re-deriving a conclusion, route, RPC recipe, or error signature that a previous session may have already recorded.",
	}
}

func (t *MemorySearchTool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var input MemorySearchRequest
	if err := json.Unmarshal(arguments, &input); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode arguments: %w", err)
	}
	input.ProfileID = strings.TrimSpace(input.ProfileID)
	if input.ProfileID == "" {
		return domain.PreparedCall{}, errors.New("profileId is required")
	}
	input.Query = strings.TrimSpace(input.Query)
	if input.Limit == 0 {
		input.Limit = 8
	}
	if input.Limit < 1 || input.Limit > 50 {
		return domain.PreparedCall{}, errors.New("limit must be between 1 and 50")
	}
	return domain.PreparedCall{
		Summary:     fmt.Sprintf("Search long-term memory for %q", abbreviate(input.Query, 200)),
		Annotations: domain.ToolAnnotations{ReadOnly: true, Idempotent: true},
		Input:       input,
	}, nil
}

func (t *MemorySearchTool) Execute(_ context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	input, ok := call.Input.(MemorySearchRequest)
	if !ok {
		return domain.ToolResult{}, errors.New("invalid prepared memory search")
	}
	entries := t.store.Relevant(input.Query, input.ProfileID, input.Limit)
	rows := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, map[string]any{
			"id":        entry.ID,
			"scope":     entry.Scope,
			"content":   entry.Content,
			"createdAt": entry.CreatedAt,
		})
	}
	if len(rows) == 0 {
		return domain.ToolResult{
			Summary:  fmt.Sprintf("No long-term memory matches %q", input.Query),
			Message:  "No cross-conversation memory matches. You can record a durable fact with memory_save so future sessions can reuse it.",
			Metadata: map[string]string{"query": input.Query, "scope": input.ProfileID},
		}, nil
	}
	return domain.ToolResult{
		Summary: fmt.Sprintf("Found %d long-term memory entries", len(rows)),
		Data: map[string]any{
			"query":   input.Query,
			"results": rows,
		},
	}, nil
}
