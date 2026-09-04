package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
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

type contextSummaryModelMock struct {
	response *schema.Message
	err      error
}

var _ model.BaseModel[*schema.Message] = (*contextSummaryModelMock)(nil)

func (m *contextSummaryModelMock) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return m.response, m.err
}

func (m *contextSummaryModelMock) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, m.err
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
		{ID: "device", Goal: "read live state", Role: "device"},
		{ID: "platform", Goal: "read platform config", Role: "platform"},
	})
	if len(steps[0].DependsOn) != 0 {
		t.Fatalf("known locator should not create an artificial dependency: %+v", steps[0].DependsOn)
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
