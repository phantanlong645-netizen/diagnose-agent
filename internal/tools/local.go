package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"olt-diagnostic-agent/internal/domain"
)

const (
	maxLocalOutputBytes  = 1024 * 1024
	maxFileSearchEntries = 25000
	fileSearchTimeout    = 15 * time.Second
	// ripgrep 输出上限。ripgrep 默认会输出全部匹配，大仓里可能几十万行。
	// 这里限制读取字节数避免内存爆掉，与 WalkDir 路径的 maxFileSearchEntries 等价。
	ripgrepOutputMaxBytes = 4 * 1024 * 1024
)

var (
	errFileSearchLimit      = errors.New("file search result limit reached")
	errFileSearchEntryLimit = errors.New("file search traversal limit reached")
	ripgrepLinePattern      = regexp.MustCompile(`^(.+?):([0-9]+):(.*)$`)
)

type FileSearchRequest struct {
	ProfileID  string `json:"profileId"`
	Pattern    string `json:"pattern,omitempty"`
	Query      string `json:"query,omitempty"`
	MaxResults int    `json:"maxResults,omitempty"`
}

type FileSearchTool struct{ targets TargetResolver }

func NewFileSearchTool(targets TargetResolver) (*FileSearchTool, error) {
	if targets == nil {
		return nil, errors.New("target resolver is required")
	}
	return &FileSearchTool{targets: targets}, nil
}

func (t *FileSearchTool) Definition() Definition {
	return Definition{Name: "search_files", Description: "Find files or ranked text matches inside allowed workspace roots. Patterns are case-insensitive and may be basenames or workspace-relative paths."}
}

func (t *FileSearchTool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var input FileSearchRequest
	if err := json.Unmarshal(arguments, &input); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode arguments: %w", err)
	}
	input.ProfileID = strings.TrimSpace(input.ProfileID)
	if _, exists := t.targets.WorkspaceRoots(input.ProfileID); !exists {
		return domain.PreparedCall{}, errors.New("target profile was not found")
	}
	if input.Pattern == "" {
		input.Pattern = "*"
	}
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" && isUnboundedFilePattern(input.Pattern) {
		return domain.PreparedCall{}, errors.New("broad file patterns require a text query; use a distinctive filename or add query to search within a focused extension")
	}
	if _, err := filepath.Match(input.Pattern, "example"); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("invalid file pattern: %w", err)
	}
	if input.MaxResults == 0 {
		input.MaxResults = 100
	}
	if input.MaxResults < 1 || input.MaxResults > 500 {
		return domain.PreparedCall{}, errors.New("maxResults must be between 1 and 500")
	}
	return domain.PreparedCall{
		Summary:     fmt.Sprintf("Search workspace files matching %s", input.Pattern),
		Annotations: domain.ToolAnnotations{ReadOnly: true, Idempotent: true},
		Input:       input,
	}, nil
}

func isUnboundedFilePattern(pattern string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || pattern == "*" || pattern == "*.*" {
		return true
	}
	// Extension-only globs such as *.go or *.md can traverse the whole
	// workspace and are not useful without a symbol/text query. Exact names
	// and focused prefixes remain valid discovery requests.
	return strings.HasPrefix(pattern, "*.") && !strings.Contains(pattern[2:], "*")
}

