// Package rag 是从 paicli-go 移植的本地代码语义索引（RAG search_code 的底层）。
//
// 与 paicli-go 原版的差异：
//   - 支持多个 workspace root（OLT target profile 可配置多个工作区），
//     chunk path 记录为"相对各自 root"的路径。
//   - BuildRoots 接受 context，诊断 run 取消时构建可中断。
//   - NeedsSameRoots 用于检测索引与当前 profile 配置的 root 集合是否漂移。
//
// 算法保持一致：TF 词频向量 + 余弦相似度 + 路径/符号名加权；
// Go 文件额外做 AST 解析，产出函数级 chunk 和 import 关系。
package rag

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Index 是代码语义索引：保存被索引文件的 chunk 集合及其相互依赖关系，
// 支持内存构建（BuildRoots）与 JSON 持久化（Save/Load）。
type Index struct {
	Path      string     `json:"-"`
	Roots     []string   `json:"roots"`
	Chunks    []Chunk    `json:"chunks"`
	Relations []Relation `json:"relations"`
}

// Chunk 是索引中的一个文本片段，可对应整个文件或单个 Go 函数；
// Terms 为预计算的 TF 词频向量，避免检索时重复分词。
type Chunk struct {
	Path      string         `json:"path"`
	StartLine int            `json:"start_line"`
	EndLine   int            `json:"end_line"`
	Kind      string         `json:"kind"`
	Symbol    string         `json:"symbol"`
	Text      string         `json:"text"`
	Terms     map[string]int `json:"terms"`
}

// Relation 表示两个对象之间的一条代码关系（如 imports / contains）。
type Relation struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// Result 是一次搜索命中的 chunk 及其相关度分数（越大越相关）。
type Result struct {
	Chunk Chunk
	Score float64
}

// NewIndex 创建索引，path 为 Save/Load 使用的持久化文件路径。
func NewIndex(path string) *Index {
	return &Index{Path: path}
}

// NeedsSameRoots 判断已加载索引的 root 集合是否与给定 roots 一致（顺序无关）。
// 不一致时调用方应重建索引，避免命中已经移除的工作区。
func (i *Index) NeedsSameRoots(roots []string) bool {
	if len(i.Roots) != len(roots) {
		return false
	}
	seen := make(map[string]int, len(i.Roots))
	for _, root := range i.Roots {
		seen[root]++
	}
	for _, root := range roots {
		seen[root]--
		if seen[root] < 0 {
			return false
		}
	}
	return true
}

// BuildRoots 在多个 workspace root 上重建索引。ctx 取消时返回 ctx.Err()。
func (i *Index) BuildRoots(ctx context.Context, roots []string) error {
	absoluteRoots := make([]string, 0, len(roots))
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		absoluteRoots = append(absoluteRoots, absolute)
	}
	i.Roots = absoluteRoots
	i.Chunks = nil
	i.Relations = nil
	for _, root := range absoluteRoots {
		if err := i.walkRoot(ctx, root); err != nil {
			return err
		}
	}
	return nil
}

// walkRoot 递归遍历 root 下的文件：跳过常见构建产物/依赖目录，
// 仅索引 isCodeFile 认可的文件，并在每步检查 ctx 以便构建可被取消。
func (i *Index) walkRoot(ctx context.Context, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			switch d.Name() {
			case ".git", ".idea", ".next", ".playwright-cli", "node_modules", "target", "dist", "build", "coverage", "vendor", "bin", "out", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		if !isCodeFile(path) {
			return nil
		}
		return i.addFile(root, path)
	})
}

// addFile 将单个文件作为整文件 chunk 加入索引，路径记录为相对 root 的形式；
// .go 文件额外解析 AST 提取函数级 chunk 与 import 关系。
func (i *Index) addFile(root, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	rel, relErr := filepath.Rel(root, path)
	if relErr != nil {
		rel = path
	}
	text := string(b)
	lines := strings.Count(text, "\n") + 1
	i.Chunks = append(i.Chunks, Chunk{
		Path:      filepath.ToSlash(rel),
		StartLine: 1,
		EndLine:   lines,
		Kind:      "file",
		Symbol:    filepath.ToSlash(rel),
		Text:      trimText(text, 8000),
		Terms:     terms(text),
	})
	if filepath.Ext(path) == ".go" {
		i.addGoSymbols(path, filepath.ToSlash(rel), text)
	}
	return nil
}

