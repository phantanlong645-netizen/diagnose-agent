package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// diagnosticSummaryInstruction 是上下文压缩时发给摘要模型的系统提示词（中文）。
// 它强制摘要输出九个固定 section，并要求：
//   - goal 只能是最近一次用户请求（跨 run 续聊时锚定当前目标）；
//   - 保留精确的 API 路径 / RPC 操作 / 行号 / evidence ID；
//   - 禁止编造、禁止输出密钥类信息，大段结果只引用 evidence ID。
const diagnosticSummaryInstruction = `请为当前 OLT 诊断生成严格 JSON 延续摘要。只输出一个 JSON object，不要 Markdown fence 或额外文字，且必须恰好包含以下九个字段：
{"goal":"","target":"","confirmed_facts":[],"observed_errors":[],"rejected_hypotheses":[],"completed_checks":[],"open_questions":[],"next_action":"","do_not_repeat":[]}
其中 goal、target、next_action 必须是字符串，其余字段必须是字符串数组；九个字段都必须存在，即使没有内容也使用空字符串或空数组。goal 只能是最近一次用户请求。保留精确的 API 路径、HTTP 方法和状态码、RPC 操作与 filter、文件名、行号、错误文本、endpoint ID 和 evidence ID。不要编造信息，不要输出凭证、token 或密码；大段结果只引用 evidence ID。`

// contextManager 是自定义的上下文压缩中间件，替换了 Eino 自带的 summarization 中间件。
// 触发逻辑（何时压缩）由业务层控制，而不是交给第三方配置策略：
//
//   - triggerTokens：单次请求上下文总 token 数达到该值即触发（按 modelMessageBytes 估算）；
//   - triggerMessages：消息条数达到该值即触发（消息数 OR token 数，任一满足）；
//   - minNewMessages：距离上次压缩至少要新增多少条消息才允许再次压缩，
//     避免"每次触发就压缩"导致反复调用摘要模型，把慢 run 拖得更慢。
//
// 压缩动作本身（保留什么、摘要是否可接受）也由本包决定：
//   - 先拉取诊断记忆（plan + facts）作为 pinned system 消息，随摘要一起作为输入；
//   - 用 projectMessagesForSummary 对历史做"去重、截断、保首尾"的投影；
//   - 摘要结果通过 validateDiagnosticSummary 校验九个 section 齐全后才替换消息流；
//   - 用 recentStableMessages 保留最近一批稳定消息（含工具结果）追加在摘要之后，
//     保证新一轮模型调用仍能看到最近的证据，而摘要只是"阵地前移"而非"全量抹除"。
type diagnosticSummary struct {
	Goal               string   `json:"goal"`
	Target             string   `json:"target"`
	ConfirmedFacts     []string `json:"confirmed_facts"`
	ObservedErrors     []string `json:"observed_errors"`
	RejectedHypotheses []string `json:"rejected_hypotheses"`
	CompletedChecks    []string `json:"completed_checks"`
	OpenQuestions      []string `json:"open_questions"`
	NextAction         string   `json:"next_action"`
	DoNotRepeat        []string `json:"do_not_repeat"`
}

type contextManager struct {
	adk.BaseChatModelAgentMiddleware
	model             model.BaseModel[*schema.Message] // 摘要用模型（与主 agent 复用同一个 ChatModel）
	memory            func() (*schema.Message, error)  // 拉取诊断记忆（plan + facts）的回调
	onFailure         func(error)
	triggerTokens     int        // token 维度触发阈值（估算值）
	triggerMessages   int        // 消息条数维度触发阈值
	minNewMessages    int        // 两次压缩之间至少新增的消息数
	mu                sync.Mutex // 保护 lastCompactedSize
	lastCompactedSize int        // 上次压缩时的消息总数，用于 minNewMessages 节流
}

