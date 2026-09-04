package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"

	"olt-diagnostic-agent/internal/application"
	"olt-diagnostic-agent/internal/domain"
)

// 团队编排常量：步骤/轮次/报告长度等上限，以及步骤状态取值。
const (
	// teamMaxSteps 单次规划最多允许的取证步骤数。
	teamMaxSteps = 6
	// teamWorkerMaxRounds 单个 worker 的模型迭代硬上限。
	teamWorkerMaxRounds = 5
	// teamWorkerSoftStop 单个 worker 的软停阈值（达到后要求收敛并写报告）。
	teamWorkerSoftStop = 3
	// teamWorkerReportMax 最终报告摘要允许的最大字节数。
	teamWorkerReportMax = 12000
	// teamSourceMaxSearch source 步骤允许的最大搜索次数。
	teamSourceMaxSearch = 4

	// 步骤状态取值：成功 / 部分完成 / 失败 / 被跳过。
	teamStepSuccess = "success"
	teamStepPartial = "partial"
	teamStepFailed  = "failed"
	teamStepSkipped = "skipped"
)

// validTeamStepID 校验 Planner 生成的步骤 ID 格式（字母开头，最多 64 字符）。
var validTeamStepID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// explicitDeviceLocator 检测目标文本是否已含明确的设备定位信息（LT 编号/端口/ONT/PON）。
var explicitDeviceLocator = regexp.MustCompile(`(?i)(\blt\d+\b|:\d{3,5}\b|channel[-_ ]?termination|chterm|_ont\d+\b|\bpon[\s/_-]*\d+\b)`)

// TeamStep 是宿主校验过的取证单元。Role 决定固定的只读工具白名单，
// 模型永远不能给自己授予工具。
type TeamStep struct {
	ID           string            `json:"id"`
	Goal         string            `json:"goal"`
	Role         string            `json:"role"`
	DependsOn    []string          `json:"dependsOn,omitempty"`
	SourceSearch *TeamSourceSearch `json:"sourceSearch,omitempty"`
}

// TeamSourceSearch 是 Planner 生成、宿主校验过的受限搜索简报。
// 它收窄 source worker 的范围，但不会把它变成固定工具脚本：
// worker 仍自行决定用哪条权威定义确立分配的事实。
type TeamSourceSearch struct {
	ExactTerms     []string `json:"exactTerms,omitempty"`
	OwnerPaths     []string `json:"ownerPaths,omitempty"`
	MaxSearches    int      `json:"maxSearches,omitempty"`
	RuntimeDerived bool     `json:"runtimeDerived,omitempty"`
}

// TeamFact 是绑定到父 run 所捕获证据上的 worker 断言。
type TeamFact struct {
	Statement   string   `json:"statement"`
	EvidenceIDs []string `json:"evidenceIds,omitempty"`
	Confidence  float64  `json:"confidence,omitempty"`
}

// TeamStepResult is the only information shared between workers. Their chat
// histories remain isolated; large raw payloads stay in the Evidence store.
type TeamStepResult struct {
	StepID      string     `json:"stepId"`
	Role        string     `json:"role"`
	Status      string     `json:"status"`
	Summary     string     `json:"summary"`
	Facts       []TeamFact `json:"facts,omitempty"`
	EvidenceIDs []string   `json:"evidenceIds,omitempty"`
	Unknowns    []string   `json:"unknowns,omitempty"`
	Error       string     `json:"error,omitempty"`
	SkipFrom    string     `json:"skipFrom,omitempty"`
}

type TeamWorkInput struct {
	OriginalGoal string                    `json:"originalGoal"`
	Step         TeamStep                  `json:"step"`
	Dependencies map[string]TeamStepResult `json:"dependencies,omitempty"`
}

// TeamRunResult 是整支团队一次运行的汇总：最终答案、步骤清单与每步结果。
type TeamRunResult struct {
	Answer  string                    `json:"answer"`
	Steps   []TeamStep                `json:"steps"`
	Results map[string]TeamStepResult `json:"results"`
}

type TeamPlanner interface {
	Plan(ctx context.Context, goal string) ([]TeamStep, error)
}

type TeamWorker interface {
	Work(ctx context.Context, input TeamWorkInput) (TeamStepResult, error)
}

type TeamReviewer interface {
	Review(ctx context.Context, goal string, outputs map[string]TeamStepResult) (string, error)
}

type FuncPlanner func(ctx context.Context, goal string) ([]TeamStep, error)

func (f FuncPlanner) Plan(ctx context.Context, goal string) ([]TeamStep, error) { return f(ctx, goal) }

type FuncWorker func(ctx context.Context, input TeamWorkInput) (TeamStepResult, error)

func (f FuncWorker) Work(ctx context.Context, input TeamWorkInput) (TeamStepResult, error) {
	return f(ctx, input)
}

type FuncReviewer func(ctx context.Context, goal string, outputs map[string]TeamStepResult) (string, error)

func (f FuncReviewer) Review(ctx context.Context, goal string, outputs map[string]TeamStepResult) (string, error) {
	return f(ctx, goal, outputs)
}

// Team is a supervisor/worker orchestrator. Ready DAG nodes run concurrently;
// dependencies receive only structured results and evidence references.
type Team struct {
	Planner        TeamPlanner
	Worker         TeamWorker
	Reviewer       TeamReviewer
	MaxParallel    int
	OnPlanned      func([]TeamStep) error
	OnStepStarted  func(TeamStep) error
	OnStepFinished func(TeamStep, TeamStepResult) error
}

