package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"olt-diagnostic-agent/internal/domain"
	"olt-diagnostic-agent/internal/rag"
)

// CodeSearchTool 把 paicli-go 的 RAG 语义代码检索接入 OLT 工具链（search_code）。
//
// 定位：search_files（ripgrep 精确文本匹配）的语义兜底——当模型不知道精确符号名、
// 或问题偏自然语言（"哪里实现了 ONU 注册流程"）时，用 TF 余弦 + 符号级索引定位。
//
// 索引策略：
//   - 索引按 profile 持久化在应用配置目录 rag/<profileID>.json 下；
//   - 首次调用或 root 集合漂移时自动重建（构建期间尊重 ctx 取消）；
//   - 显式 rebuild=true 强制重建（workspace 发生大改后使用）。
type CodeSearchTool struct {
	targets   TargetResolver
	storage   string
	mu        sync.Mutex
	indexes   map[string]*rag.Index
	indexRoots map[string][]string
}

type CodeSearchRequest struct {
	ProfileID string `json:"profileId"`
	Query     string `json:"query"`
	TopK      int    `json:"topK,omitempty"`
	Rebuild   bool   `json:"rebuild,omitempty"`
}

func NewCodeSearchTool(targets TargetResolver, storageDir string) (*CodeSearchTool, error) {
	if targets == nil {
		return nil, errors.New("target resolver is required")
	}
	if strings.TrimSpace(storageDir) == "" {
		return nil, errors.New("index storage directory is required")
	}
	return &CodeSearchTool{
		targets:    targets,
		storage:    storageDir,
		indexes:    make(map[string]*rag.Index),
		indexRoots: make(map[string][]string),
	}, nil
}

func (t *CodeSearchTool) Definition() Definition {
	return Definition{
		Name: "search_code",
		Description: "Semantic code search over the workspace code index (TF cosine over symbol-level chunks). Use it as the fallback when search_files cannot locate an implementation because the exact symbol name or text is unknown, or for natural-language questions such as where a feature is implemented. Returns file, line range, symbol, score, and preview.",
	}
}

func (t *CodeSearchTool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var input CodeSearchRequest
	if err := json.Unmarshal(arguments, &input); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode arguments: %w", err)
	}
	input.ProfileID = strings.TrimSpace(input.ProfileID)
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" {
		return domain.PreparedCall{}, errors.New("query is required")
	}
	if _, exists := t.targets.WorkspaceRoots(input.ProfileID); !exists {
		return domain.PreparedCall{}, errors.New("target profile was not found")
	}
	if input.TopK == 0 {
		input.TopK = 8
	}
	if input.TopK < 1 || input.TopK > 20 {
		return domain.PreparedCall{}, errors.New("topK must be between 1 and 20")
	}
	return domain.PreparedCall{
		Summary:     fmt.Sprintf("Semantic code search for %q", abbreviate(input.Query, 200)),
		Annotations: domain.ToolAnnotations{ReadOnly: true, Idempotent: true},
		Input:       input,
	}, nil
}

func (t *CodeSearchTool) Execute(ctx context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	input, ok := call.Input.(CodeSearchRequest)
	if !ok {
		return domain.ToolResult{}, errors.New("invalid prepared code search")
	}
	roots, _ := t.targets.WorkspaceRoots(input.ProfileID)
	if len(roots) == 0 {
		return domain.ToolResult{}, errors.New("profile has no allowed workspace roots")
	}

	index, rebuilt, err := t.indexFor(ctx, input.ProfileID, roots, input.Rebuild)
	if err != nil {
		return domain.ToolResult{}, err
	}
	results := index.Search(input.Query, input.TopK)
	rows := make([]map[string]any, 0, len(results))
	for _, result := range results {
		rows = append(rows, map[string]any{
			"path":      result.Chunk.Path,
			"startLine": result.Chunk.StartLine,
			"endLine":   result.Chunk.EndLine,
			"kind":      result.Chunk.Kind,
			"symbol":    result.Chunk.Symbol,
			"score":     fmt.Sprintf("%.3f", result.Score),
			"preview":   result.Chunk.Preview(),
		})
	}
	indexedFiles := 0
	for _, chunk := range index.Chunks {
		if chunk.Kind == "file" {
			indexedFiles++
		}
	}
	if len(rows) == 0 {
		return domain.ToolResult{
			Summary: fmt.Sprintf("No semantic match for %q in %d indexed files", input.Query, indexedFiles),
			Message: "No semantic match. If the workspace changed significantly since the index was built, retry once with rebuild=true; otherwise switch evidence source instead of repeating the same query.",
			Metadata: map[string]string{"query": input.Query, "indexedFiles": fmt.Sprint(indexedFiles)},
		}, nil
	}
	return domain.ToolResult{
		Summary: fmt.Sprintf("Found %d semantic matches for %q", len(rows), input.Query),
		Data: map[string]any{
			"query":        input.Query,
			"results":      rows,
			"rebuilt":      rebuilt,
			"indexedFiles": indexedFiles,
		},
	}, nil
}

// indexFor 返回可用的 profile 索引：命中缓存/磁盘且 root 未漂移时直接复用，
// 否则在 ctx 控制下重建并落盘。互斥锁串行化构建，避免并行 tool call 重复建索引。
func (t *CodeSearchTool) indexFor(ctx context.Context, profileID string, roots []string, forceRebuild bool) (*rag.Index, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !forceRebuild {
		if cached, exists := t.indexes[profileID]; exists && sameRoots(t.indexRoots[profileID], roots) && len(cached.Chunks) > 0 {
			return cached, false, nil
		}
	}
	indexPath := filepath.Join(t.storage, "rag", sanitizeIndexName(profileID)+".json")
	if !forceRebuild {
		if disk := rag.NewIndex(indexPath); disk.Load() == nil && disk.NeedsSameRoots(roots) && len(disk.Chunks) > 0 {
			t.indexes[profileID] = disk
			t.indexRoots[profileID] = roots
			return disk, false, nil
		}
	}
	index := rag.NewIndex(indexPath)
	if err := index.BuildRoots(ctx, roots); err != nil {
		return nil, false, fmt.Errorf("build code index: %w", err)
	}
	if err := index.Save(); err != nil {
		return nil, false, fmt.Errorf("save code index: %w", err)
	}
	t.indexes[profileID] = index
	t.indexRoots[profileID] = roots
	return index, true, nil
}

// DropIndex 删除指定 profile 的内存索引缓存（root 变更等场景可扩展调用）。
func (t *CodeSearchTool) DropIndex(profileID string) {
	t.mu.Lock()
	delete(t.indexes, profileID)
	delete(t.indexRoots, profileID)
	t.mu.Unlock()
}

func sameRoots(cached, current []string) bool {
	if len(cached) != len(current) {
		return false
	}
	seen := make(map[string]int, len(cached))
	for _, root := range cached {
		seen[root]++
	}
	for _, root := range current {
		seen[root]--
		if seen[root] < 0 {
			return false
		}
	}
	return true
}

// sanitizeIndexName 把 profileID（uuid 或用户输入）变成安全文件名。
func sanitizeIndexName(profileID string) string {
	replacer := strings.NewReplacer("\\", "_", "/", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_", " ", "_")
	name := replacer.Replace(strings.TrimSpace(profileID))
	if name == "" {
		return "default"
	}
	return name
}