// BeforeModelRewriteState 在每次 ChatModel 生成前调用，是"是否压缩"的判定点。
//
// 触发判定：消息条数 >= triggerMessages 或估算 token >= triggerTokens，
// 且距离上次压缩新增的消息数 >= minNewMessages。两个维度任一满足即压缩，
// 同时合并触发条件避免"条数够了但 token 很小"或"token 很大但条数很少"的盲区。
//
// 压缩流程：
//  1. 拉取诊断记忆（pinned system 消息）；
//  2. projectMessagesForSummary 投影历史（剥除冗余 system、按块截断保首尾）；
//  3. 组装「摘要指令 + 诊断记忆 + 投影历史」发给摘要模型；
//  4. validateDiagnosticSummary 校验摘要完整性；
//  5. 用「摘要 user 消息 + 最近稳定消息」替换 state.Messages。
//
// 返回的新 state 会被 Eino 写入，后续的 GenModelInput / 模型调用都基于压缩后的历史。
func (m *contextManager) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	m.mu.Lock()
	messageCount := len(state.Messages)
	// 判定 1/3：消息条数达到阈值，且与上次压缩间隔足够。
	shouldCompact := messageCount >= m.triggerMessages && messageCount-m.lastCompactedSize >= m.minNewMessages
	if !shouldCompact {
		// 判定 2/3：条数不足但 token 数达到阈值（长消息场景）。
		// token 用 modelMessageBytes 估算（4 字节/token），与 AgentEngine 的估算口径一致。
		tokens := 0
		for _, message := range state.Messages {
			if message != nil {
				tokens += (modelMessageBytes(message) + 3) / 4
			}
		}
		shouldCompact = tokens >= m.triggerTokens && messageCount-m.lastCompactedSize >= m.minNewMessages
	}
	if !shouldCompact {
		m.mu.Unlock()
		return ctx, state, nil
	}
	// 记录本次压缩位置用于节流，然后执行压缩。
	m.lastCompactedSize = messageCount
	m.mu.Unlock()

	// 空消息流无需压缩。
	if len(state.Messages) == 0 {
		return ctx, state, nil
	}
	// 1. 拉取诊断记忆：plan + facts 作为 pinned 上下文，随摘要一起输入。
	memory, err := m.memory()
	if err != nil {
		return m.compactionFailure(ctx, state, fmt.Errorf("load diagnostic context memory: %w", err))
	}
	// 2. 投影历史：去重 system、按 block 截断、保留首尾重要片段。
	projected := projectMessagesForSummary(state.Messages)
	// 3. 组装摘要输入：摘要指令 + 诊断记忆 + 投影历史。
	input := make([]*schema.Message, 0, len(projected)+2)
	input = append(input, schema.SystemMessage(diagnosticSummaryInstruction))
	if memory != nil {
		input = append(input, memory)
	}
	input = append(input, projected...)
	// 4. 调用摘要模型并校验输出完整性。
	summary, err := m.model.Generate(ctx, input)
	if err != nil {
		return m.compactionFailure(ctx, state, fmt.Errorf("generate diagnostic context summary: %w", err))
	}
	if summary == nil {
		return m.compactionFailure(ctx, state, errors.New("diagnostic context summary is empty"))
	}
	validated, err := validateDiagnosticSummary(summary.Content)
	if err != nil {
		return m.compactionFailure(ctx, state, err)
	}

	// 5. 重组消息流：摘要作为第一条 user 消息，后面追加最近一批稳定消息。
	//    这样既把历史"阵地前移"控制住了上下文体积，又保留了最近的证据供模型直接使用。
	recent := recentStableMessages(state.Messages, summaryRecentMessageCount)
	compacted := make([]*schema.Message, 0, len(recent)+1)
	compacted = append(compacted, schema.UserMessage("[诊断上下文摘要]\n"+validated))
	compacted = append(compacted, recent...)
	after := *state
	after.Messages = compacted
	return ctx, &after, nil
}

func (m *contextManager) compactionFailure(ctx context.Context, state *adk.ChatModelAgentState, err error) (context.Context, *adk.ChatModelAgentState, error) {
	if m.onFailure != nil {
		m.onFailure(err)
	}
	return ctx, state, nil
}

// recentStableMessages 取消息流末尾 limit 条作为"最近稳定消息"。
// 如果末尾是从 assistant 的 tool call 开始、以 tool result 结束的完整轮次，
// 会整体保留；若 limit 恰好切在一条 tool result 中间（该 tool result 前一行
// 是 assistant tool call），则把起点前移到覆盖完整的 assistant 消息，
// 避免压缩后留下孤立的 tool result（没有对应 ToolCallID 前文）。
func recentStableMessages(messages []*schema.Message, limit int) []*schema.Message {
	if limit <= 0 || len(messages) <= limit {
		return append([]*schema.Message(nil), messages...)
	}
	start := len(messages) - limit
	// 如果起点落在 tool 消息上，说明被截断的可能是"assistant tool call + 对应 result"
	// 的完整轮次，向前扩展起点直到不再是 tool 消息，保持轮次完整性。
	for start < len(messages) && messages[start] != nil && messages[start].Role == schema.Tool {
		start++
	}
	return append([]*schema.Message(nil), messages[start:]...)
}

// validateDiagnosticSummary 校验摘要是严格 JSON object，且恰好包含九个字段及其类型。
func validateDiagnosticSummary(content string) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader([]byte(content)))
	decoder.DisallowUnknownFields()
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return "", fmt.Errorf("diagnostic context summary is invalid JSON: %w", err)
	}
	const fieldCount = 9
	if len(raw) != fieldCount {
		return "", fmt.Errorf("diagnostic context summary must contain exactly nine fields, got %d", len(raw))
	}
	stringFields := map[string]struct{}{"goal": {}, "target": {}, "next_action": {}}
	arrayFields := map[string]struct{}{"confirmed_facts": {}, "observed_errors": {}, "rejected_hypotheses": {}, "completed_checks": {}, "open_questions": {}, "do_not_repeat": {}}
	for field := range stringFields {
		value, ok := raw[field]
		if !ok {
			return "", fmt.Errorf("diagnostic context summary missing field %q", field)
		}
		var text string
		if len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &text) != nil {
			return "", fmt.Errorf("diagnostic context summary field %q must be a string", field)
		}
	}
	for field := range arrayFields {
		value, ok := raw[field]
		if !ok {
			return "", fmt.Errorf("diagnostic context summary missing field %q", field)
		}
		var items []string
		if len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &items) != nil {
			return "", fmt.Errorf("diagnostic context summary field %q must be a string array", field)
		}
	}
	encodedRaw, err := json.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("encode diagnostic context summary fields: %w", err)
	}
	var summary diagnosticSummary
	if err := json.Unmarshal(encodedRaw, &summary); err != nil {
		return "", fmt.Errorf("diagnostic context summary has invalid field types: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return "", errors.New("diagnostic context summary contains trailing JSON")
		}
		return "", fmt.Errorf("diagnostic context summary contains trailing data: %w", err)
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		return "", fmt.Errorf("encode diagnostic context summary: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return "", fmt.Errorf("inspect diagnostic context summary: %w", err)
	}
	if len(fields) != 9 {
		return "", fmt.Errorf("diagnostic context summary must contain exactly nine fields, got %d", len(fields))
	}
	return string(encoded), nil
}
