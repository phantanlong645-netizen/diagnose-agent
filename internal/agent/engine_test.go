package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"olt-diagnostic-agent/internal/domain"
)

func TestRedactModelText(t *testing.T) {
	input := `curl -H "Authorization: Bearer live-token" -H "Token: ${TOKEN}" --data '{"password":"secret-value"}' <token>jwt-value</token>`
	redacted := redactModelText(input)

	for _, value := range []string{"live-token", "${TOKEN}", "secret-value", "jwt-value"} {
		if strings.Contains(redacted, value) {
			t.Fatalf("redacted model text still contains %q: %s", value, redacted)
		}
	}
	if !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("expected redaction marker in %q", redacted)
	}
}

func TestSanitizeModelMessagesDoesNotMutateInput(t *testing.T) {
	original := []*schema.Message{{
		Role:    schema.Assistant,
		Content: "Token: ${TOKEN}",
	}}
	safe := sanitizeModelMessages(original)

	if original[0].Content != "Token: ${TOKEN}" {
		t.Fatalf("sanitization mutated original message: %q", original[0].Content)
	}
	if strings.Contains(safe[0].Content, "${TOKEN}") {
		t.Fatalf("sanitized message still contains token placeholder: %q", safe[0].Content)
	}
}

func TestDisplayableAssistantReasoningCollectsAndRedactsProviderOutput(t *testing.T) {
	message := &schema.Message{
		Role:             schema.Assistant,
		Content:          `<think>检查上游依赖。</think>最终正文`,
		ReasoningContent: `先读取状态。 {"password":"device-secret"}`,
		AssistantGenMultiContent: []schema.MessageOutputPart{{
			Type:      schema.ChatMessagePartTypeReasoning,
			Reasoning: &schema.MessageOutputReasoning{Text: "对照现有证据。"},
		}},
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
			CompletionTokensDetails: schema.CompletionTokensDetails{ReasoningTokens: 37},
		}},
	}

	content, tokens := displayableAssistantReasoning(message)
	for _, expected := range []string{"先读取状态", "对照现有证据", "检查上游依赖"} {
		if !strings.Contains(content, expected) {
			t.Fatalf("reasoning is missing %q: %q", expected, content)
		}
	}
	if strings.Contains(content, "device-secret") || !strings.Contains(content, "[REDACTED]") {
		t.Fatalf("reasoning was not redacted: %q", content)
	}
	if strings.Contains(content, "最终正文") {
		t.Fatalf("visible answer leaked into reasoning: %q", content)
	}
	if tokens != 37 {
		t.Fatalf("reasoning tokens = %d, want 37", tokens)
	}
}

func TestDisplayableAssistantReasoningDeduplicatesAndBoundsUnicode(t *testing.T) {
	repeated := strings.Repeat("诊", reasoningDisplayMaxRunes+100)
	content, _ := displayableAssistantReasoning(&schema.Message{
		Role:             schema.Assistant,
		Content:          "<think>" + repeated + "</think>",
		ReasoningContent: repeated,
	})

	if len([]rune(content)) > reasoningDisplayMaxRunes {
		t.Fatalf("reasoning length = %d runes, want <= %d", len([]rune(content)), reasoningDisplayMaxRunes)
	}
	if strings.Count(content, "...[truncated]...") != 1 {
		t.Fatalf("reasoning was not deduplicated and bounded once: %q", content)
	}
}