func (t *Team) Run(ctx context.Context, goal string) (TeamRunResult, error) {
	if t.Planner == nil || t.Worker == nil || t.Reviewer == nil {
		return TeamRunResult{}, errors.New("team requires planner, worker and reviewer")
	}
	steps, err := t.Planner.Plan(ctx, goal)
	if err != nil {
		return TeamRunResult{}, fmt.Errorf("plan goal: %w", err)
	}
	if t.OnPlanned != nil {
		if err = t.OnPlanned(steps); err != nil {
			return TeamRunResult{}, err
		}
	}

	maxParallel := t.MaxParallel
	if maxParallel <= 0 {
		maxParallel = 1
	}
	if maxParallel > 4 {
		maxParallel = 4
	}

	results := make(map[string]TeamStepResult, len(steps))
	var resultsMu sync.RWMutex
	dagSteps := make([]DAGStep, 0, len(steps))
	for _, plannedStep := range steps {
		step := plannedStep
		dagSteps = append(dagSteps, DAGStep{
			ID:        step.ID,
			DependsOn: append([]string(nil), step.DependsOn...),
			Work: func(ctx context.Context) error {
				if t.OnStepStarted != nil {
					if startErr := t.OnStepStarted(step); startErr != nil {
						return startErr
					}
				}

				dependencies := make(map[string]TeamStepResult, len(step.DependsOn))
				resultsMu.RLock()
				for _, dependencyID := range step.DependsOn {
					dependencies[dependencyID] = results[dependencyID]
				}
				resultsMu.RUnlock()

				result, workErr := t.Worker.Work(ctx, TeamWorkInput{
					OriginalGoal: goal,
					Step:         step,
					Dependencies: dependencies,
				})
				result = normalizeTeamStepResult(step, result, workErr)
				resultsMu.Lock()
				results[step.ID] = result
				resultsMu.Unlock()
				if t.OnStepFinished != nil {
					if finishErr := t.OnStepFinished(step, result); finishErr != nil {
						workErr = errors.Join(workErr, finishErr)
					}
				}
				return workErr
			},
		})
	}

	dagResults, err := ScheduleDAG(ctx, dagSteps, maxParallel)
	if err != nil {
		return TeamRunResult{}, err
	}
	for _, step := range steps {
		dagResult := dagResults[step.ID]
		resultsMu.RLock()
		_, resultExists := results[step.ID]
		resultsMu.RUnlock()
		if dagResult.Err != nil && !resultExists {
			result := TeamStepResult{
				StepID:  step.ID,
				Role:    step.Role,
				Status:  teamStepFailed,
				Summary: "Worker could not start or report its result.",
				Error:   dagResult.Err.Error(),
			}
			resultsMu.Lock()
			results[step.ID] = result
			resultsMu.Unlock()
			if t.OnStepFinished != nil {
				if err = t.OnStepFinished(step, result); err != nil {
					return TeamRunResult{}, err
				}
			}
			continue
		}
		if !dagResult.Skipped {
			continue
		}
		result := TeamStepResult{
			StepID:   step.ID,
			Role:     step.Role,
			Status:   teamStepSkipped,
			Summary:  fmt.Sprintf("Skipped because dependency %s failed.", dagResult.SkipFrom),
			SkipFrom: dagResult.SkipFrom,
		}
		resultsMu.Lock()
		results[step.ID] = result
		resultsMu.Unlock()
		if t.OnStepFinished != nil {
			if err = t.OnStepFinished(step, result); err != nil {
				return TeamRunResult{}, err
			}
		}
	}

	answer, err := t.Reviewer.Review(ctx, goal, results)
	if err != nil {
		return TeamRunResult{}, fmt.Errorf("review team results: %w", err)
	}
	return TeamRunResult{Answer: strings.TrimSpace(answer), Steps: steps, Results: results}, nil
}

func normalizeTeamStepResult(step TeamStep, result TeamStepResult, workErr error) TeamStepResult {
	result.StepID = step.ID
	result.Role = step.Role
	result.Summary = strings.TrimSpace(result.Summary)
	result.EvidenceIDs = uniqueStrings(result.EvidenceIDs)
	result.Unknowns = uniqueStrings(result.Unknowns)
	if workErr != nil {
		result.Status = teamStepFailed
		result.Error = workErr.Error()
		if result.Summary == "" {
			result.Summary = "Worker failed before completing the investigation."
		}
		return result
	}
	switch result.Status {
	case teamStepSuccess, teamStepPartial:
	default:
		result.Status = teamStepSuccess
	}
	if result.Summary == "" {
		result.Summary = "Worker completed without a summary."
	}
	return result
}

// LLMPlanner 用 ChatModel 把诊断目标规划成取证步骤 DAG。
type LLMPlanner struct {
	Model model.ToolCallingChatModel
}

// teamPlannerPrompt 是规划阶段发给模型的系统提示词（保持英文）。
const teamPlannerPrompt = `You are the Planner for a supervisor/worker OLT diagnostic system.
Create the smallest useful DAG of independent evidence investigations. Return ONLY a JSON array.
Each element must be: {"id":"short_id","goal":"one concrete investigation goal","role":"device|platform|source|web|correlator","dependsOn":["upstream_id"],"sourceSearch":{"exactTerms":["literal"],"ownerPaths":["repo/relative/path"],"maxSearches":1,"runtimeDerived":false}}.

Roles and host-enforced capabilities:
- device: live read-only NETCONF investigation.
- platform: read-only Access Console REST and focused business-log investigation.
- source: local code, YANG, route catalog, and design-document investigation.
- web: public vendor documentation, standards, release notes, and known issues.
- correlator: compare dependency evidence; it can only re-read evidence by ID.

Rules:
- Return 1 to 6 steps. Prefer 2 to 4 independent evidence steps.
- Add dependsOn only when a step needs an upstream result. Do not serialize independent sources.
- A per-ONT device query needs an exact device locator: LT endpoint plus the applicable ONT AID, PON/channel-termination, or another verified narrow key. When the goal only contains a serial/MAC and no locator, plan one platform inventory/locator step first and make dependent device checks consume its result. Never make device workers guess or independently probe every LT.
- Create a source step only to explain a specific observed status/error, discover an unknown route/RPC/field, or verify a concrete implementation mismatch. Do not ask it to inspect all YANG, code, and design documents for general background.
- sourceSearch is allowed only for source steps. Supply exact literals/symbols/routes and likely owner paths whenever they are already known. maxSearches must be between 1 and 4.
- When the source lookup term must come from a live platform/device response, set runtimeDerived=true and make the source step depend on that live step. Otherwise source investigation may run in parallel.
- Do not create a final-answer step; a separate Reviewer always synthesizes the result.
- Prefer read-only evidence. Never plan configuration changes or shell commands.
- Use web only when the requested fact cannot be established from the target or configured workspaces.
- Do not invent target state, paths, RPCs, identifiers, or evidence.`