func (t *FileSearchTool) Execute(ctx context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	input, ok := call.Input.(FileSearchRequest)
	if !ok {
		return domain.ToolResult{}, errors.New("invalid prepared file search")
	}
	roots, _ := t.targets.WorkspaceRoots(input.ProfileID)

	// 优先用 ripgrep：它在大型 workspace 上比 WalkDir+ReadFile+Contains 快一个数量级，
	// 而且自带并行、.gitignore 跳过、二进制检测。Windows 用户需单独装 rg（winget install BurntSushi.ripgrep.MSVC）。
	// 找不到 rg 或 rg 调用失败时回退到原有 WalkDir 实现，保证功能不退化。
	if ripgrepAvailable() {
		matches, visited, limitedReason, rgErr := searchWithRipgrep(ctx, roots, input)
		if rgErr == nil {
			sort.Slice(matches, func(i, j int) bool { return matches[i]["path"].(string) < matches[j]["path"].(string) })
			data := map[string]any{
				"matches":        matches,
				"limited":        limitedReason != "",
				"visitedEntries": visited,
				"engine":         "ripgrep",
			}
			if limitedReason != "" {
				data["limitReason"] = limitedReason
			}
			return domain.ToolResult{
				Summary: searchSummary(len(matches), limitedReason),
				Data:    data,
			}, nil
		}
		// rg 出错（参数问题、权限问题等）就回退到 WalkDir，不要把错误暴露给模型。
	}

	matches := make([]map[string]any, 0)
	query := strings.ToLower(input.Query)
	searchContext, cancel := context.WithTimeout(ctx, fileSearchTimeout)
	defer cancel()
	visited := 0
	limitedReason := ""
	for _, root := range roots {
		rootVisited := 0
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if err := searchContext.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return walkErr
			}
			visited++
			rootVisited++
			if rootVisited > maxFileSearchEntries {
				return errFileSearchEntryLimit
			}
			if len(matches) >= input.MaxResults {
				return errFileSearchLimit
			}
			if entry.IsDir() {
				if path != root {
					switch entry.Name() {
					case ".git", ".idea", ".next", ".playwright-cli", "bin", "build", "coverage", "dist", "node_modules", "out", "vendor",
						".venv", "venv", "__pycache__", "site-packages", "mingw32", "mingw64":
						return filepath.SkipDir
					}
				}
				return nil
			}
			matched := filePatternMatches(input.Pattern, root, path, entry.Name())
			if !matched {
				return nil
			}
			if potentiallySensitivePath(path) {
				return nil
			}
			result := map[string]any{"path": path}
			if query != "" {
				info, statErr := entry.Info()
				if statErr != nil || info.Size() > maxLocalOutputBytes {
					return nil
				}
				content, readErr := os.ReadFile(path)
				if readErr != nil || strings.IndexByte(string(content), 0) >= 0 {
					return nil
				}
				line, snippet, found := matchingLine(string(content), query)
				if !found {
					return nil
				}
				result["line"] = line
				result["snippet"] = snippet
			}
			matches = append(matches, result)
			return nil
		})
		switch {
		case errors.Is(err, errFileSearchLimit):
			limitedReason = "result_limit"
		case errors.Is(err, errFileSearchEntryLimit):
			limitedReason = "traversal_limit"
			// A large first workspace (for example a Desktop or OneDrive root)
			// must not prevent the next configured project root from being
			// searched. The traversal limit is per root, not global.
			continue
		case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
			limitedReason = "time_limit"
		case err != nil:
			return domain.ToolResult{}, fmt.Errorf("search workspace %s: %w", root, err)
		}
		if limitedReason == "result_limit" || limitedReason == "time_limit" {
			break
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i]["path"].(string) < matches[j]["path"].(string) })
	data := map[string]any{
		"matches":        matches,
		"limited":        limitedReason != "",
		"visitedEntries": visited,
		"engine":         "walkdir",
	}
	if limitedReason != "" {
		data["limitReason"] = limitedReason
	}
	return domain.ToolResult{
		Summary: searchSummary(len(matches), limitedReason),
		Data:    data,
	}, nil
}

func searchSummary(matchCount int, limitedReason string) string {
	summary := fmt.Sprintf("Found %d workspace matches", matchCount)
	if limitedReason == "" {
		return summary
	}
	return fmt.Sprintf("%s (search limit reached: %s; this is not a definitive no-match)", summary, limitedReason)
}

// filePatternMatches treats a pattern without a path separator as a basename
// glob and a pattern with a separator as a workspace-relative glob. Matching
// is case-insensitive because Windows paths are case-insensitive, while
// filepath.Match itself is not.
func filePatternMatches(pattern, root, filePath, baseName string) bool {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	if pattern == "" || pattern == "*" || pattern == "*.*" {
		return true
	}
	candidates := []string{baseName}
	if strings.Contains(pattern, "/") {
		if relative, err := filepath.Rel(root, filePath); err == nil {
			candidates = append(candidates, filepath.ToSlash(relative))
		}
	}
	for _, candidate := range candidates {
		if globMatchInsensitive(pattern, candidate) {
			return true
		}
	}
	return false
}