func TestConsumeAgentEventMessageStreamsVisibleContentWithoutThinkBlocks(t *testing.T) {
	stream := schema.StreamReaderFromArray([]*schema.Message{
		schema.AssistantMessage("<thi", nil),
		schema.AssistantMessage("nk>private reasoning</thi", nil),
		schema.AssistantMessage("nk>根据当前证据，", nil),
		schema.AssistantMessage("NTP 请求失败。", nil),
	})
	event := adk.EventFromMessage(nil, stream, schema.Assistant, "")
	var deltas strings.Builder

	message, err := consumeAgentEventMessage(event, func(delta string) error {
		deltas.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := deltas.String(), "根据当前证据，NTP 请求失败。"; got != want {
		t.Fatalf("visible deltas = %q, want %q", got, want)
	}
	if got, want := visibleAssistantContent(message.Content), deltas.String(); got != want {
		t.Fatalf("completed visible content = %q, streamed content = %q", got, want)
	}
	if strings.Contains(deltas.String(), "private reasoning") || strings.Contains(deltas.String(), "<thi") {
		t.Fatalf("reasoning leaked into visible stream: %q", deltas.String())
	}
}

func TestConsumeAgentEventMessageKeepsEinoToolCallConcatenation(t *testing.T) {
	index := 0
	stream := schema.StreamReaderFromArray([]*schema.Message{
		schema.AssistantMessage("", []schema.ToolCall{{
			Index: &index,
			ID:    "call-1",
			Type:  "function",
			Function: schema.FunctionCall{
				Name:      "netconf_rpc",
				Arguments: `{"rpc":"<get`,
			},
		}}),
		schema.AssistantMessage("", []schema.ToolCall{{
			Index: &index,
			Function: schema.FunctionCall{
				Arguments: `/>"}`,
			},
		}}),
	})
	event := adk.EventFromMessage(nil, stream, schema.Assistant, "")
	deltaCalls := 0

	message, err := consumeAgentEventMessage(event, func(string) error {
		deltaCalls++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if deltaCalls != 0 {
		t.Fatalf("tool-call-only stream emitted %d visible deltas, want 0", deltaCalls)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("merged tool calls = %d, want 1", len(message.ToolCalls))
	}
	call := message.ToolCalls[0]
	if call.ID != "call-1" || call.Function.Name != "netconf_rpc" || call.Function.Arguments != `{"rpc":"<get/>"}` {
		t.Fatalf("tool call was not merged by Eino semantics: %+v", call)
	}
}

func TestRetainModelMessagesDropsReasoningWithoutAttachmentMarker(t *testing.T) {
	retained := retainModelMessages([]*schema.Message{{
		Role:             schema.Assistant,
		Content:          "最终结论",
		ReasoningContent: "内部推理",
		AssistantGenMultiContent: []schema.MessageOutputPart{{
			Type:      schema.ChatMessagePartTypeReasoning,
			Reasoning: &schema.MessageOutputReasoning{Text: "分块推理"},
		}},
	}})

	if len(retained) != 1 {
		t.Fatalf("retained message count = %d, want 1", len(retained))
	}
	if retained[0].ReasoningContent != "" || len(retained[0].AssistantGenMultiContent) != 0 {
		t.Fatalf("reasoning was retained in conversation context: %+v", retained[0])
	}
	if retained[0].Content != "最终结论" {
		t.Fatalf("reasoning was represented as an attachment or visible text: %q", retained[0].Content)
	}
}

func TestBuildUserMessageWithImageUsesMultiContent(t *testing.T) {
	message := buildUserMessage(domain.Run{
		Goal: "诊断图片中的告警",
		Images: []domain.ImageAttachment{{
			Filename: "alarm.png",
			MimeType: "image/png",
			Data:     "aGVsbG8=",
		}},
	})

	if message.Content != "" || len(message.UserInputMultiContent) != 0 {
		t.Fatalf("image message must not set competing content fields: %+v", message)
	}
	if len(message.MultiContent) != 2 {
		t.Fatalf("expected text and image input parts, got %d", len(message.MultiContent))
	}
	image := message.MultiContent[1]
	if image.Type != schema.ChatMessagePartTypeImageURL || image.ImageURL == nil {
		t.Fatalf("expected image input part, got %+v", image)
	}
	if image.ImageURL.URL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("unexpected image URL: %q", image.ImageURL.URL)
	}
}

func TestBuildUserMessageWithoutImageUsesTextContent(t *testing.T) {
	message := buildUserMessage(domain.Run{Goal: "检查 ONU 状态"})
	if message.Role != schema.User || message.Content != "检查 ONU 状态" {
		t.Fatalf("unexpected text user message: %+v", message)
	}
	if len(message.UserInputMultiContent) != 0 || len(message.MultiContent) != 0 {
		t.Fatalf("text user message must not contain multimodal parts: %+v", message)
	}
}

func TestExplainModelProviderError(t *testing.T) {
	original := errors.New("status code: 422, message: input new_sensitive (1026)")
	explained := explainModelProviderError(original)
	if !strings.Contains(explained.Error(), "MiniMax 1026") || !errors.Is(explained, original) {
		t.Fatalf("unexpected provider error explanation: %v", explained)
	}
}

func TestManualDraftInstructionRoutesUserSideVSIToLT(t *testing.T) {
	for _, required := range []string{
		"User-side/ONU VSI",
		"LT endpoint that hosts the ONT",
		"never replace them with an IHUB VPLS/VP query",
		"Ask for the exact LT endpoint",
	} {
		if !strings.Contains(manualDraftInstruction, required) {
			t.Fatalf("manual draft instruction is missing VSI routing constraint %q", required)
		}
	}
}

func TestManualDraftInstructionAllowsAllONTsOnKnownLT(t *testing.T) {
	for _, required := range []string{
		"list all ONTs on lt1",
		"selected LT is already the bounded scope",
		"GetOnlineOnt",
		"do not ask for an ONT name, AID",
	} {
		if !strings.Contains(manualDraftInstruction, required) {
			t.Fatalf("manual draft instruction is missing ONT collection guidance %q", required)
		}
	}
}

func TestBuiltinSkillContainsAccessConsoleSourceMap(t *testing.T) {
	for _, required := range []string{
		"Access Console source map and request lookup",
		"server/internal/routers/nbi/ext.go",
		"server/internal/pkgs/template_fs/template",
		"template/onu/service/getServiceRB_revert.tpl",
		"user-side VSI",
		"GetOnlineOnt",
		"bbf-xponift:channel-termination",
		"Access Console business-log evidence",
		"collect_access_console_logs",
		"Raw archives, passwords, and tokens must never enter the model context",
	} {
		if !strings.Contains(builtinSkillMarkdown, required) {
			t.Fatalf("builtin skill is missing source-backed request guidance %q", required)
		}
	}
}

func TestAccessConsoleLogWorkflowIsExplicitAndBounded(t *testing.T) {
	for _, required := range []string{
		"collect_access_console_logs",
		"GET /nms/v1/log/download",
		"bounded redacted excerpts",
		"not files read directly from the OLT",
	} {
		if !strings.Contains(systemInstruction, required) {
			t.Fatalf("system instruction is missing Access Console log constraint %q", required)
		}
	}
	for _, required := range []string{
		"Access Console business-log download",
		"server/internal/routers/inner/v1/log/routers.go",
		"server/internal/pkgs/log/logger.go",
	} {
		if !strings.Contains(projectOrientation, required) {
			t.Fatalf("project orientation is missing Access Console log route %q", required)
		}
	}
}

func TestRESTLookupUsesFocusedAPIDocBeforeSource(t *testing.T) {
	for _, required := range []string{
		"consult the preferred Access Console REST API document first",
		"pattern REST_API_Doc_V0618.md",
		"never read the entire document",
		"Before a POST, PUT, PATCH, or DELETE, additionally inspect the focused registered router/controller",
	} {
		if !strings.Contains(systemInstruction, required) {
			t.Fatalf("system instruction is missing API-doc-first rule %q", required)
		}
	}
	for _, required := range []string{
		"first lookup for an unverified REST request",
		"Never read the whole 277 KB document",
		"unverified REST route or operator intent -> focused REST_API_Doc_V0618.md search",
	} {
		if !strings.Contains(projectOrientation, required) {
			t.Fatalf("project orientation is missing API-doc fast path %q", required)
		}
	}
	for _, forbidden := range []string{
		"REST API contract (indexed reference, NOT a starting point)",
		"consult REST_API_Doc_V0618.md only if the router cannot be located",
	} {
		if strings.Contains(projectOrientation, forbidden) {
			t.Fatalf("project orientation still contains conflicting REST lookup rule %q", forbidden)
		}
	}
}

func TestModelIterationTrackerCountsModelGenerationsAndNotifiesOnToolCall(t *testing.T) {
	notified := int64(0)
	tracker := &modelIterationTracker{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		softThreshold:                2,
		onSoftThreshold: func(iteration int64) error {
			notified = iteration
			return nil
		},
	}
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID: "call-1",
		}},
	}}}

	if _, _, err := tracker.AfterModelRewriteState(context.Background(), state, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tracker.AfterModelRewriteState(context.Background(), state, nil); err != nil {
		t.Fatal(err)
	}
	if tracker.count() != 2 || notified != 2 {
		t.Fatalf("unexpected iteration state: count=%d notified=%d", tracker.count(), notified)
	}
}

func TestModelIterationTrackerDoesNotNotifyForFinalResponse(t *testing.T) {
	notified := false
	tracker := &modelIterationTracker{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		softThreshold:                1,
		onSoftThreshold: func(int64) error {
			notified = true
			return nil
		},
	}
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{{
		Role:    schema.Assistant,
		Content: "final diagnosis",
	}}}

	if _, _, err := tracker.AfterModelRewriteState(context.Background(), state, nil); err != nil {
		t.Fatal(err)
	}
	if notified {
		t.Fatal("final response must not emit a soft-threshold notification")
	}
}

func TestSoftConstraintMessageAllowsTargetedEvidenceCollection(t *testing.T) {
	message := softConstraintMessage(softStopIterationThreshold)
	for _, expected := range []string{"24", "exact unresolved evidence gap", "you may continue", "Do not repeat"} {
		if !strings.Contains(message.Content, expected) {
			t.Fatalf("soft constraint missing %q: %s", expected, message.Content)
		}
	}
}

func TestMergeSystemMessagesProducesOneLeadingSystemMessage(t *testing.T) {
	merged := mergeSystemMessages(
		schema.SystemMessage("main instruction"),
		schema.SystemMessage("<diagnostic-memory>goal</diagnostic-memory>"),
		softConstraintMessage(softStopIterationThreshold),
	)

	if merged == nil || merged.Role != schema.System {
		t.Fatalf("expected one system message, got %+v", merged)
	}
	for _, expected := range []string{"main instruction", "<diagnostic-memory>goal</diagnostic-memory>", "24 model iterations"} {
		if !strings.Contains(merged.Content, expected) {
			t.Fatalf("merged system message is missing %q: %s", expected, merged.Content)
		}
	}
}

func TestMergeSystemMessagesSkipsEmptySections(t *testing.T) {
	merged := mergeSystemMessages(nil, schema.SystemMessage("  "), schema.SystemMessage("instruction"))
	if merged == nil || merged.Content != "instruction" {
		t.Fatalf("unexpected merged system message: %+v", merged)
	}
	if mergeSystemMessages(nil, schema.SystemMessage("")) != nil {
		t.Fatal("empty system sections must not produce a message")
	}
}