func (p LLMPlanner) Plan(ctx context.Context, goal string) ([]TeamStep, error) {
	if p.Model == nil {
		return nil, errors.New("planner model is not configured")
	}
	response, err := p.Model.Generate(ctx, []*schema.Message{
		schema.SystemMessage(teamPlannerPrompt),
		schema.UserMessage(goal),
	})
	if err != nil {
		return nil, err
	}
	steps, err := parseTeamSteps(response.Content)
	if err != nil {
		return nil, fmt.Errorf("planner returned invalid plan: %w", err)
	}
	steps = wireTeamLocatorDependencies(goal, steps)
	return wireTeamSourceDependencies(steps), nil
}

// wireTeamLocatorDependencies provides a deterministic safety net around the
// model-authored DAG. A serial/MAC alone is not enough to issue a narrow
// per-ONT NETCONF query, so live device steps wait for the first independent
// platform step to resolve the locator. The Planner still decides whether a
// platform lookup is needed; the host owns the data dependency once it exists.
func wireTeamLocatorDependencies(goal string, steps []TeamStep) []TeamStep {
	if explicitDeviceLocator.MatchString(goal) {
		return steps
	}
	locatorID := ""
	for _, step := range steps {
		if step.Role == "platform" && len(step.DependsOn) == 0 {
			locatorID = step.ID
			break
		}
	}
	if locatorID == "" {
		return steps
	}
	for index := range steps {
		if steps[index].Role != "device" || steps[index].ID == locatorID {
			continue
		}
		if teamStepTransitivelyDependsOn(steps, locatorID, steps[index].ID) {
			continue
		}
		steps[index].DependsOn = uniqueStrings(append(steps[index].DependsOn, locatorID))
	}
	return steps
}

// wireTeamSourceDependencies makes runtime-derived code lookups consume a live
// result instead of searching for every possible status in parallel. Explicit
// Planner dependencies win; the host only fills a missing live dependency and
// refuses to introduce a cycle.
func wireTeamSourceDependencies(steps []TeamStep) []TeamStep {
	for index := range steps {
		step := &steps[index]
		if step.Role != "source" || step.SourceSearch == nil || !step.SourceSearch.RuntimeDerived {
			continue
		}
		hasLiveDependency := false
		for _, dependencyID := range step.DependsOn {
			for _, candidate := range steps {
				if candidate.ID == dependencyID && (candidate.Role == "platform" || candidate.Role == "device") {
					hasLiveDependency = true
					break
				}
			}
		}
		if hasLiveDependency {
			continue
		}
		for _, preferredRole := range []string{"platform", "device"} {
			for _, candidate := range steps {
				if candidate.Role != preferredRole || candidate.ID == step.ID || teamStepTransitivelyDependsOn(steps, candidate.ID, step.ID) {
					continue
				}
				step.DependsOn = uniqueStrings(append(step.DependsOn, candidate.ID))
				hasLiveDependency = true
				break
			}
			if hasLiveDependency {
				break
			}
		}
	}
	return steps
}

func teamStepTransitivelyDependsOn(steps []TeamStep, startID, targetID string) bool {
	dependencies := make(map[string][]string, len(steps))
	for _, step := range steps {
		dependencies[step.ID] = step.DependsOn
	}
	visited := make(map[string]bool, len(steps))
	var visit func(string) bool
	visit = func(stepID string) bool {
		if stepID == targetID {
			return true
		}
		if visited[stepID] {
			return false
		}
		visited[stepID] = true
		for _, dependencyID := range dependencies[stepID] {
			if visit(dependencyID) {
				return true
			}
		}
		return false
	}
	return visit(startID)
}

type LLMReviewer struct {
	Model model.ToolCallingChatModel
}

const teamReviewerPrompt = `You are the Reviewer for an OLT diagnostic supervisor/worker run.
Produce one evidence-grounded final answer in Simplified Chinese from the structured worker results.
Distinguish observed facts from inference. Cite evidence IDs next to important facts. Reconcile contradictions explicitly.
Cite each evidence ID using Markdown inline-code formatting so the UI can resolve it to the persisted evidence item.
The host-populated evidenceIds arrays are authoritative. Never treat an ID mentioned only inside a worker summary as valid evidence when it is absent from that worker's evidenceIds array.
If a worker is partial, failed, or skipped, state the exact remaining evidence gap without inventing a result.
Do not claim a diagnosis is certain when the available evidence only supports a hypothesis.`

// Review 调用模型汇总 worker 结果，产出证据支撑的最终答案。
func (r LLMReviewer) Review(ctx context.Context, goal string, outputs map[string]TeamStepResult) (string, error) {
	if r.Model == nil {
		return "", errors.New("reviewer model is not configured")
	}
	payload, err := json.Marshal(outputs)
	if err != nil {
		return "", fmt.Errorf("encode worker outputs for review: %w", err)
	}
	response, err := r.Model.Generate(ctx, []*schema.Message{
		schema.SystemMessage(teamReviewerPrompt),
		schema.UserMessage(fmt.Sprintf("goal: %s\nworker results:\n%s", goal, payload)),
	})
	if err != nil {
		return "", err
	}
	return response.Content, nil
}