func globMatchInsensitive(pattern, value string) bool {
	pattern = strings.ToLower(filepath.ToSlash(pattern))
	value = strings.ToLower(filepath.ToSlash(value))
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == value
	}
	var expression strings.Builder
	expression.WriteString("^")
	for index := 0; index < len(pattern); index++ {
		switch pattern[index] {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				if index+2 < len(pattern) && pattern[index+2] == '/' {
					// Globstar followed by a slash also matches zero
					// directories, e.g. server/**/routes.go matches
					// server/routes.go and server/nbi/routes.go.
					expression.WriteString("(?:.*/)?")
					index += 2
				} else {
					expression.WriteString(".*")
					index++
				}
			} else {
				expression.WriteString("[^/]*")
			}
		case '?':
			expression.WriteString("[^/]")
		default:
			expression.WriteString(regexp.QuoteMeta(string(pattern[index])))
		}
	}
	expression.WriteString("$")
	matched, err := regexp.MatchString(expression.String(), value)
	return err == nil && matched
}

// ripgrepAvailable is intentionally checked per search. GoLand, VS Code, and
// a terminal can expose different PATH values, and caching a startup miss
// would force the process to use the less capable fallback forever.
func ripgrepAvailable() bool {
	_, err := exec.LookPath("rg")
	return err == nil
}

// searchWithRipgrep 调用 ripgrep 子进程完成搜索。返回 matches 与 WalkDir 路径同样的 schema。
//
// 命令形态分两种：
//   - 仅按文件名匹配（query 为空）：`rg --files -g PATTERN ROOT`
//   - 内容匹配（query 非空）：`rg -n --color never --no-heading --max-count 1 -g PATTERN QUERY ROOT`
//
// rg 退出码：0=有匹配，1=无匹配，2=错误。这里只在 2 时返回 error 让上层回退。
func searchWithRipgrep(ctx context.Context, roots []string, input FileSearchRequest) ([]map[string]any, int, string, error) {
	searchContext, cancel := context.WithTimeout(ctx, fileSearchTimeout)
	defer cancel()

	matches := make([]map[string]any, 0, input.MaxResults)
	visited := 0
	limitedReason := ""

	for _, root := range roots {
		args := buildRipgrepArgs(input, root)
		if args == nil {
			continue
		}
		command := exec.CommandContext(searchContext, "rg", args...)
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		// rg 默认会读 .gitignore；这里我们想让结果和 WalkDir 一致（跳过 .git/build/node_modules 等），
		// 所以传 --no-ignore --hidden 并依赖 -g 显式过滤。
		command.Env = append(os.Environ(), "RIPGREP_CONFIG_PATH=")
		if err := command.Run(); err != nil {
			// 退出码 1 = 无匹配，是正常情况；其他错误才回退。
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				continue
			}
			return nil, 0, "", fmt.Errorf("ripgrep failed: %w; stderr: %s", err, stderr.String())
		}
		scanner := bufio.NewScanner(io.LimitReader(bytes.NewReader(stdout.Bytes()), ripgrepOutputMaxBytes))
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			if len(matches) >= input.MaxResults {
				limitedReason = "result_limit"
				break
			}
			line := scanner.Text()
			if line == "" {
				continue
			}
			visited++
			match, ok := parseRipgrepLine(line, input.Query)
			if !ok {
				continue
			}
			if potentiallySensitivePath(match["path"].(string)) {
				continue
			}
			matches = append(matches, match)
		}
		if visited > maxFileSearchEntries {
			limitedReason = "traversal_limit"
		}
		if limitedReason != "" {
			break
		}
	}
	return matches, visited, limitedReason, nil
}