// addGoSymbols 解析 Go 源码 AST：为每个函数生成一个 chunk
// （符号名为"接收者.函数名"），并记录"文件包含符号"与"文件导入依赖"两类关系。
func (i *Index) addGoSymbols(path, rel, text string) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, text, parser.ParseComments)
	if err != nil {
		return
	}
	for _, spec := range file.Imports {
		to := strings.Trim(spec.Path.Value, `"`)
		i.Relations = append(i.Relations, Relation{From: rel, To: to, Kind: "imports"})
	}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		start := fset.Position(fn.Pos()).Line
		end := fset.Position(fn.End()).Line
		snippet := lines(text, start, end)
		sym := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			sym = exprString(fn.Recv.List[0].Type) + "." + sym
		}
		i.Chunks = append(i.Chunks, Chunk{
			Path:      rel,
			StartLine: start,
			EndLine:   end,
			Kind:      "function",
			Symbol:    sym,
			Text:      trimText(snippet, 8000),
			Terms:     terms(sym + "\n" + snippet),
		})
		i.Relations = append(i.Relations, Relation{From: rel, To: sym, Kind: "contains"})
		return false
	})
}

func (i *Index) Save() error {
	if err := os.MkdirAll(filepath.Dir(i.Path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(i.Path, b, 0o600)
}

// Load 从 Path 读取并解析 JSON 格式的索引。
func (i *Index) Load() error {
	b, err := os.ReadFile(i.Path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, i)
}

// Search 返回与 query 最相关的 topK 个 chunk。相关性 = TF 余弦
// + 路径子串命中加成 0.4 + 符号名子串命中加成 0.8（精确符号名检索优先）。
func (i *Index) Search(query string, topK int) []Result {
	if topK <= 0 {
		topK = 8
	}
	q := terms(query)
	lowerQuery := strings.ToLower(strings.TrimSpace(query))
	var out []Result
	for _, chunk := range i.Chunks {
		score := cosine(q, chunk.Terms)
		if lowerQuery != "" {
			if strings.Contains(strings.ToLower(chunk.Path), lowerQuery) {
				score += 0.4
			}
			if strings.Contains(strings.ToLower(chunk.Symbol), lowerQuery) {
				score += 0.8
			}
		}
		if score > 0 {
			out = append(out, Result{Chunk: chunk, Score: score})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	if len(out) > topK {
		out = out[:topK]
	}
	return out
}

// RelationsFrom 返回以指定文件为起点的代码关系（import / contains），
// 用于把"这个文件依赖谁、定义了哪些符号"作为证据返回给模型。
func (i *Index) RelationsFrom(rel string) []Relation {
	out := make([]Relation, 0)
	for _, relation := range i.Relations {
		if relation.From == rel {
			out = append(out, relation)
		}
	}
	return out
}

func (c Chunk) Preview() string {
	return trimText(strings.TrimSpace(c.Text), 500)
}

var tokenRE = regexp.MustCompile(`[A-Za-z0-9_\p{Han}]+`)

func terms(s string) map[string]int {
	out := map[string]int{}
	for _, t := range tokenRE.FindAllString(strings.ToLower(s), -1) {
		if len([]rune(t)) < 2 {
			continue
		}
		out[t]++
	}
	return out
}

func cosine(a, b map[string]int) float64 {
	var dot, aa, bb float64
	for k, av := range a {
		aa += float64(av * av)
		if bv, ok := b[k]; ok {
			dot += float64(av * bv)
		}
	}
	for _, bv := range b {
		bb += float64(bv * bv)
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return dot / (math.Sqrt(aa) * math.Sqrt(bb))
}

func isCodeFile(path string) bool {
	switch filepath.Ext(path) {
	case ".go", ".java", ".ts", ".tsx", ".js", ".jsx", ".py", ".md", ".yaml", ".yml", ".json":
		return true
	default:
		return false
	}
}

// trimText 将文本截断到 n 字节，超出部分以 "\n..." 结尾。
func trimText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n..."
}

// lines 提取文本中 [start, end] 行号区间的内容（行号从 1 开始）。
func lines(text string, start, end int) string {
	all := strings.Split(text, "\n")
	if start < 1 {
		start = 1
	}
	if end > len(all) {
		end = len(all)
	}
	if start > end {
		return ""
	}
	return strings.Join(all[start-1:end], "\n")
}

// exprString 将 AST 表达式还原为可读字符串（如 *pkg.Type.Receiver），用于拼接符号名。
func exprString(expr ast.Expr) string {
	switch x := expr.(type) {
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	default:
		return "receiver"
	}
}