func TestModelMessagesWithLeadingSystemDropsRetainedSystemCopies(t *testing.T) {
	messages := modelMessagesWithLeadingSystem(
		[]*schema.Message{
			schema.SystemMessage("current instruction"),
			schema.SystemMessage("current diagnostic memory"),
			softConstraintMessage(softStopIterationThreshold),
		},
		[]*schema.Message{
			schema.SystemMessage("stale checkpoint instruction"),
			schema.UserMessage("current request"),
			{Role: schema.Assistant, Content: "previous answer"},
		},
	)

	if len(messages) != 3 {
		t.Fatalf("expected one system and two history messages, got %d", len(messages))
	}
	if messages[0].Role != schema.System || messages[1].Role != schema.User || messages[2].Role != schema.Assistant {
		t.Fatalf("unexpected provider role sequence: %s, %s, %s", messages[0].Role, messages[1].Role, messages[2].Role)
	}
	if strings.Contains(messages[0].Content, "stale checkpoint") {
		t.Fatalf("stale system copy reached provider payload: %s", messages[0].Content)
	}
	for _, expected := range []string{"current instruction", "current diagnostic memory", "24 model iterations"} {
		if !strings.Contains(messages[0].Content, expected) {
			t.Fatalf("leading system message is missing %q", expected)
		}
	}
}

func TestRetainModelMessagesOmitsRegeneratedSystemContent(t *testing.T) {
	retained := retainModelMessages([]*schema.Message{
		schema.SystemMessage(strings.Repeat("instruction", 100)),
		schema.UserMessage("request"),
		{Role: schema.Assistant, Content: "answer"},
	})

	if len(retained) != 2 {
		t.Fatalf("expected only conversation history, got %d messages", len(retained))
	}
	for _, message := range retained {
		if message.Role == schema.System {
			t.Fatal("checkpoint must not retain regenerated system messages")
		}
	}
}

type contextSummaryModelMock struct {
	response *schema.Message
	err      error
}

type streamingModelMock struct {
	chunks []*schema.Message
}

type flakyTeamModelMock struct {
	response  *schema.Message
	err       error
	failUntil int
	calls     int
}

type recordingTeamModelMock struct {
	response *schema.Message
	messages [][]*schema.Message
}

func (m *recordingTeamModelMock) Generate(_ context.Context, messages []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.messages = append(m.messages, messages)
	return m.response, nil
}

func (m *recordingTeamModelMock) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("stream is not used by direct team generation")
}

func (m *recordingTeamModelMock) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type fixedErrorReader struct {
	err error
}

type closeErrorReadCloser struct {
	io.Reader
	err error
}

func (r closeErrorReadCloser) Close() error {
	return r.err
}