// buildRipgrepArgs 构造 rg 命令参数。返回 nil 表示该 root 不需要搜索（不应该发生）。
func buildRipgrepArgs(input FileSearchRequest, root string) []string {
	// --no-ignore: 不读 .gitignore/.ignore，让结果和 WalkDir 一致（WalkDir 只 SkipDir 几个固定目录）
	// --hidden: 包含隐藏文件，但后面会用 -g 过滤
	// -g PATTERN: 只保留文件名匹配的；同时排除掉 WalkDir 跳过的目录
	args := []string{
		"--no-ignore",
		"--hidden",
		"--glob-case-insensitive",
		"-g", "!.git/**",
		"-g", "!.idea/**",
		"-g", "!.next/**",
		"-g", "!.playwright-cli/**",
		"-g", "!bin/**",
		"-g", "!build/**",
		"-g", "!coverage/**",
		"-g", "!dist/**",
		"-g", "!node_modules/**",
		"-g", "!out/**",
		"-g", "!vendor/**",
		"-g", "!.venv/**",
		"-g", "!venv/**",
		"-g", "!__pycache__/**",
		"-g", "!site-packages/**",
		"-g", "!mingw32/**",
		"-g", "!mingw64/**",
	}
	pattern := strings.TrimSpace(input.Pattern)
	if pattern == "" || pattern == "*" || pattern == "*.*" {
		// 不带 pattern 时，rg --files 列出所有文件
		args = append(args, "--files")
	} else {
		args = append(args, "-g", pattern)
		args = append(args, "--files")
	}
	if query := strings.TrimSpace(input.Query); query != "" {
		// 内容搜索：改用 rg 的默认搜索模式（不是 --files）
		args = []string{
			"--no-ignore",
			"--hidden",
			"--glob-case-insensitive",
			"--ignore-case",
			"--color", "never",
			"--no-heading",
			"--line-number",
			// Keep multiple matches per file so an API document's table of
			// contents does not hide its later detailed contract section.
			"--max-count", "10",
			"-g", "!.git/**",
			"-g", "!.idea/**",
			"-g", "!.next/**",
			"-g", "!.playwright-cli/**",
			"-g", "!bin/**",
			"-g", "!build/**",
			"-g", "!coverage/**",
			"-g", "!dist/**",
			"-g", "!node_modules/**",
			"-g", "!out/**",
			"-g", "!vendor/**",
			"-g", "!.venv/**",
			"-g", "!venv/**",
			"-g", "!__pycache__/**",
			"-g", "!site-packages/**",
			"-g", "!mingw32/**",
			"-g", "!mingw64/**",
		}
		if pattern != "" && pattern != "*" && pattern != "*.*" {
			args = append(args, "-g", pattern)
		}
		args = append(args, query, root)
		return args
	}
	args = append(args, root)
	return args
}

// parseRipgrepLine 解析 rg 输出的一行。
// --files 模式：每行是文件路径
// 搜索模式：每行是 "path:line:content" 或 "path-line-content"（rg 实际用冒号分隔）
func parseRipgrepLine(line, query string) (map[string]any, bool) {
	if query == "" {
		// --files 模式
		return map[string]any{"path": line}, true
	}
	// 搜索模式：path:line:content。内容本身经常包含冒号（例如
	// /service_instances/:sn_ip），所以必须识别数字行号分隔符，不能从
	// 右侧按最后两个冒号切分。非贪婪路径仍可正确跨过 Windows 盘符冒号。
	parts := ripgrepLinePattern.FindStringSubmatch(line)
	if len(parts) != 4 {
		return nil, false
	}
	return map[string]any{
		"path":    parts[1],
		"line":    parts[2],
		"snippet": abbreviate(redactLocalText(strings.TrimSpace(parts[3])), 300),
	}, true
}

func matchingLine(content, lowerQuery string) (int, string, bool) {
	query := strings.ToLower(strings.TrimSpace(lowerQuery))
	if query == "" {
		return 0, "", false
	}
	terms := searchTerms(query)
	bestLine, bestScore := 0, 0
	var bestSnippet string
	for index, line := range strings.Split(content, "\n") {
		lowerLine := strings.ToLower(line)
		score := 0
		if strings.Contains(lowerLine, query) {
			score += 100 + len(query)
		}
		matchedTerms := 0
		for _, term := range terms {
			if strings.Contains(lowerLine, term) {
				matchedTerms++
				score += len(term)
			}
		}
		// For a multi-word query, require all meaningful terms. This handles
		// natural-language searches without returning a line matching only a
		// generic word such as "status".
		if len(terms) > 1 && matchedTerms != len(terms) {
			continue
		}
		if score > bestScore {
			bestLine = index + 1
			bestScore = score
			bestSnippet = abbreviate(redactLocalText(strings.TrimSpace(line)), 300)
		}
	}
	if bestLine == 0 {
		return 0, "", false
	}
	return bestLine, bestSnippet, true
}

func searchTerms(query string) []string {
	parts := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	terms := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		if len(part) < 2 {
			continue
		}
		if _, exists := seen[part]; exists {
			continue
		}
		seen[part] = struct{}{}
		terms = append(terms, part)
	}
	return terms
}