func parseTeamSteps(content string) ([]TeamStep, error) {
	clean := cleanJSONBlock(content)
	var steps []TeamStep
	if err := json.Unmarshal([]byte(clean), &steps); err != nil {
		var wrapper struct {
			Steps []TeamStep `json:"steps"`
		}
		if wrapErr := json.Unmarshal([]byte(clean), &wrapper); wrapErr != nil {
			return nil, err
		}
		steps = wrapper.Steps
	}
	if len(steps) == 0 || len(steps) > teamMaxSteps {
		return nil, fmt.Errorf("plan must contain between 1 and %d steps", teamMaxSteps)
	}

	seen := make(map[string]bool, len(steps))
	for index := range steps {
		step := &steps[index]
		step.ID = strings.TrimSpace(step.ID)
		step.Goal = strings.TrimSpace(step.Goal)
		step.Role = strings.ToLower(strings.TrimSpace(step.Role))
		if !validTeamStepID.MatchString(step.ID) {
			return nil, fmt.Errorf("step %d has invalid id %q", index, step.ID)
		}
		if step.Goal == "" {
			return nil, fmt.Errorf("step %s is missing goal", step.ID)
		}
		if _, ok := teamRoleTools[step.Role]; !ok {
			return nil, fmt.Errorf("step %s has unsupported role %q", step.ID, step.Role)
		}
		if seen[step.ID] {
			return nil, fmt.Errorf("duplicate step id: %s", step.ID)
		}
		seen[step.ID] = true
		step.DependsOn = uniqueStrings(step.DependsOn)
		if err := normalizeTeamSourceSearch(step); err != nil {
			return nil, fmt.Errorf("step %s has invalid source search brief: %w", step.ID, err)
		}
	}
	for _, step := range steps {
		for _, dependencyID := range step.DependsOn {
			if dependencyID == step.ID {
				return nil, fmt.Errorf("step %s cannot depend on itself", step.ID)
			}
			if !seen[dependencyID] {
				return nil, fmt.Errorf("step %s depends on unknown step %s", step.ID, dependencyID)
			}
		}
	}
	return steps, nil
}

func normalizeTeamSourceSearch(step *TeamStep) error {
	if step.Role != "source" {
		step.SourceSearch = nil
		return nil
	}
	if step.SourceSearch == nil {
		step.SourceSearch = &TeamSourceSearch{}
	}
	search := step.SourceSearch
	search.ExactTerms = boundedTeamHints(search.ExactTerms, 6, 160)
	search.OwnerPaths = boundedTeamHints(search.OwnerPaths, 4, 240)
	for index, ownerPath := range search.OwnerPaths {
		ownerPath = filepath.ToSlash(strings.TrimSpace(ownerPath))
		cleaned := filepath.ToSlash(filepath.Clean(ownerPath))
		if filepath.IsAbs(ownerPath) || filepath.VolumeName(ownerPath) != "" || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return fmt.Errorf("owner path must be workspace-relative: %q", ownerPath)
		}
		search.OwnerPaths[index] = strings.TrimPrefix(cleaned, "./")
	}
	search.OwnerPaths = uniqueStrings(search.OwnerPaths)
	if search.MaxSearches < 1 || search.MaxSearches > teamSourceMaxSearch {
		search.MaxSearches = teamSourceMaxSearch
	}
	return nil
}

func boundedTeamHints(values []string, maxItems, maxRunes int) []string {
	values = uniqueStrings(values)
	if len(values) > maxItems {
		values = values[:maxItems]
	}
	for index, value := range values {
		runes := []rune(value)
		if len(runes) > maxRunes {
			values[index] = string(runes[:maxRunes])
		}
	}
	return values
}

// teamRoleTools 定义各角色 worker 可用的只读工具白名单。
var teamRoleTools = map[string][]string{
	"device":     {"netconf_rpc", "read_evidence"},
	"platform":   {"nbi_request", "collect_access_console_logs", "read_evidence"},
	"source":     {"search_files", "search_code", "read_file", "read_evidence"},
	"web":        {"web_search", "web_fetch", "read_evidence"},
	"correlator": {"read_evidence"},
}

var teamRoleGuidance = map[string]string{
	"device":     `Use dependency results to obtain the exact LT endpoint and narrow ONT/PON key before querying live state. Never guess a locator and never scan every LT. If the locator is absent, report that exact evidence gap instead of issuing a broad RPC. Prefer one narrow operational-state RPC and at most one follow-up RPC that closes a clearly stated gap.`,
	"platform":   `Use Access Console inventory/configuration APIs to resolve the target serial/MAC to its LT, AID, PON/channel-termination, deployment state, and authentication state. Begin with the narrowest verified inventory route. Query logs only when the API result leaves a concrete historical question.`,
	"source":     `Locate authoritative field/route/RPC definitions, not live target state. Follow the sourceSearch brief next to the assigned task. If exactTerms are present, use search_files inside the likely ownerPaths and then read the best matches. If no exact term is known, use search_code and read only the strongest results. Additional searches must test a distinct owner, contract, or symbol hypothesis; do not repeat equivalent searches with paraphrased keywords.`,
	"web":        `Use public sources only for facts unavailable in configured targets or workspaces. Search once with a focused query, open the strongest authoritative result, and stop when the assigned fact is established.`,
	"correlator": `Use only the dependency summaries and their evidence IDs. Re-read a cited evidence item only when its summary is insufficient to resolve a specific contradiction. Do not collect a new independent data source.`,
}

var teamRoleToolCallLimit = map[string]int64{
	"device":     4,
	"platform":   6,
	"source":     10,
	"web":        4,
	"correlator": 4,
}

