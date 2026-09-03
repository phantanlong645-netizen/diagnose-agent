package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"olt-diagnostic-agent/internal/domain"
)

func TestRedactModelText(t *testing.T) {
	input := `curl -H "Token: ${TOKEN}" --data '{"password":"secret-value"}' <token>jwt-value</token>`
	redacted := redactModelText(input)

	for _, value := range []string{"${TOKEN}", "secret-value", "jwt-value"} {
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