type FileReadRequest struct {
	ProfileID string `json:"profileId"`
	Path      string `json:"path"`
	MaxBytes  int64  `json:"maxBytes,omitempty"`
	StartLine int    `json:"startLine,omitempty"`
	MaxLines  int    `json:"maxLines,omitempty"`
}

type FileReadTool struct{ targets TargetResolver }

func NewFileReadTool(targets TargetResolver) (*FileReadTool, error) {
	if targets == nil {
		return nil, errors.New("target resolver is required")
	}
	return &FileReadTool{targets: targets}, nil
}

func (t *FileReadTool) Definition() Definition {
	return Definition{Name: "read_file", Description: "Read a file inside an allowed workspace root"}
}

func (t *FileReadTool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var input FileReadRequest
	if err := json.Unmarshal(arguments, &input); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode arguments: %w", err)
	}
	path, err := resolveWorkspacePath(t.targets, input.ProfileID, input.Path)
	if err != nil {
		return domain.PreparedCall{}, err
	}
	if input.MaxBytes == 0 {
		input.MaxBytes = maxLocalOutputBytes
	}
	if input.MaxBytes < 1 || input.MaxBytes > maxLocalOutputBytes {
		return domain.PreparedCall{}, fmt.Errorf("maxBytes must be between 1 and %d", maxLocalOutputBytes)
	}
	if input.StartLine < 0 {
		return domain.PreparedCall{}, errors.New("startLine must be zero or greater")
	}
	if input.MaxLines < 0 || input.MaxLines > 2000 {
		return domain.PreparedCall{}, errors.New("maxLines must be between 0 and 2000")
	}
	if input.MaxLines > 0 && input.StartLine == 0 {
		input.StartLine = 1
	}
	input.Path = path
	return domain.PreparedCall{
		Summary:     "Read " + path,
		Annotations: domain.ToolAnnotations{ReadOnly: true, Idempotent: true, Sensitive: potentiallySensitivePath(path)},
		Input:       input,
	}, nil
}

func (t *FileReadTool) Execute(_ context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	input, ok := call.Input.(FileReadRequest)
	if !ok {
		return domain.ToolResult{}, errors.New("invalid prepared file read")
	}
	file, err := os.Open(input.Path)
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("open file: %w", err)
	}
	defer file.Close()
	if input.StartLine > 0 {
		return readFileLineRange(file, input)
	}
	contents, err := io.ReadAll(io.LimitReader(file, input.MaxBytes+1))
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("read file: %w", err)
	}
	truncated := int64(len(contents)) > input.MaxBytes
	if truncated {
		contents = contents[:input.MaxBytes]
	}
	if strings.IndexByte(string(contents), 0) >= 0 {
		return domain.ToolResult{}, errors.New("binary files are not supported")
	}
	return domain.ToolResult{
		Summary: fmt.Sprintf("Read %d bytes from %s", len(contents), filepath.Base(input.Path)),
		Data:    map[string]any{"path": input.Path, "content": redactLocalText(string(contents)), "truncated": truncated},
	}, nil
}

func readFileLineRange(file *os.File, input FileReadRequest) (domain.ToolResult, error) {
	maxLines := input.MaxLines
	if maxLines == 0 {
		maxLines = 200
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxLocalOutputBytes)
	var content strings.Builder
	lineNumber := 0
	selectedLines := 0
	endLine := input.StartLine - 1
	truncated := false
	for scanner.Scan() {
		lineNumber++
		if lineNumber < input.StartLine {
			continue
		}
		if selectedLines >= maxLines {
			truncated = true
			break
		}
		line := scanner.Text()
		separatorBytes := 0
		if selectedLines > 0 {
			separatorBytes = 1
		}
		if int64(content.Len()+separatorBytes+len(line)) > input.MaxBytes {
			truncated = true
			break
		}
		if strings.IndexByte(line, 0) >= 0 {
			return domain.ToolResult{}, errors.New("binary files are not supported")
		}
		if selectedLines > 0 {
			content.WriteByte('\n')
		}
		content.WriteString(line)
		selectedLines++
		endLine = lineNumber
	}
	if err := scanner.Err(); err != nil {
		return domain.ToolResult{}, fmt.Errorf("read file lines: %w", err)
	}
	return domain.ToolResult{
		Summary: fmt.Sprintf("Read lines %d-%d from %s", input.StartLine, endLine, filepath.Base(input.Path)),
		Data: map[string]any{
			"path":      input.Path,
			"content":   redactLocalText(content.String()),
			"startLine": input.StartLine,
			"endLine":   endLine,
			"truncated": truncated,
		},
	}, nil
}

