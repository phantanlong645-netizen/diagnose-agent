package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// MapReducer 把超长文本切成分片后并行执行 map（每个分片独立压缩），
// 再按顺序归并（reduce）成单一结果。并发上限由 MaxParallel 控制。
//
// 与一次性整体摘要的区别：map 阶段先对每个局部片段做无损保留式的压缩，
// reduce 阶段再对压缩后的片段做全局归并。单分片时直接走 MapFn，不触发 reduce。
type MapReducer struct {
	MaxShardChars int
	MaxParallel   int
	MapFn         func(ctx context.Context, shard string, index int) (string, error)
	ReduceFn      func(ctx context.Context, summaries []string) (string, error)
}

// Run 执行 map-reduce。text 为空时退化为对空串的一次 MapFn 调用。
func (m *MapReducer) Run(ctx context.Context, text string) (string, error) {
	if m.MapFn == nil || m.ReduceFn == nil {
		return "", fmt.Errorf("map-reduce requires both MapFn and ReduceFn")
	}
	shards := splitShards(text, m.MaxShardChars)
	if len(shards) <= 1 {
		return m.MapFn(ctx, text, 0)
	}

	maxParallel := m.MaxParallel
	if maxParallel <= 0 {
		maxParallel = 4
	}
	if maxParallel > len(shards) {
		maxParallel = len(shards)
	}

	// runCtx 是 map 阶段专用的上下文：任意一个分片失败时立刻 cancel(),
	// 让还没启动的分片不再开始执行，尽快收敛。reduce 阶段仍使用调用方原始的 ctx
	// （此时 map 已全部归队，deferred cancel 要等 Run 返回后才生效，不受影响）。
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	summaries := make([]string, len(shards))
	// sem 是并发信号量（容量 maxParallel）：通过"先取令牌再起 goroutine"的方式
	// 限制同时在跑的 map worker 数量，避免分片太多时无限开 goroutine。
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

loop:
	for index, shard := range shards {
		select {
		case sem <- struct{}{}:
		case <-runCtx.Done():
			break loop
		}
		if runCtx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(index int, shard string) {
			defer wg.Done()
			defer func() { <-sem }() // goroutine 结束归还令牌
			out, err := m.MapFn(runCtx, shard, index)
			mu.Lock()
			// 记下第一个错误并触发 cancel：后续 map worker 因 ctx 已取消
			// 会在取令牌或调用 MapFn 时快速失败，避免无意义的重复请求。
			// 注意即使失败也要把 out/summaries[index] 写回（可能是部分结果）。
			if err != nil && firstErr == nil {
				firstErr = err
				cancel()
			}
			summaries[index] = out
			mu.Unlock()
		}(index, shard)
	}
	wg.Wait()
	if firstErr != nil {
		return "", firstErr
	}
	return m.ReduceFn(ctx, summaries)
}

// splitShards 把文本按 maxChars 切块，尽量在段落/换行边界断开，避免切断句子。
// 返回的分片已 TrimSpace 且不会包含空串。
//
// 注意：换行处回退时必须保证 idx > 0（即 end > start 一定成立）。
// 若不加该约束，当 maxChars 很小（例如 1）且窗口恰好是单个 "\n" 时，
// LastIndex 返回 0，满足 idx >= maxChars/2 会把 end 设回 start，导致死循环。
func splitShards(text string, maxChars int) []string {
	if maxChars <= 0 {
		maxChars = 8000
	}
	if len(text) <= maxChars {
		return []string{text}
	}
	shards := make([]string, 0, len(text)/maxChars+1)
	start := 0
	for start < len(text) {
		end := start + maxChars
		if end > len(text) {
			end = len(text)
		} else if end < len(text) {
			window := text[start:end]
			// 优先在段落边界（\n\n）断开；找不到再退到行边界（\n）。
			// 只有"切分点位于窗口后半段"才回退，否则保持整块，避免过度切碎。
			if idx := strings.LastIndex(window, "\n\n"); idx > 0 && idx >= maxChars/2 {
				end = start + idx
			} else if idx := strings.LastIndex(window, "\n"); idx > 0 && idx >= maxChars/2 {
				end = start + idx
			}
		}
		if shard := strings.TrimSpace(text[start:end]); shard != "" {
			shards = append(shards, shard)
		}
		start = end
	}
	if len(shards) == 0 {
		return []string{text}
	}
	return shards
}

// mapShardPrompt 是 map 阶段发给模型的系统提示词（保持英文）。
// 要求对单个分片进行"保留精确标识符/路由/错误串/行号/证据引用"的密集无损压缩。
const mapShardPrompt = `You are the map step of a map-reduce summarizer.
Summarize the following text chunk in Simplified Chinese, preserving exact identifiers, API routes, error strings, filenames, line numbers, and evidence references verbatim. Keep the summary dense and lossless for the reduce step. Do not invent content.`

// reduceShardPrompt 是 reduce 阶段发给模型的系统提示词（保持英文）。
// 要求把多个分片摘要合并成一份连贯、无冗余的总结，同样保留全部精确标识符。
const reduceShardPrompt = `You are the reduce step of a map-reduce summarizer.
Merge the following chunk summaries into one coherent, non-redundant summary in Simplified Chinese. Preserve every exact identifier, route, error string, filename, line number, and evidence reference. Do not invent content absent from the inputs.`

// MapReduceSummarize 用当前 ChatModel 对超长文本做 map-reduce 分片压缩。
// maxShardChars 控制每个分片大小，maxParallel 控制并行摘要的并发上限。
func (e *Engine) MapReduceSummarize(ctx context.Context, text string, maxShardChars, maxParallel int) (string, error) {
	e.mu.RLock()
	chatModel := e.model
	e.mu.RUnlock()
	if chatModel == nil {
		return "", fmt.Errorf("chat model is not configured")
	}
	if maxShardChars <= 0 {
		maxShardChars = 8000
	}
	if maxParallel <= 0 {
		maxParallel = 4
	}

	reducer := &MapReducer{
		MaxShardChars: maxShardChars,
		MaxParallel:   maxParallel,
		MapFn: func(ctx context.Context, shard string, index int) (string, error) {
			response, err := chatModel.Generate(ctx, []*schema.Message{
				schema.SystemMessage(mapShardPrompt),
				schema.UserMessage(shard),
			})
			if err != nil {
				return "", err
			}
			return response.Content, nil
		},
		ReduceFn: func(ctx context.Context, summaries []string) (string, error) {
			response, err := chatModel.Generate(ctx, []*schema.Message{
				schema.SystemMessage(reduceShardPrompt),
				schema.UserMessage(strings.Join(summaries, "\n\n---\n\n")),
			})
			if err != nil {
				return "", err
			}
			return response.Content, nil
		},
	}
	return reducer.Run(ctx, text)
}