// teamToolBudget 限制单个 worker 的工具调用总数与其中搜索类调用的次数。
type teamToolBudget struct {
	limit       int64
	used        atomic.Int64
	searchLimit int64
	searchUsed  atomic.Int64
}

// reserve 以原子方式占用一个工具调用名额；达到 limit 后返回 false。
func (b *teamToolBudget) reserve() bool {
	for {
		used := b.used.Load()
		if used >= b.limit {
			return false
		}
		if b.used.CompareAndSwap(used, used+1) {
			return true
		}
	}
}

func (b *teamToolBudget) reserveTool(name string) (bool, string) {
	isSearch := name == "search_files" || name == "search_code"
	if isSearch && b.searchLimit > 0 {
		for {
			used := b.searchUsed.Load()
			if used >= b.searchLimit {
				return false, fmt.Sprintf("Source search budget exhausted after %d search calls. Read the best existing matches and report; do not broaden the search.", b.searchLimit)
			}
			if b.searchUsed.CompareAndSwap(used, used+1) {
				break
			}
		}
	}
	if b.reserve() {
		return true, ""
	}
	if isSearch && b.searchLimit > 0 {
		b.searchUsed.Add(-1)
	}
	return false, fmt.Sprintf("Worker tool budget exhausted after %d calls. Do not request another tool; summarize the evidence already collected.", b.limit)
}

func (b *teamToolBudget) middleware() compose.ToolMiddleware {
	refuse := func(message string) (string, error) {
		payload, err := json.Marshal(agentToolObservation{
			Successful: false,
			Error:      message,
		})
		return string(payload), err
	}
	return compose.ToolMiddleware{
		Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
			return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
				if ok, message := b.reserveTool(input.Name); !ok {
					result, err := refuse(message)
					if err != nil {
						return nil, err
					}
					return &compose.ToolOutput{Result: result}, nil
				}
				return next(ctx, input)
			}
		},
		Streamable: func(next compose.StreamableToolEndpoint) compose.StreamableToolEndpoint {
			return func(ctx context.Context, input *compose.ToolInput) (*compose.StreamToolOutput, error) {
				if ok, message := b.reserveTool(input.Name); !ok {
					result, err := refuse(message)
					if err != nil {
						return nil, err
					}
					return &compose.StreamToolOutput{Result: schema.StreamReaderFromArray([]string{result})}, nil
				}
				return next(ctx, input)
			}
		},
	}
}

func teamSourceSearchMiddleware() compose.ToolMiddleware {
	return compose.ToolMiddleware{
		Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
			return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
				constrainTeamSourceToolInput(input)
				return next(ctx, input)
			}
		},
		Streamable: func(next compose.StreamableToolEndpoint) compose.StreamableToolEndpoint {
			return func(ctx context.Context, input *compose.ToolInput) (*compose.StreamToolOutput, error) {
				constrainTeamSourceToolInput(input)
				return next(ctx, input)
			}
		},
	}
}

func constrainTeamSourceToolInput(input *compose.ToolInput) {
	if input == nil {
		return
	}
	switch input.Name {
	case "search_files":
		var arguments fileSearchArguments
		if json.Unmarshal([]byte(input.Arguments), &arguments) != nil {
			return
		}
		if arguments.MaxResults == 0 || arguments.MaxResults > 50 {
			arguments.MaxResults = 50
		}
		if encoded, err := json.Marshal(arguments); err == nil {
			input.Arguments = string(encoded)
		}
	case "search_code":
		var arguments codeSearchArguments
		if json.Unmarshal([]byte(input.Arguments), &arguments) != nil {
			return
		}
		if arguments.TopK == 0 || arguments.TopK > 8 {
			arguments.TopK = 8
		}
		arguments.Rebuild = false
		if encoded, err := json.Marshal(arguments); err == nil {
			input.Arguments = string(encoded)
		}
	}
}

func teamSourceSearchBrief(step TeamStep) string {
	if step.Role != "source" || step.SourceSearch == nil {
		return ""
	}
	encoded, err := json.Marshal(step.SourceSearch)
	if err != nil {
		return ""
	}
	return fmt.Sprintf(`

Source search contract (host-enforced search cap; search_code uses the same persistent profile index as the supervisor):
%s
Treat ownerPaths as the first search boundary. When runtimeDerived is true, extract exact status/error/field literals from dependency evidence before searching. Do not perform a repository survey.`, encoded)
}

type engineTeamWorker struct {
	engine *Engine
	run    domain.Run
	model  model.ToolCallingChatModel
}

func (w *engineTeamWorker) Work(ctx context.Context, input TeamWorkInput) (TeamStepResult, error) {
	return w.engine.runTeamWorker(ctx, w.run, w.model, input)
}