// EvidenceResolver is implemented by the application runner/journal. Keeping
// this small interface here lets the evidence tool remain an ordinary
// registry tool without coupling the tools package to the application layer.
type EvidenceResolver interface {
	Evidence(runID, evidenceID string) (domain.Evidence, bool, error)
}

type EvidenceRequest struct {
	RunID      string `json:"runId"`
	EvidenceID string `json:"evidenceId"`
	MaxBytes   int    `json:"maxBytes,omitempty"`
}

type EvidenceTool struct {
	resolver EvidenceResolver
	readMu   sync.Mutex
	read     map[string]struct{} // 同 run 内已读过的 evidence ID，防重读循环
}

// ResetReads 在新 run 开始时清空已读记录，防止跨 run 误拦。
func (t *EvidenceTool) ResetReads() {
	t.readMu.Lock()
	defer t.readMu.Unlock()
	t.read = make(map[string]struct{})
}

func NewEvidenceTool(resolver EvidenceResolver) (*EvidenceTool, error) {
	if resolver == nil {
		return nil, errors.New("evidence resolver is required")
	}
	return &EvidenceTool{resolver: resolver, read: make(map[string]struct{})}, nil
}

func (t *EvidenceTool) Definition() Definition {
	return Definition{Name: "read_evidence", Description: "Read one previously captured diagnostic evidence item by ID"}
}

func (t *EvidenceTool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var input EvidenceRequest
	if err := json.Unmarshal(arguments, &input); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode arguments: %w", err)
	}
	input.RunID = strings.TrimSpace(input.RunID)
	input.EvidenceID = strings.TrimSpace(input.EvidenceID)
	if input.RunID == "" || input.EvidenceID == "" {
		return domain.PreparedCall{}, errors.New("runId and evidenceId are required")
	}
	if input.MaxBytes == 0 {
		input.MaxBytes = maxLocalOutputBytes
	}
	if input.MaxBytes < 1 || input.MaxBytes > maxLocalOutputBytes {
		return domain.PreparedCall{}, fmt.Errorf("maxBytes must be between 1 and %d", maxLocalOutputBytes)
	}
	return domain.PreparedCall{
		Summary:     fmt.Sprintf("Read diagnostic evidence %s", input.EvidenceID),
		Annotations: domain.ToolAnnotations{ReadOnly: true, Idempotent: true},
		Input:       input,
	}, nil
}

func (t *EvidenceTool) Execute(_ context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	input, ok := call.Input.(EvidenceRequest)
	if !ok {
		return domain.ToolResult{}, errors.New("invalid prepared evidence read")
	}
	evidence, found, err := t.resolver.Evidence(input.RunID, input.EvidenceID)
	if err != nil {
		return domain.ToolResult{}, err
	}
	if !found {
		return domain.ToolResult{}, fmt.Errorf("evidence not found: %s", input.EvidenceID)
	}
	// 防重读：同 run 内第二次读取同一 evidence 时，只返回摘要，不返回完整数据。
	// 大证据（如 NETCONF 109 条接口）被重读会立即触发 compaction，形成 compaction →
	// 重读 → compaction 的无限循环，最终耗尽 API 重试次数。
	readKey := input.RunID + "\x00" + input.EvidenceID
	t.readMu.Lock()
	_, already := t.read[readKey]
	if !already {
		t.read[readKey] = struct{}{}
	}
	t.readMu.Unlock()
	if already {
		return domain.ToolResult{
			Summary: fmt.Sprintf("Evidence %s already read (%s)", evidence.ID, evidence.Kind),
			Message: fmt.Sprintf(
				"This evidence (%s, %d bytes) was already read in this run. "+
					"Do not re-read it — the full data would cause another context compaction. "+
					"Use the information you already have.",
				evidence.Kind, evidenceSize(evidence.Data)),
			Metadata: map[string]string{
				"evidenceId":   evidence.ID,
				"kind":         evidence.Kind,
				"already_read": "true",
			},
		}, nil
	}
	data := evidence.Data
	encoded, marshalErr := json.Marshal(data)
	if marshalErr == nil && len(encoded) > input.MaxBytes {
		data = map[string]any{
			"truncated":     true,
			"rawPrefix":     string(encoded[:input.MaxBytes]),
			"originalBytes": len(encoded),
		}
	}
	return domain.ToolResult{
		Summary: fmt.Sprintf("Read evidence %s (%s)", evidence.ID, evidence.Kind),
		Data:    data,
		Metadata: map[string]string{
			"evidenceId": evidence.ID,
			"kind":       evidence.Kind,
		},
	}, nil
}