func (r fixedErrorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func tracedOpenAITestModel(t *testing.T, transport http.RoundTripper, records *[]map[string]any) *tracedChatModel {
	t.Helper()
	base, err := einoopenai.NewChatModel(context.Background(), &einoopenai.ChatModelConfig{
		APIKey:     "api-secret-that-must-not-be-recorded",
		BaseURL:    "https://provider.example/v1",
		Model:      "trace-test-model",
		HTTPClient: &http.Client{Transport: modelTraceTransport{base: transport}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &tracedChatModel{
		base:      base,
		modelName: "trace-test-model",
		recordTrace: func(_ context.Context, _ string, payload map[string]any) {
			*records = append(*records, payload)
		},
	}
}

func modelTraceTestResponse(request *http.Request, statusCode int, body io.ReadCloser, contentLength int64) *http.Response {
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		StatusCode:    statusCode,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          body,
		ContentLength: contentLength,
		Request:       request,
	}
}

func invokeModelHTTPTraceHooks(request *http.Request, firstResponseByte bool) {
	trace := httptrace.ContextClientTrace(request.Context())
	if trace == nil {
		return
	}
	if trace.GotConn != nil {
		trace.GotConn(httptrace.GotConnInfo{Reused: true, WasIdle: true})
	}
	if trace.WroteRequest != nil {
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
	if firstResponseByte && trace.GotFirstResponseByte != nil {
		trace.GotFirstResponseByte()
	}
}

const validModelTraceResponse = `{"id":"chatcmpl-trace","object":"chat.completion","created":1,"model":"trace-test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func (m *flakyTeamModelMock) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	m.calls++
	if m.calls <= m.failUntil {
		return nil, m.err
	}
	return m.response, nil
}

func (m *flakyTeamModelMock) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("stream is not used by direct team generation")
}

func (m *flakyTeamModelMock) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

var _ model.BaseModel[*schema.Message] = (*contextSummaryModelMock)(nil)

func (m *contextSummaryModelMock) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return m.response, m.err
}

func (m *contextSummaryModelMock) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, m.err
}

func (m *contextSummaryModelMock) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *streamingModelMock) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("Generate is not used by the streaming test model")
}

func (m *streamingModelMock) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray(m.chunks), nil
}

func (m *streamingModelMock) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func TestTeamPlannerAndReviewerPublishReasoning(t *testing.T) {
	plannerReasoning := ""
	planner := LLMPlanner{
		Model: &contextSummaryModelMock{response: &schema.Message{
			Role:             schema.Assistant,
			Content:          `<think>先拆分独立数据源。</think>[{"id":"platform_state","goal":"读取平台状态","role":"platform"}]`,
			ReasoningContent: "先拆分独立数据源。",
		}},
		OnReasoning: func(_ context.Context, content string, _ int) error {
			plannerReasoning = content
			return nil
		},
	}
	steps, err := planner.Plan(context.Background(), "诊断 ONT")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].ID != "platform_state" || plannerReasoning != "先拆分独立数据源。" {
		t.Fatalf("planner reasoning or visible plan was not handled correctly: steps=%+v reasoning=%q", steps, plannerReasoning)
	}

	reviewerReasoning := ""
	reviewer := LLMReviewer{
		Model: &contextSummaryModelMock{response: &schema.Message{
			Role:    schema.Assistant,
			Content: "<think>核对证据充分性。</think>最终诊断",
		}},
		OnReasoning: func(_ context.Context, content string, _ int) error {
			reviewerReasoning = content
			return nil
		},
	}
	answer, err := reviewer.Review(context.Background(), "诊断 ONT", map[string]TeamStepResult{})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "最终诊断" || reviewerReasoning != "核对证据充分性。" {
		t.Fatalf("reviewer reasoning leaked into answer or was not published: answer=%q reasoning=%q", answer, reviewerReasoning)
	}
}

func TestTeamPlannerRetriesUnexpectedEOF(t *testing.T) {
	chatModel := &flakyTeamModelMock{
		response:  schema.AssistantMessage(`[{"id":"platform_state","goal":"读取平台状态","role":"platform"}]`, nil),
		err:       io.ErrUnexpectedEOF,
		failUntil: 1,
	}
	retries := 0
	planner := LLMPlanner{
		Model: chatModel,
		OnRetry: func(_ context.Context, attempt, maxAttempts int, retryErr error) error {
			retries++
			if attempt != 2 || maxAttempts != teamModelMaxAttempts || !errors.Is(retryErr, io.ErrUnexpectedEOF) {
				t.Fatalf("unexpected retry callback: attempt=%d max=%d err=%v", attempt, maxAttempts, retryErr)
			}
			return nil
		},
	}

	steps, err := planner.Plan(context.Background(), "诊断 ONT")
	if err != nil {
		t.Fatal(err)
	}
	if chatModel.calls != 2 || retries != 1 || len(steps) != 1 {
		t.Fatalf("calls=%d retries=%d steps=%+v", chatModel.calls, retries, steps)
	}
}

func TestTeamModelGenerateDoesNotRetryCancellation(t *testing.T) {
	chatModel := &flakyTeamModelMock{err: context.Canceled, failUntil: teamModelMaxAttempts}
	_, err := generateTeamModel(context.Background(), chatModel, []*schema.Message{schema.UserMessage("goal")}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if chatModel.calls != 1 {
		t.Fatalf("calls = %d, want 1", chatModel.calls)
	}
}

func TestTracedChatModelRecordsHTTPOutcomes(t *testing.T) {
	tests := []struct {
		name           string
		roundTrip      roundTripFunc
		wantError      bool
		wantPhase      string
		wantErrorKind  string
		wantStatus     int
		wantHeaders    bool
		wantFirstByte  bool
		wantBodyBytes  bool
		wantContent    string
		wantRetryAfter string
	}{
		{
			name: "success",
			roundTrip: func(request *http.Request) (*http.Response, error) {
				invokeModelHTTPTraceHooks(request, true)
				return modelTraceTestResponse(request, http.StatusOK, io.NopCloser(strings.NewReader(validModelTraceResponse)), int64(len(validModelTraceResponse))), nil
			},
			wantPhase:     "complete",
			wantErrorKind: "none",
			wantStatus:    http.StatusOK,
			wantHeaders:   true,
			wantFirstByte: true,
			wantBodyBytes: true,
			wantContent:   "ok",
		},
		{
			name: "valid response with body close anomaly",
			roundTrip: func(request *http.Request) (*http.Response, error) {
				invokeModelHTTPTraceHooks(request, true)
				body := closeErrorReadCloser{Reader: strings.NewReader(validModelTraceResponse), err: io.ErrClosedPipe}
				return modelTraceTestResponse(request, http.StatusOK, body, int64(len(validModelTraceResponse))), nil
			},
			wantPhase:     "response_body",
			wantErrorKind: "closed_pipe",
			wantStatus:    http.StatusOK,
			wantHeaders:   true,
			wantFirstByte: true,
			wantBodyBytes: true,
			wantContent:   "ok",
		},
		{
			name: "EOF before response headers",
			roundTrip: func(request *http.Request) (*http.Response, error) {
				invokeModelHTTPTraceHooks(request, false)
				return nil, io.EOF
			},
			wantError:     true,
			wantPhase:     "before_headers",
			wantErrorKind: "eof",
		},
		{
			name: "empty successful response body",
			roundTrip: func(request *http.Request) (*http.Response, error) {
				invokeModelHTTPTraceHooks(request, true)
				return modelTraceTestResponse(request, http.StatusOK, io.NopCloser(strings.NewReader("")), 0), nil
			},
			wantError:     true,
			wantPhase:     "response_decode",
			wantErrorKind: "empty_body",
			wantStatus:    http.StatusOK,
			wantHeaders:   true,
			wantFirstByte: true,
		},
		{
			name: "invalid JSON response body",
			roundTrip: func(request *http.Request) (*http.Response, error) {
				invokeModelHTTPTraceHooks(request, true)
				const body = "not-json-secret-body"
				return modelTraceTestResponse(request, http.StatusOK, io.NopCloser(strings.NewReader(body)), int64(len(body))), nil
			},
			wantError:     true,
			wantPhase:     "response_decode",
			wantErrorKind: "invalid_json",
			wantStatus:    http.StatusOK,
			wantHeaders:   true,
			wantFirstByte: true,
			wantBodyBytes: true,
		},
		{
			name: "truncated response body",
			roundTrip: func(request *http.Request) (*http.Response, error) {
				invokeModelHTTPTraceHooks(request, true)
				body := io.NopCloser(io.MultiReader(
					strings.NewReader(`{"choices":[`),
					fixedErrorReader{err: io.ErrUnexpectedEOF},
				))
				return modelTraceTestResponse(request, http.StatusOK, body, 100), nil
			},
			wantError:     true,
			wantPhase:     "response_body",
			wantErrorKind: "unexpected_eof",
			wantStatus:    http.StatusOK,
			wantHeaders:   true,
			wantFirstByte: true,
			wantBodyBytes: true,
		},
		{
			name: "HTTP 429",
			roundTrip: func(request *http.Request) (*http.Response, error) {
				invokeModelHTTPTraceHooks(request, true)
				const body = `{"error":{"message":"provider-secret-message","type":"rate_limit_error","code":"rate_limit"}}`
				response := modelTraceTestResponse(request, http.StatusTooManyRequests, io.NopCloser(strings.NewReader(body)), int64(len(body)))
				response.Header.Set("Retry-After", "2")
				return response, nil
			},
			wantError:      true,
			wantPhase:      "http_status",
			wantErrorKind:  "http_status",
			wantStatus:     http.StatusTooManyRequests,
			wantHeaders:    true,
			wantFirstByte:  true,
			wantBodyBytes:  true,
			wantRetryAfter: "2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			records := make([]map[string]any, 0, 1)
			chatModel := tracedOpenAITestModel(t, test.roundTrip, &records)
			response, err := chatModel.Generate(
				withModelTraceRun(context.Background(), "run-trace", "planner"),
				[]*schema.Message{schema.UserMessage("Token: user-secret-that-must-not-be-recorded")},
			)
			if test.wantError != (err != nil) {
				t.Fatalf("Generate error = %v, wantError=%v", err, test.wantError)
			}
			if test.wantContent != "" && (response == nil || response.Content != test.wantContent) {
				t.Fatalf("response = %+v, want content %q", response, test.wantContent)
			}
			if len(records) != 1 {
				t.Fatalf("trace records = %d, want 1", len(records))
			}
			trace := records[0]
			if trace["failurePhase"] != test.wantPhase || trace["errorKind"] != test.wantErrorKind {
				t.Fatalf("trace classification = phase:%v kind:%v, want phase:%s kind:%s", trace["failurePhase"], trace["errorKind"], test.wantPhase, test.wantErrorKind)
			}
			if trace["statusCode"] != test.wantStatus || trace["headersReceived"] != test.wantHeaders || trace["gotFirstResponseByte"] != test.wantFirstByte {
				t.Fatalf("trace HTTP state = status:%v headers:%v firstByte:%v", trace["statusCode"], trace["headersReceived"], trace["gotFirstResponseByte"])
			}
			bodyBytes, ok := trace["responseBytes"].(int64)
			if !ok || test.wantBodyBytes != (bodyBytes > 0) {
				t.Fatalf("trace responseBytes = %#v, want positive=%v", trace["responseBytes"], test.wantBodyBytes)
			}
			if trace["host"] != "provider.example" || trace["path"] != "/chat/completions" || trace["method"] != http.MethodPost {
				t.Fatalf("trace request target was not recorded safely: %+v", trace)
			}
			if requestBytes, ok := trace["requestBytes"].(int64); !ok || requestBytes <= 0 {
				t.Fatalf("trace requestBytes = %#v, want positive int64", trace["requestBytes"])
			}
			if trace["stage"] != "planner" || trace["model"] != "trace-test-model" || trace["messageCount"] != 1 {
				t.Fatalf("trace model scope is incomplete: %+v", trace)
			}
			if test.wantRetryAfter != "" && trace["retryAfter"] != test.wantRetryAfter {
				t.Fatalf("trace retryAfter = %v, want %q", trace["retryAfter"], test.wantRetryAfter)
			}
		})
	}
}

func TestModelTraceURLMetadataDropsCredentialsAndQuery(t *testing.T) {
	target, err := url.Parse("https://user:password@example.test/private/token%3Dsecret/v1/chat/completions?api_key=query-secret")
	if err != nil {
		t.Fatal(err)
	}
	safePath := safeModelTracePath(target)
	safeError := safeModelTraceError(&url.Error{Op: http.MethodPost, URL: target.String(), Err: errors.New("connection reset")})
	combined := safePath + " " + safeError
	for _, secret := range []string{"user", "password", "private", "secret", "api_key", "query-secret"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("safe model URL metadata leaked %q: %s", secret, combined)
		}
	}
	if safePath != "/chat/completions" || !strings.Contains(safeError, "https://example.test/chat/completions") {
		t.Fatalf("safe URL metadata lost the useful endpoint identity: path=%q error=%q", safePath, safeError)
	}
}

func TestModelHTTPTraceDoesNotPersistSecrets(t *testing.T) {
	records := make([]map[string]any, 0, 1)
	chatModel := tracedOpenAITestModel(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		invokeModelHTTPTraceHooks(request, true)
		const body = "response-secret-body"
		return modelTraceTestResponse(request, http.StatusOK, io.NopCloser(strings.NewReader(body)), int64(len(body))), nil
	}), &records)

	_, _ = chatModel.Generate(
		withModelTraceRun(context.Background(), "run-trace", "agent"),
		[]*schema.Message{schema.UserMessage("password=user-secret-prompt")},
	)
	if len(records) != 1 {
		t.Fatalf("trace records = %d, want 1", len(records))
	}
	encoded, err := json.Marshal(records[0])
	if err != nil {
		t.Fatal(err)
	}
	traceJSON := string(encoded)
	for _, secret := range []string{
		"api-secret-that-must-not-be-recorded",
		"user-secret-prompt",
		"response-secret-body",
	} {
		if strings.Contains(traceJSON, secret) {
			t.Fatalf("model HTTP trace leaked %q: %s", secret, traceJSON)
		}
	}
	if !strings.Contains(traceJSON, `"errorKind":"invalid_json"`) {
		t.Fatalf("trace lost the safe error classification: %s", traceJSON)
	}
}

func TestTracedChatModelWithToolsPreservesTracing(t *testing.T) {
	records := make([]map[string]any, 0, 1)
	chatModel := &tracedChatModel{
		base:      &contextSummaryModelMock{response: schema.AssistantMessage("done", nil)},
		modelName: "wrapped-model",
		recordTrace: func(_ context.Context, _ string, payload map[string]any) {
			records = append(records, payload)
		},
	}
	bound, err := chatModel.WithTools([]*schema.ToolInfo{{Name: "read_state"}, {Name: "read_source"}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := bound.Generate(withModelTraceRun(context.Background(), "run-tools", "worker"), []*schema.Message{schema.UserMessage("inspect")})
	if err != nil || response == nil || response.Content != "done" {
		t.Fatalf("bound model Generate = response:%+v err:%v", response, err)
	}
	if len(records) != 1 || records[0]["toolCount"] != 2 || records[0]["stage"] != "worker" || records[0]["failurePhase"] != "complete" {
		t.Fatalf("WithTools did not preserve trace metadata: %+v", records)
	}
}

func TestTracedChatModelRecordsStreamAfterItIsConsumed(t *testing.T) {
	records := make(chan map[string]any, 1)
	chatModel := &tracedChatModel{
		base: &streamingModelMock{chunks: []*schema.Message{
			schema.AssistantMessage("first ", nil),
			schema.AssistantMessage("second", nil),
		}},
		modelName: "stream-model",
		recordTrace: func(_ context.Context, _ string, payload map[string]any) {
			records <- payload
		},
	}
	stream, err := chatModel.Stream(
		withModelTraceRun(context.Background(), "run-stream", "agent"),
		[]*schema.Message{schema.UserMessage("inspect")},
	)
	if err != nil {
		t.Fatal(err)
	}
	message, err := schema.ConcatMessageStream(stream)
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "first second" {
		t.Fatalf("stream content = %q, want %q", message.Content, "first second")
	}
	select {
	case record := <-records:
		if record["stream"] != true || record["stage"] != "agent" || record["failurePhase"] != "complete" {
			t.Fatalf("stream trace metadata is incomplete: %+v", record)
		}
		if bytes, _ := record["responseMessageBytes"].(int); bytes <= 0 {
			t.Fatalf("stream trace response bytes = %v, want > 0", record["responseMessageBytes"])
		}
	case <-time.After(time.Second):
		t.Fatal("stream trace was not recorded after consumption")
	}
}

func TestGenerateTeamModelGroupsExplicitRetryAttempts(t *testing.T) {
	records := make([]map[string]any, 0, 2)
	chatModel := &tracedChatModel{
		base: &flakyTeamModelMock{
			response:  schema.AssistantMessage("done", nil),
			err:       io.EOF,
			failUntil: 1,
		},
		modelName: "retry-model",
		recordTrace: func(_ context.Context, _ string, payload map[string]any) {
			records = append(records, payload)
		},
	}
	response, err := generateTeamModel(
		withModelTraceRun(context.Background(), "run-retry", "planner"),
		chatModel,
		[]*schema.Message{schema.UserMessage("continue")},
		nil,
	)
	if err != nil || response == nil || response.Content != "done" {
		t.Fatalf("generateTeamModel = response:%+v err:%v", response, err)
	}
	if len(records) != 2 {
		t.Fatalf("trace records = %d, want 2", len(records))
	}
	logicalCallID, _ := records[0]["logicalCallId"].(string)
	if logicalCallID == "" || records[1]["logicalCallId"] != logicalCallID {
		t.Fatalf("retry traces were not grouped: %+v", records)
	}
	for index, record := range records {
		want := index + 1
		if record["attempt"] != want || record["sequence"] != int64(want) {
			t.Fatalf("trace %d attempt/sequence = %v/%v, want %d/%d", index, record["attempt"], record["sequence"], want, want)
		}
	}
}

func TestPlannerHistoryPreservesConversationOrderAndFiltersProtocolMessages(t *testing.T) {
	chatModel := &recordingTeamModelMock{response: schema.AssistantMessage(
		`[{"id":"source_ntp","goal":"继续检查 NTP 参数映射","role":"source","sourceSearch":{"exactTerms":["association-type"],"ownerPaths":["server/internal"],"maxSearches":1}}]`,
		nil,
	)}
	planner := LLMPlanner{
		Model:  chatModel,
		Memory: schema.SystemMessage("verified memory: original NTP request is unfinished"),
		History: []*schema.Message{
			schema.SystemMessage("stale system instruction"),
			schema.UserMessage("original NTP request"),
			{
				Role:    schema.Assistant,
				Content: "calling a tool",
				ToolCalls: []schema.ToolCall{{
					ID:       "call-1",
					Function: schema.FunctionCall{Name: "search_files", Arguments: `{}`},
				}},
			},
			{Role: schema.Tool, ToolCallID: "call-1", Content: "large tool result that must stay out"},
			schema.AssistantMessage("previous evidence-backed answer", nil),
			schema.UserMessage("please continue that investigation"),
			schema.AssistantMessage("   ", nil),
		},
	}

	steps, err := planner.Plan(context.Background(), "go on")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || len(chatModel.messages) != 1 {
		t.Fatalf("planner result/calls = steps:%+v calls:%d", steps, len(chatModel.messages))
	}
	messages := chatModel.messages[0]
	wantRoles := []schema.RoleType{schema.System, schema.User, schema.Assistant, schema.User, schema.User}
	wantContents := []string{
		"",
		"original NTP request",
		"previous evidence-backed answer",
		"please continue that investigation",
		"go on",
	}
	if len(messages) != len(wantRoles) {
		t.Fatalf("planner messages = %+v, want %d filtered messages", messages, len(wantRoles))
	}
	for index := range wantRoles {
		if messages[index].Role != wantRoles[index] {
			t.Fatalf("planner message %d role = %s, want %s", index, messages[index].Role, wantRoles[index])
		}
		if index > 0 && messages[index].Content != wantContents[index] {
			t.Fatalf("planner message %d content = %q, want %q", index, messages[index].Content, wantContents[index])
		}
	}
	if !strings.Contains(messages[0].Content, teamPlannerPrompt) || !strings.Contains(messages[0].Content, "verified memory") {
		t.Fatalf("planner system message did not merge prompt and memory: %q", messages[0].Content)
	}
	for _, forbidden := range []string{"stale system instruction", "calling a tool", "large tool result"} {
		for _, message := range messages {
			if strings.Contains(message.Content, forbidden) {
				t.Fatalf("planner history retained filtered content %q: %+v", forbidden, messages)
			}
		}
	}
}

func validDiagnosticSummaryJSON() string {
	return `{"goal":"检查 ONU 状态","target":"onu-1","confirmed_facts":["在线"],"observed_errors":[],"rejected_hypotheses":[],"completed_checks":["ping"],"open_questions":[],"next_action":"继续检查","do_not_repeat":[]}`
}

func TestValidateDiagnosticSummaryStrictJSON(t *testing.T) {
	tests := []struct {
		name    string
		content string
		valid   bool
	}{
		{name: "合法", content: validDiagnosticSummaryJSON(), valid: true},
		{name: "缺字段", content: `{"goal":"g","target":"t","confirmed_facts":[],"observed_errors":[],"rejected_hypotheses":[],"completed_checks":[],"open_questions":[],"next_action":"a"}`},
		{name: "未知字段", content: `{"goal":"g","target":"t","confirmed_facts":[],"observed_errors":[],"rejected_hypotheses":[],"completed_checks":[],"open_questions":[],"next_action":"a","do_not_repeat":[],"extra":"nope"}`},
		{name: "尾随 JSON", content: validDiagnosticSummaryJSON() + ` {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := validateDiagnosticSummary(test.content)
			if test.valid {
				if err != nil || got == "" {
					t.Fatalf("expected valid summary, got %q, %v", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected summary validation failure, got %q", got)
			}
		})
	}
}

func TestContextManagerSummaryModelFailurePreservesStateAndCallsOnFailure(t *testing.T) {
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{schema.UserMessage("原始消息")}}
	modelErr := errors.New("summary model failed")
	failureCalled := false
	manager := &contextManager{
		model: &contextSummaryModelMock{err: modelErr}, memory: func() (*schema.Message, error) { return nil, nil },
		onFailure: func(err error) { failureCalled = errors.Is(err, modelErr) }, triggerMessages: 1, minNewMessages: 1,
	}
	_, got, err := manager.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != state || len(got.Messages) != 1 || got.Messages[0].Content != "原始消息" {
		t.Fatalf("state was not returned unchanged: got=%+v", got)
	}
	if !failureCalled {
		t.Fatal("expected onFailure to be called with model error")
	}
}

func TestContextManagerValidSummaryIncludesSummaryAndRecentMessages(t *testing.T) {
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{schema.UserMessage("用户请求"), {Role: schema.Assistant, Content: "最近证据"}}}
	manager := &contextManager{
		model: &contextSummaryModelMock{response: schema.AssistantMessage(validDiagnosticSummaryJSON(), nil)}, memory: func() (*schema.Message, error) { return nil, nil },
		triggerMessages: 1, minNewMessages: 1,
	}
	_, got, err := manager.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("expected summary plus two recent messages, got %d", len(got.Messages))
	}
	if !strings.Contains(got.Messages[0].Content, `"goal":"检查 ONU 状态"`) || !strings.Contains(got.Messages[2].Content, "最近证据") {
		t.Fatalf("unexpected compacted message layout: %+v", got.Messages)
	}
}

func TestTeamPassesDependencyResultsToDownstreamWorker(t *testing.T) {
	team := Team{
		Planner: FuncPlanner(func(context.Context, string) ([]TeamStep, error) {
			return []TeamStep{
				{ID: "device", Goal: "read device state", Role: "device"},
				{ID: "correlate", Goal: "correlate evidence", Role: "correlator", DependsOn: []string{"device"}},
			}, nil
		}),
		Worker: FuncWorker(func(_ context.Context, input TeamWorkInput) (TeamStepResult, error) {
			if input.Step.ID == "device" {
				return TeamStepResult{Status: teamStepSuccess, Summary: "ONU is up", EvidenceIDs: []string{"ev-1"}}, nil
			}
			dependency, ok := input.Dependencies["device"]
			if !ok || dependency.Summary != "ONU is up" || len(dependency.EvidenceIDs) != 1 || dependency.EvidenceIDs[0] != "ev-1" {
				t.Fatalf("downstream worker received wrong dependencies: %+v", input.Dependencies)
			}
			return TeamStepResult{Status: teamStepSuccess, Summary: "correlated"}, nil
		}),
		Reviewer: FuncReviewer(func(_ context.Context, _ string, outputs map[string]TeamStepResult) (string, error) {
			if outputs["device"].Status != teamStepSuccess || outputs["correlate"].Status != teamStepSuccess {
				t.Fatalf("reviewer received incomplete outputs: %+v", outputs)
			}
			if outputs["device"].Goal != "read device state" || outputs["correlate"].Goal != "correlate evidence" {
				t.Fatalf("reviewer did not receive host-populated step goals: %+v", outputs)
			}
			return "done", nil
		}),
		MaxParallel: 4,
	}

	result, err := team.Run(context.Background(), "diagnose")
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "done" {
		t.Fatalf("answer = %q, want done", result.Answer)
	}
}

func TestTeamSkipsDownstreamStepAfterDependencyFailure(t *testing.T) {
	downstreamCalled := false
	team := Team{
		Planner: FuncPlanner(func(context.Context, string) ([]TeamStep, error) {
			return []TeamStep{
				{ID: "device", Goal: "read device state", Role: "device"},
				{ID: "correlate", Goal: "correlate evidence", Role: "correlator", DependsOn: []string{"device"}},
			}, nil
		}),
		Worker: FuncWorker(func(_ context.Context, input TeamWorkInput) (TeamStepResult, error) {
			if input.Step.ID == "device" {
				return TeamStepResult{Summary: "device unreachable"}, errors.New("timeout")
			}
			downstreamCalled = true
			return TeamStepResult{Status: teamStepSuccess, Summary: "unexpected"}, nil
		}),
		Reviewer: FuncReviewer(func(_ context.Context, _ string, outputs map[string]TeamStepResult) (string, error) {
			if outputs["device"].Status != teamStepFailed {
				t.Fatalf("failed dependency not preserved: %+v", outputs["device"])
			}
			if outputs["correlate"].Status != teamStepSkipped || outputs["correlate"].SkipFrom != "device" {
				t.Fatalf("downstream skip not preserved: %+v", outputs["correlate"])
			}
			return "partial conclusion", nil
		}),
	}

	if _, err := team.Run(context.Background(), "diagnose"); err != nil {
		t.Fatal(err)
	}
	if downstreamCalled {
		t.Fatal("downstream worker ran after its dependency failed")
	}
}

func TestParseTeamStepsRejectsModelGrantedRole(t *testing.T) {
	_, err := parseTeamSteps(`[{"id":"unsafe","goal":"change device state","role":"writer"}]`)
	if err == nil || !strings.Contains(err.Error(), "unsupported role") {
		t.Fatalf("expected unsupported role error, got %v", err)
	}
}

func TestWireTeamLocatorDependenciesOrdersDeviceWorkAfterPlatformLookup(t *testing.T) {
	steps := wireTeamLocatorDependencies("ALCLFE2F75E8 这个 ONT 为什么没上线", []TeamStep{
		{ID: "ont_state", Goal: "read live ONT state", Role: "device"},
		{ID: "source_def", Goal: "find field definitions", Role: "source"},
		{ID: "platform_locator", Goal: "resolve serial to LT and PON", Role: "platform"},
		{ID: "optical", Goal: "read optical state", Role: "device"},
	})
	for _, step := range steps {
		if step.Role == "device" && (len(step.DependsOn) != 1 || step.DependsOn[0] != "platform_locator") {
			t.Fatalf("device step %s was not wired behind locator: %+v", step.ID, step.DependsOn)
		}
		if step.Role == "source" && len(step.DependsOn) != 0 {
			t.Fatalf("independent source step was serialized: %+v", step.DependsOn)
		}
	}
}

func TestWireTeamLocatorDependenciesKeepsKnownLTParallel(t *testing.T) {
	steps := wireTeamLocatorDependencies("检查 lt2 上的 ALCLFE2F75E8", []TeamStep{
		{ID: "device", Goal: "read live state from lt2 for ALCLFE2F75E8", Role: "device"},
		{ID: "platform", Goal: "read platform config", Role: "platform"},
	})
	if len(steps[0].DependsOn) != 0 {
		t.Fatalf("known locator should not create an artificial dependency: %+v", steps[0].DependsOn)
	}
}

func TestWireTeamLocatorDependenciesUsesSelfContainedContinuationStep(t *testing.T) {
	steps := wireTeamLocatorDependencies("go on", []TeamStep{
		{ID: "device", Goal: "read live ONT state from lt2 for ALCLFE2F75E8", Role: "device"},
		{ID: "platform", Goal: "compare platform provisioning", Role: "platform"},
	})
	if len(steps[0].DependsOn) != 0 {
		t.Fatalf("self-contained continuation step already has an LT locator: %+v", steps[0].DependsOn)
	}
}

func TestTeamPlanMemoryGoalKeepsRequestAndResolvedScope(t *testing.T) {
	goal := teamPlanMemoryGoal("go on", []TeamStep{
		{ID: "source", Goal: "trace POST /northbound/olt/provisioning/ntp binding", Role: "source"},
		{ID: "yang", Goal: "verify association-type enum values", Role: "source"},
	})
	for _, expected := range []string{"latest user request: go on", "POST /northbound/olt/provisioning/ntp", "association-type"} {
		if !strings.Contains(goal, expected) {
			t.Fatalf("plan memory goal is missing %q: %s", expected, goal)
		}
	}
}

func TestFilterLegacyPendingGoalFacts(t *testing.T) {
	facts := filterLegacyPendingGoalFacts([]domain.ContextFact{
		{Key: "goal", Kind: "goal", Content: "go on"},
		{Key: "GOAL", Kind: "Goal", Content: "old request"},
		{Key: "ont-status", Kind: "device_state", Content: "notActivated"},
	})
	if len(facts) != 1 || facts[0].Key != "ont-status" {
		t.Fatalf("legacy pending goal facts were not isolated: %+v", facts)
	}
}

func TestParseTeamStepsNormalizesBoundedSourceSearchBrief(t *testing.T) {
	steps, err := parseTeamSteps(`[{
		"id":"source_def",
		"goal":"explain the observed notActivated state",
		"role":"source",
		"sourceSearch":{
			"exactTerms":[" notActivated ","notActivated"],
			"ownerPaths":["server\\internal\\routers\\nbi"],
			"maxSearches":99,
			"runtimeDerived":true
		}
	}]`)
	if err != nil {
		t.Fatal(err)
	}
	search := steps[0].SourceSearch
	if search == nil {
		t.Fatal("source search brief was not created")
	}
	if len(search.ExactTerms) != 1 || search.ExactTerms[0] != "notActivated" {
		t.Fatalf("exact terms were not normalized: %+v", search.ExactTerms)
	}
	if len(search.OwnerPaths) != 1 || search.OwnerPaths[0] != "server/internal/routers/nbi" {
		t.Fatalf("owner paths were not normalized: %+v", search.OwnerPaths)
	}
	if search.MaxSearches != teamSourceMaxSearch || !search.RuntimeDerived {
		t.Fatalf("source search bounds were not enforced: %+v", search)
	}
}

func TestParseTeamStepsRejectsSourceSearchPathTraversal(t *testing.T) {
	_, err := parseTeamSteps(`[{
		"id":"source_def",
		"goal":"read a definition",
		"role":"source",
		"sourceSearch":{"ownerPaths":["../outside"],"maxSearches":1}
	}]`)
	if err == nil || !strings.Contains(err.Error(), "workspace-relative") {
		t.Fatalf("expected unsafe owner path to be rejected, got %v", err)
	}
}

func TestWireTeamSourceDependenciesUsesLivePlatformResult(t *testing.T) {
	steps := wireTeamSourceDependencies([]TeamStep{
		{ID: "source_def", Goal: "explain returned status", Role: "source", SourceSearch: &TeamSourceSearch{RuntimeDerived: true, MaxSearches: 2}},
		{ID: "platform_state", Goal: "read platform state", Role: "platform"},
		{ID: "device_state", Goal: "read device state", Role: "device", DependsOn: []string{"platform_state"}},
	})
	if len(steps[0].DependsOn) != 1 || steps[0].DependsOn[0] != "platform_state" {
		t.Fatalf("runtime-derived source step did not wait for platform evidence: %+v", steps[0].DependsOn)
	}
}

func TestWireTeamSourceDependenciesDoesNotCreateCycle(t *testing.T) {
	steps := wireTeamSourceDependencies([]TeamStep{
		{ID: "source_def", Goal: "explain returned status", Role: "source", SourceSearch: &TeamSourceSearch{RuntimeDerived: true, MaxSearches: 2}},
		{ID: "platform_state", Goal: "read platform state", Role: "platform", DependsOn: []string{"source_def"}},
	})
	if len(steps[0].DependsOn) != 0 {
		t.Fatalf("source dependency wiring introduced a cycle: %+v", steps[0].DependsOn)
	}
}

func TestBuildTeamWorkerResultAcceptsNaturalLanguageAndHostEvidence(t *testing.T) {
	result, err := buildTeamWorkerResult(
		TeamWorkInput{Step: TeamStep{Role: "platform"}},
		"Observed: ONT is discovered but not activated [ev-platform].",
		[]domain.Evidence{
			{ID: "ev-platform", Kind: "nbi_get"},
			{ID: "ev-device", Kind: "read_ont_state"},
			{ID: "ev-platform", Kind: "nbi_get"},
		},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != teamStepSuccess {
		t.Fatalf("status = %q, want %q", result.Status, teamStepSuccess)
	}
	if result.Summary != "Observed: ONT is discovered but not activated [ev-platform]." {
		t.Fatalf("unexpected summary: %q", result.Summary)
	}
	if len(result.EvidenceIDs) != 2 || result.EvidenceIDs[0] != "ev-platform" || result.EvidenceIDs[1] != "ev-device" {
		t.Fatalf("host evidence IDs were not preserved and deduplicated: %+v", result.EvidenceIDs)
	}
}

func TestBuildTeamWorkerResultPreservesEvidenceAtIterationLimit(t *testing.T) {
	limitErr := errors.New("[NodeRunError] pre processor fail: exceeds max iterations")
	result, err := buildTeamWorkerResult(
		TeamWorkInput{Step: TeamStep{Role: "device"}},
		"",
		[]domain.Evidence{{ID: "ev-1", Kind: "read_ont_state"}},
		[]string{`{"evidenceId":"ev-1","summary":"NBI returned HTTP 200","data":{"status":"discovered"}}`},
		nil,
		limitErr,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != teamStepPartial || len(result.EvidenceIDs) != 1 {
		t.Fatalf("bounded partial result did not preserve evidence: %+v", result)
	}
	if !strings.Contains(result.Summary, "status") || !strings.Contains(result.Error, "max iterations") {
		t.Fatalf("partial result lost its evidence snapshot or stop reason: %+v", result)
	}
}

func TestBuildTeamWorkerResultFailsWithoutReportOrEvidence(t *testing.T) {
	_, err := buildTeamWorkerResult(TeamWorkInput{}, "", nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "without evidence") {
		t.Fatalf("expected empty worker result to fail, got %v", err)
	}
}

func TestBuildTeamWorkerResultAcceptsReferencedDependencyEvidence(t *testing.T) {
	const evidenceID = "93da4951-a234-4ef0-9d92-317c2ad2949c"
	result, err := buildTeamWorkerResult(
		TeamWorkInput{
			Step: TeamStep{Role: "correlator"},
			Dependencies: map[string]TeamStepResult{
				"device": {EvidenceIDs: []string{evidenceID}},
			},
		},
		"The device observation is established by `93da4951`.",
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != teamStepSuccess {
		t.Fatalf("status = %q, want %q: %+v", result.Status, teamStepSuccess, result)
	}
	if len(result.ReferencedEvidenceIDs) != 1 || result.ReferencedEvidenceIDs[0] != evidenceID {
		t.Fatalf("validated dependency evidence was not recorded: %+v", result.ReferencedEvidenceIDs)
	}
	if len(result.EvidenceIDs) != 0 {
		t.Fatalf("dependency evidence was incorrectly counted as newly collected: %+v", result.EvidenceIDs)
	}
}

func TestBuildTeamWorkerResultRejectsUnregisteredEvidenceReference(t *testing.T) {
	result, err := buildTeamWorkerResult(
		TeamWorkInput{
			Step: TeamStep{Role: "correlator"},
			Dependencies: map[string]TeamStepResult{
				"device": {EvidenceIDs: []string{"93da4951-a234-4ef0-9d92-317c2ad2949c"}},
			},
		},
		"The conclusion is established by `deadbeef`.",
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != teamStepPartial || len(result.ReferencedEvidenceIDs) != 0 {
		t.Fatalf("unregistered evidence reference was accepted: %+v", result)
	}
}

func TestBuildTeamWorkerResultMarksToolErrorsPartial(t *testing.T) {
	result, err := buildTeamWorkerResult(
		TeamWorkInput{Step: TeamStep{Role: "source"}},
		"The handler validates the request before dispatch.",
		[]domain.Evidence{{ID: "ev-read", Kind: "read_file", Data: map[string]any{"content": "handler"}}},
		nil,
		[]string{"open file: path was not found"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != teamStepPartial {
		t.Fatalf("tool error did not produce a partial result: %+v", result)
	}
}

func TestBuildTeamWorkerResultMarksEmptySourceSearchPartial(t *testing.T) {
	result, err := buildTeamWorkerResult(
		TeamWorkInput{Step: TeamStep{Role: "source"}},
		"No matching definition was located.",
		[]domain.Evidence{{ID: "ev-search", Kind: "search_files", Data: map[string]any{"matches": []any{}}}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != teamStepPartial {
		t.Fatalf("empty repository search was treated as substantive evidence: %+v", result)
	}
}

func TestTeamToolBudgetEnforcesLimitAcrossConcurrentCalls(t *testing.T) {
	budget := &teamToolBudget{limit: 8}
	results := make(chan bool, 64)
	var workers sync.WaitGroup
	for index := 0; index < cap(results); index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			results <- budget.reserve()
		}()
	}
	workers.Wait()
	close(results)
	accepted := 0
	for result := range results {
		if result {
			accepted++
		}
	}
	if accepted != 8 || budget.used.Load() != 8 {
		t.Fatalf("accepted=%d used=%d, want exactly 8", accepted, budget.used.Load())
	}
}

func TestTeamToolBudgetCapsSourceSearchesSeparatelyFromReads(t *testing.T) {
	budget := &teamToolBudget{limit: 10, searchLimit: 4}
	results := make(chan bool, 32)
	var workers sync.WaitGroup
	for index := 0; index < cap(results); index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			accepted, _ := budget.reserveTool("search_code")
			results <- accepted
		}()
	}
	workers.Wait()
	close(results)
	acceptedSearches := 0
	for accepted := range results {
		if accepted {
			acceptedSearches++
		}
	}
	if acceptedSearches != 4 || budget.searchUsed.Load() != 4 {
		t.Fatalf("accepted searches=%d used=%d, want exactly 4", acceptedSearches, budget.searchUsed.Load())
	}
	acceptedReads := 0
	for index := 0; index < 7; index++ {
		if accepted, _ := budget.reserveTool("read_file"); accepted {
			acceptedReads++
		}
	}
	if acceptedReads != 6 || budget.used.Load() != 10 {
		t.Fatalf("accepted reads=%d total used=%d, want 6 reads and 10 total calls", acceptedReads, budget.used.Load())
	}
}

func TestConstrainTeamSourceToolInputBoundsResultFanoutAndDisablesRebuild(t *testing.T) {
	codeInput := &compose.ToolInput{Name: "search_code", Arguments: `{"query":"ONU activation","topK":20,"rebuild":true}`}
	constrainTeamSourceToolInput(codeInput)
	var codeArguments codeSearchArguments
	if err := json.Unmarshal([]byte(codeInput.Arguments), &codeArguments); err != nil {
		t.Fatal(err)
	}
	if codeArguments.TopK != 8 || codeArguments.Rebuild {
		t.Fatalf("semantic search was not bounded: %+v", codeArguments)
	}

	fileInput := &compose.ToolInput{Name: "search_files", Arguments: `{"query":"notActivated","maxResults":500}`}
	constrainTeamSourceToolInput(fileInput)
	var fileArguments fileSearchArguments
	if err := json.Unmarshal([]byte(fileInput.Arguments), &fileArguments); err != nil {
		t.Fatal(err)
	}
	if fileArguments.MaxResults != 50 {
		t.Fatalf("exact search result fanout = %d, want 50", fileArguments.MaxResults)
	}
}

func TestNormalizeDiagnosticModeDefaultsToAgent(t *testing.T) {
	mode, err := domain.NormalizeDiagnosticMode("")
	if err != nil {
		t.Fatal(err)
	}
	if mode != domain.DiagnosticModeAgent {
		t.Fatalf("mode = %q, want %q", mode, domain.DiagnosticModeAgent)
	}
}

func TestScheduleDAGConvertsWorkerPanicToFailureAndSkipsDependent(t *testing.T) {
	results, err := ScheduleDAG(context.Background(), []DAGStep{
		{ID: "panic_step", Work: func(context.Context) error { panic("boom") }},
		{ID: "dependent", DependsOn: []string{"panic_step"}, Work: func(context.Context) error {
			t.Fatal("dependent step should not run")
			return nil
		}},
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if results["panic_step"].Err == nil || !strings.Contains(results["panic_step"].Err.Error(), "panicked") {
		t.Fatalf("panic was not converted to a step failure: %+v", results["panic_step"])
	}
	if !results["dependent"].Skipped || results["dependent"].SkipFrom != "panic_step" {
		t.Fatalf("dependent step was not skipped: %+v", results["dependent"])
	}
}