// RunTeam 是显式的深度诊断入口。worker 历史刻意不保留：只有最终审阅后的
// 答案加入父对话，而工具证据持续持久化在父 run 之下。
func (e *Engine) RunTeam(ctx context.Context, runID string) error {
	run, exists := e.runner.Run(runID)
	if !exists {
		return fmt.Errorf("diagnostic run not found: %s", runID)
	}
	if strings.TrimSpace(run.ProfileID) == "" {
		return e.fail(runID, errors.New("target profile is required"))
	}
	e.mu.RLock()
	chatModel := e.model
	e.mu.RUnlock()
	if chatModel == nil {
		return e.fail(runID, errors.New("chat model is not configured"))
	}
	if err := e.runner.PublishAgentMessage(runID, "已启动深度诊断：Planner 将拆分只读取证任务，独立子 Agent 会通过 DAG 并行执行，最终由 Reviewer 汇总证据。"); err != nil {
		return e.fail(runID, err)
	}
	teamGoal := run.Goal
	memoryMessage, err := e.diagnosticMemoryMessage(run.ConversationID)
	if err != nil {
		return e.fail(runID, err)
	}
	if memoryMessage != nil && strings.TrimSpace(memoryMessage.Content) != "" {
		teamGoal += "\n\nExisting verified diagnostic memory:\n" + memoryMessage.Content
	}

	team := e.Team(&engineTeamWorker{engine: e, run: run, model: chatModel})
	team.OnPlanned = func(steps []TeamStep) error {
		if err := e.runner.PublishTeamEvent(runID, domain.EventTeamPlanned, map[string]any{
			"summary": fmt.Sprintf("Planner created %d investigation steps", len(steps)),
			"steps":   steps,
		}); err != nil {
			return err
		}
		questions := make([]string, 0, len(steps))
		for _, step := range steps {
			questions = append(questions, step.Goal)
		}
		return e.runner.SavePlan(domain.DiagnosticPlan{
			ConversationID:      run.ConversationID,
			Goal:                run.Goal,
			Target:              run.ProfileID,
			UnresolvedQuestions: questions,
			NextAction:          "并行执行当前无依赖的只读取证子任务",
			UpdatedAt:           run.StartedAt,
		})
	}
	team.OnStepStarted = func(step TeamStep) error {
		return e.runner.PublishTeamEvent(runID, domain.EventTeamStepStarted, map[string]any{
			"stepId":  step.ID,
			"role":    step.Role,
			"summary": step.Goal,
		})
	}
	team.OnStepFinished = func(_ TeamStep, result TeamStepResult) error {
		return e.runner.PublishTeamEvent(runID, domain.EventTeamStepFinished, result)
	}

	teamResult, err := team.Run(ctx, teamGoal)
	if err != nil {
		return e.fail(runID, err)
	}
	if teamResult.Answer == "" {
		return e.fail(runID, errors.New("team reviewer completed without an answer"))
	}
	if err = e.runner.PublishAgentMessage(runID, teamResult.Answer); err != nil {
		return e.fail(runID, err)
	}

	completed, unresolved := summarizeTeamPlan(teamResult)
	if err = e.runner.SavePlan(domain.DiagnosticPlan{
		ConversationID:      run.ConversationID,
		Goal:                run.Goal,
		Target:              run.ProfileID,
		CompletedChecks:     completed,
		UnresolvedQuestions: unresolved,
		NextAction:          "等待用户根据诊断结论决定是否继续取证或执行变更",
		UpdatedAt:           time.Now().UTC(),
	}); err != nil {
		return e.fail(runID, err)
	}

	messages := make([]*schema.Message, 0)
	contextJSON, contextExists, contextErr := e.runner.Context(run.ConversationID)
	if contextErr != nil {
		return e.fail(runID, contextErr)
	}
	if contextExists {
		if err = json.Unmarshal(contextJSON, &messages); err != nil {
			return e.fail(runID, fmt.Errorf("decode conversation context: %w", err))
		}
		messages = sanitizeModelMessages(messages)
	}
	messages = append(messages, buildUserMessage(run), schema.AssistantMessage(teamResult.Answer, nil))
	contextJSON, err = json.Marshal(retainModelMessages(messages))
	if err != nil {
		return e.fail(runID, fmt.Errorf("encode team conversation context: %w", err))
	}
	_, err = e.runner.Complete(runID, contextJSON)
	return err
}

func (e *Engine) Team(worker TeamWorker) *Team {
	e.mu.RLock()
	chatModel := e.model
	e.mu.RUnlock()
	return &Team{
		Planner:     LLMPlanner{Model: chatModel},
		Worker:      worker,
		Reviewer:    LLMReviewer{Model: chatModel},
		MaxParallel: 4,
	}
}