// evidenceSize 估算 evidence 数据的字节大小
func evidenceSize(data any) int {
	if data == nil {
		return 0
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return 0
	}
	return len(encoded)
}

type FileWriteRequest struct {
	ProfileID string `json:"profileId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

type FileWriteTool struct{ targets TargetResolver }

func NewFileWriteTool(targets TargetResolver) (*FileWriteTool, error) {
	if targets == nil {
		return nil, errors.New("target resolver is required")
	}
	return &FileWriteTool{targets: targets}, nil
}

func (t *FileWriteTool) Definition() Definition {
	return Definition{Name: "write_file", Description: "Write a file inside an allowed workspace root after approval"}
}

func (t *FileWriteTool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var input FileWriteRequest
	if err := json.Unmarshal(arguments, &input); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode arguments: %w", err)
	}
	path, err := resolveWorkspacePath(t.targets, input.ProfileID, input.Path)
	if err != nil {
		return domain.PreparedCall{}, err
	}
	input.Path = path
	if len(input.Content) > maxLocalOutputBytes {
		return domain.PreparedCall{}, fmt.Errorf("file content exceeds %d bytes", maxLocalOutputBytes)
	}
	_, statErr := os.Stat(path)
	return domain.PreparedCall{
		Summary: "Write " + path,
		Preview: fmt.Sprintf("Write %d bytes to %s", len(input.Content), path),
		Annotations: domain.ToolAnnotations{
			Destructive: statErr == nil,
			Idempotent:  true,
		},
		Input: input,
	}, nil
}

func (t *FileWriteTool) Execute(_ context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	input, ok := call.Input.(FileWriteRequest)
	if !ok {
		return domain.ToolResult{}, errors.New("invalid prepared file write")
	}
	if err := os.WriteFile(input.Path, []byte(input.Content), 0o600); err != nil {
		return domain.ToolResult{}, fmt.Errorf("write file: %w", err)
	}
	return domain.ToolResult{Summary: fmt.Sprintf("Wrote %d bytes to %s", len(input.Content), filepath.Base(input.Path)), Data: map[string]any{"path": input.Path, "bytes": len(input.Content)}}, nil
}

type ShellRequest struct {
	ProfileID        string `json:"profileId"`
	Shell            string `json:"shell,omitempty"`
	Command          string `json:"command"`
	WorkingDirectory string `json:"workingDirectory,omitempty"`
	TimeoutSeconds   int    `json:"timeoutSeconds,omitempty"`
}

type ShellTool struct{ targets TargetResolver }

func NewShellTool(targets TargetResolver) (*ShellTool, error) {
	if targets == nil {
		return nil, errors.New("target resolver is required")
	}
	return &ShellTool{targets: targets}, nil
}

func (t *ShellTool) Definition() Definition {
	return Definition{Name: "run_shell", Description: "Run a local PowerShell, cmd, or bash command starting in an allowed workspace after approval; the shell itself is not sandboxed"}
}

func (t *ShellTool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var input ShellRequest
	if err := json.Unmarshal(arguments, &input); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode arguments: %w", err)
	}
	input.Command = strings.TrimSpace(input.Command)
	if input.Command == "" {
		return domain.PreparedCall{}, errors.New("shell command is required")
	}
	if input.Shell == "" {
		input.Shell = "powershell"
	}
	if input.Shell != "powershell" && input.Shell != "cmd" && input.Shell != "bash" {
		return domain.PreparedCall{}, errors.New("shell must be powershell, cmd, or bash")
	}
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = 30
	}
	if input.TimeoutSeconds < 1 || input.TimeoutSeconds > 300 {
		return domain.PreparedCall{}, errors.New("shell timeout must be between 1 and 300 seconds")
	}
	workingDirectory, err := resolveWorkspaceDirectory(t.targets, input.ProfileID, input.WorkingDirectory)
	if err != nil {
		return domain.PreparedCall{}, err
	}
	input.WorkingDirectory = workingDirectory
	return domain.PreparedCall{
		Summary:     fmt.Sprintf("Run %s in %s: %s", input.Shell, workingDirectory, abbreviate(redactLocalText(input.Command), 300)),
		Preview:     abbreviate(redactLocalText(input.Command), 8000),
		Annotations: domain.ToolAnnotations{Destructive: true, OpenWorld: true},
		Input:       input,
	}, nil
}

func (t *ShellTool) Execute(ctx context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	input, ok := call.Input.(ShellRequest)
	if !ok {
		return domain.ToolResult{}, errors.New("invalid prepared shell command")
	}
	commandCtx, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
	defer cancel()
	var command *exec.Cmd
	switch input.Shell {
	case "cmd":
		command = exec.CommandContext(commandCtx, "cmd.exe", "/d", "/s", "/c", input.Command)
	case "bash":
		command = exec.CommandContext(commandCtx, "bash", "-lc", input.Command)
	default:
		command = exec.CommandContext(commandCtx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", input.Command)
	}
	command.Dir = input.WorkingDirectory
	output, err := command.CombinedOutput()
	truncated := len(output) > maxLocalOutputBytes
	if truncated {
		output = output[:maxLocalOutputBytes]
	}
	result := map[string]any{"output": redactLocalText(string(output)), "truncated": truncated, "workingDirectory": input.WorkingDirectory}
	if commandCtx.Err() != nil {
		return domain.ToolResult{}, fmt.Errorf("shell command timed out: %w", commandCtx.Err())
	}
	if err != nil {
		result["error"] = err.Error()
	}
	return domain.ToolResult{Summary: "Shell command completed", Data: result}, nil
}

var localSecretPattern = regexp.MustCompile(`(?i)(authorization|password|passwd|api[_-]?key|secret|token)(["']?\s*[:=]\s*["']?)([^\s,;"']+)`)

func redactLocalText(value string) string {
	return localSecretPattern.ReplaceAllString(value, "$1$2[REDACTED]")
}

func potentiallySensitivePath(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	if name == ".env" || strings.HasPrefix(name, ".env.") || name == "id_rsa" || name == "id_ed25519" || name == "credentials" || name == "credentials.json" || name == "secrets.json" || name == "secrets.yaml" || name == "secrets.yml" {
		return true
	}
	return false
}

func resolveWorkspacePath(targets TargetResolver, profileID, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is required")
	}
	roots, exists := targets.WorkspaceRoots(strings.TrimSpace(profileID))
	if !exists || len(roots) == 0 {
		return "", errors.New("profile has no allowed workspace roots")
	}
	if filepath.IsAbs(path) {
		resolved, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		if pathWithinAnyRoot(resolved, roots) {
			return resolved, nil
		}
		return "", errors.New("path is outside the allowed workspace roots")
	}
	resolved, err := filepath.Abs(filepath.Join(roots[0], path))
	if err != nil {
		return "", err
	}
	if !pathWithinAnyRoot(resolved, roots) {
		return "", errors.New("path is outside the allowed workspace roots")
	}
	return resolved, nil
}

func resolveWorkspaceDirectory(targets TargetResolver, profileID, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		roots, exists := targets.WorkspaceRoots(strings.TrimSpace(profileID))
		if !exists || len(roots) == 0 {
			return "", errors.New("profile has no allowed workspace roots")
		}
		path = roots[0]
	}
	resolved, err := resolveWorkspacePath(targets, profileID, path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect working directory: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("working directory is not a directory")
	}
	return resolved, nil
}

func pathWithinAnyRoot(path string, roots []string) bool {
	canonicalPath, err := canonicalPathForBoundary(path)
	if err != nil {
		return false
	}
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		absoluteRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		canonicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(canonicalRoot, canonicalPath)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
			return true
		}
	}
	return false
}

func canonicalPathForBoundary(path string) (string, error) {
	if _, err := os.Stat(path); err == nil {
		return filepath.EvalSymlinks(path)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func abbreviate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