func (e *Engine) runTeamWorker(ctx context.Context, run domain.Run, chatModel model.ToolCallingChatModel, input TeamWorkInput) (TeamStepResult, error) {
	ctx = application.WithTeamWorkerExecution(ctx, input.Step.ID, input.Step.Role)
	if err := normalizeTeamSourceSearch(&input.Step); err != nil {
		return TeamStepResult{}, fmt.Errorf("validate team worker %s search brief: %w", input.Step.ID, err)
	}
	allTools, err := e.tools(run.ID, run.ProfileID)
	if err != nil {
		return TeamStepResult{}, err
	}
	allowedTools, err := selectTeamTools(ctx, allTools, teamRoleTools[input.Step.Role])
	if err != nil {
		return TeamStepResult{}, err
	}
	dependencyJSON, err := json.Marshal(input.Dependencies)
	if err != nil {
		return TeamStepResult{}, fmt.Errorf("encode dependency results: %w", err)
	}
	allowedNames := append([]string(nil), teamRoleTools[input.Step.Role]...)
	sort.Strings(allowedNames)
	workerInstruction := fmt.Sprintf(`%s

<isolated-team-worker>
You are the isolated %s worker for step %s. Investigate only the assigned step.
You do not share chat history with other workers. Dependency results below are the only upstream messages you may rely on.
Use only these host-enforced read-only tools: %s.
Every observed fact must cite an evidence ID returned by a tool. Do not invent unavailable state.
Stop as soon as the step goal is established or the exact blocker is known.
Role-specific operating guidance: %s
Your final response is a concise evidence report for the supervisor, not JSON. State observed facts first with their evidence IDs, then inference and remaining gaps. The host constructs and validates the structured handoff envelope.
</isolated-team-worker>`, e.instruction(run.ProfileID), input.Step.Role, input.Step.ID, strings.Join(allowedNames, ", "), teamRoleGuidance[input.Step.Role])
	workerPrompt := fmt.Sprintf("Original diagnostic goal:\n%s\n\nAssigned step:\n%s%s\n\nDependency results (summaries and evidence references only):\n%s", input.OriginalGoal, input.Step.Goal, teamSourceSearchBrief(input.Step), dependencyJSON)

	usageTracker := &tokenUsageTracker{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		runID:                        run.ID,
		conversationID:               run.ConversationID,
		onRecord:                     e.runner.SaveTokenUsage,
	}
	iterationTracker := &modelIterationTracker{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		softThreshold:                teamWorkerSoftStop,
	}
	toolBudget := &teamToolBudget{limit: teamRoleToolCallLimit[input.Step.Role]}
	toolMiddlewares := []compose.ToolMiddleware{toolBudget.middleware(), e.recoverToolCallErrors(run.ID)}
	if input.Step.Role == "source" {
		toolBudget.searchLimit = int64(input.Step.SourceSearch.MaxSearches)
		toolMiddlewares = append([]compose.ToolMiddleware{teamSourceSearchMiddleware()}, toolMiddlewares...)
	}
	workerAgent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "team_worker_" + strings.ReplaceAll(input.Step.ID, "-", "_"),
		Description: "Isolated read-only OLT diagnostic evidence worker",
		Instruction: workerInstruction,
		Model:       chatModel,
		ModelRetryConfig: &adk.ModelRetryConfig{
			MaxRetries: 2,
			ShouldRetry: func(_ context.Context, retryContext *adk.RetryContext) *adk.RetryDecision {
				return &adk.RetryDecision{Retry: retryContext != nil && modelErrorIsRetryable(retryContext.Err)}
			},
		},
		GenModelInput: func(_ context.Context, instruction string, agentInput *adk.AgentInput) ([]*schema.Message, error) {
			systemMessages := []*schema.Message{schema.SystemMessage(instruction)}
			if iterationTracker.count() >= teamWorkerSoftStop {
				systemMessages = append(systemMessages, schema.SystemMessage(`The worker investigation budget is exhausted. Do not call another tool. Using only the evidence already returned, write the final supervisor report now. If the evidence is insufficient, state the exact gap; a bounded partial result is valid and preferable to another search.`))
			}
			return modelMessagesWithLeadingSystem(systemMessages, agentInput.Messages), nil
		},
		MaxIterations: teamWorkerMaxRounds,
		Handlers:      []adk.ChatModelAgentMiddleware{iterationTracker, usageTracker},
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools:               allowedTools,
			ExecuteSequentially: false,
			ToolCallMiddlewares: toolMiddlewares,
		}},
	})
	if err != nil {
		return TeamStepResult{}, fmt.Errorf("create team worker %s: %w", input.Step.ID, err)
	}

	workerRunner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: workerAgent})
	events := workerRunner.Run(ctx, []*schema.Message{schema.UserMessage(workerPrompt)})
	finalContent := ""
	evidenceIDs := make([]string, 0)
	evidenceDigests := make([]string, 0)
	toolErrors := make([]string, 0)
	var iterationLimitErr error
	for {
		event, ok := events.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			explainedErr := explainModelProviderError(event.Err)
			if isTeamWorkerIterationLimit(explainedErr) {
				iterationLimitErr = explainedErr
				break
			}
			return TeamStepResult{}, fmt.Errorf("team worker %s: %w", input.Step.ID, explainedErr)
		}
		message, _, messageErr := adk.GetMessage(event)
		if messageErr != nil {
			return TeamStepResult{}, fmt.Errorf("read team worker %s event: %w", input.Step.ID, messageErr)
		}
		if message == nil {
			continue
		}
		if message.Role == schema.Tool {
			var observation agentToolObservation
			if json.Unmarshal([]byte(message.Content), &observation) == nil {
				if observation.Evidence != nil {
					evidenceIDs = append(evidenceIDs, observation.Evidence.ID)
					evidenceDigests = append(evidenceDigests, compactTeamEvidence(*observation.Evidence))
				}
				if observation.Error != "" {
					toolErrors = append(toolErrors, observation.Error)
				}
			}
		}
		if message.Role == schema.Assistant && len(message.ToolCalls) == 0 {
			if content := visibleAssistantContent(message.Content); content != "" {
				finalContent = content
			}
		}
	}
	if err = ctx.Err(); err != nil {
		return TeamStepResult{}, err
	}
	if iterationLimitErr != nil && strings.TrimSpace(finalContent) == "" {
		finalContent, err = e.finalizeTeamWorkerReport(ctx, run, chatModel, input, evidenceDigests, toolErrors)
		if err == nil {
			iterationLimitErr = nil
		} else {
			toolErrors = append(toolErrors, "No-tool finalization failed: "+err.Error())
		}
	}
	return buildTeamWorkerResult(finalContent, evidenceIDs, evidenceDigests, toolErrors, iterationLimitErr)
}

// finalizeTeamWorkerReport 在 worker 达到迭代上限却未产出报告时，
// 用无工具的一次模型调用依据已有证据快照补写最终报告。
func (e *Engine) finalizeTeamWorkerReport(ctx context.Context, run domain.Run, finalizerModel model.ToolCallingChatModel, input TeamWorkInput, evidenceDigests, toolErrors []string) (string, error) {
	if finalizerModel == nil {
		return "", errors.New("worker finalizer model is not configured")
	}
	dependencies, err := json.Marshal(input.Dependencies)
	if err != nil {
		return "", fmt.Errorf("encode finalizer dependencies: %w", err)
	}
	evidenceContext := abbreviateContext(strings.Join(evidenceDigests, "\n"), teamWorkerReportMax)
	errorContext := abbreviateContext(strings.Join(uniqueStrings(toolErrors), "\n"), 3000)
	startedAt := time.Now()
	response, err := finalizerModel.Generate(ctx, []*schema.Message{
		schema.SystemMessage(`You are the no-tool finalization stage for one OLT diagnostic worker. You cannot call tools. Write a concise Simplified Chinese supervisor report using only the supplied dependency results and evidence snapshots. Separate observed facts, inference, and remaining gaps. Cite only evidence IDs present in the snapshots. A bounded partial conclusion is valid; never ask for another search or invent missing state.`),
		schema.UserMessage(fmt.Sprintf("Original goal:\n%s\n\nWorker role and step:\n%s / %s\n\nAssigned goal:\n%s\n\nDependencies:\n%s\n\nEvidence snapshots:\n%s\n\nTool errors:\n%s", input.OriginalGoal, input.Step.Role, input.Step.ID, input.Step.Goal, dependencies, evidenceContext, errorContext)),
	})
	if err != nil {
		return "", explainModelProviderError(err)
	}
	if response == nil || strings.TrimSpace(response.Content) == "" {
		return "", errors.New("worker finalizer returned an empty report")
	}
	if response.ResponseMeta != nil && response.ResponseMeta.Usage != nil {
		usage := response.ResponseMeta.Usage
		if err = e.runner.SaveTokenUsage(domain.TokenUsageRecord{
			ID:             uuid.NewString(),
			RunID:          run.ID,
			ConversationID: run.ConversationID,
			Iteration:      teamWorkerMaxRounds + 1,
			InputTokens:    usage.PromptTokens,
			OutputTokens:   usage.CompletionTokens,
			TotalTokens:    usage.TotalTokens,
			ElapsedMS:      time.Since(startedAt).Milliseconds(),
			RecordedAt:     time.Now().UTC(),
		}); err != nil {
			return "", fmt.Errorf("record worker finalizer token usage: %w", err)
		}
	}
	return visibleAssistantContent(response.Content), nil
}

func selectTeamTools(ctx context.Context, available []einotool.BaseTool, allowedNames []string) ([]einotool.BaseTool, error) {
	allowed := make(map[string]bool, len(allowedNames))
	for _, name := range allowedNames {
		allowed[name] = true
	}
	selected := make([]einotool.BaseTool, 0, len(allowed))
	found := make(map[string]bool, len(allowed))
	for _, candidate := range available {
		info, err := candidate.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("read team tool info: %w", err)
		}
		if allowed[info.Name] {
			selected = append(selected, candidate)
			found[info.Name] = true
		}
	}
	for _, name := range allowedNames {
		if !found[name] {
			return nil, fmt.Errorf("team role requires unavailable tool %s", name)
		}
	}
	return selected, nil
}

func buildTeamWorkerResult(content string, evidenceIDs, evidenceDigests, toolErrors []string, iterationErr error) (TeamStepResult, error) {
	content = strings.TrimSpace(content)
	evidenceIDs = uniqueStrings(evidenceIDs)
	toolErrors = uniqueStrings(toolErrors)

	if content == "" && len(evidenceIDs) == 0 {
		if iterationErr != nil {
			return TeamStepResult{}, iterationErr
		}
		if len(toolErrors) > 0 {
			return TeamStepResult{}, errors.New(strings.Join(toolErrors, "; "))
		}
		return TeamStepResult{}, errors.New("worker completed without evidence or a final report")
	}

	result := TeamStepResult{
		Status:      teamStepSuccess,
		Summary:     abbreviateContext(content, teamWorkerReportMax),
		EvidenceIDs: evidenceIDs,
	}
	if content == "" {
		result.Status = teamStepPartial
		result.Summary = fmt.Sprintf("Worker collected %d evidence item(s) but reached its investigation limit before producing a final synthesis.", len(evidenceIDs))
		if digest := strings.TrimSpace(strings.Join(evidenceDigests, "\n")); digest != "" {
			result.Summary += "\n\nHost-preserved evidence snapshots:\n" + abbreviateContext(digest, teamWorkerReportMax-len(result.Summary))
		}
	}
	if len(evidenceIDs) == 0 {
		result.Status = teamStepPartial
		result.Unknowns = append(result.Unknowns, "No successful tool evidence was captured for this worker.")
	}
	if iterationErr != nil {
		result.Status = teamStepPartial
		result.Error = iterationErr.Error()
		result.Unknowns = append(result.Unknowns, "The worker reached its bounded investigation limit before a normal final response.")
	}
	if len(toolErrors) > 0 {
		result.Unknowns = append(result.Unknowns, toolErrors...)
	}
	result.Unknowns = uniqueStrings(result.Unknowns)
	return result, nil
}

func compactTeamEvidence(evidence domain.Evidence) string {
	payload, err := json.Marshal(map[string]any{
		"evidenceId": evidence.ID,
		"kind":       evidence.Kind,
		"summary":    evidence.Summary,
		"data":       evidence.Data,
		"message":    evidence.Message,
	})
	if err != nil {
		return fmt.Sprintf("- [%s] %s: %s", evidence.ID, evidence.Kind, evidence.Summary)
	}
	return abbreviateContext(string(payload), 2400)
}

func isTeamWorkerIterationLimit(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "exceeds max iterations")
}

func cleanJSONBlock(content string) string {
	clean := strings.TrimSpace(content)
	clean = strings.TrimPrefix(clean, "```json")
	clean = strings.TrimPrefix(clean, "```")
	clean = strings.TrimSuffix(strings.TrimSpace(clean), "```")
	return strings.TrimSpace(clean)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// summarizeTeamPlan 把各步骤结果归并为诊断计划的完成项与未解决问题。
func summarizeTeamPlan(result TeamRunResult) (completed []string, unresolved []string) {
	for _, step := range result.Steps {
		stepResult := result.Results[step.ID]
		switch stepResult.Status {
		case teamStepSuccess:
			completed = append(completed, step.Goal)
		case teamStepPartial:
			completed = append(completed, step.Goal+"（部分完成）")
			unresolved = append(unresolved, stepResult.Unknowns...)
		case teamStepFailed, teamStepSkipped:
			unresolved = append(unresolved, step.Goal+"："+stepResult.Summary)
		}
	}
	return uniqueStrings(completed), uniqueStrings(unresolved)
}
