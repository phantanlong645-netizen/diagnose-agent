package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"olt-diagnostic-agent/internal/domain"
	"olt-diagnostic-agent/internal/policy"
	"olt-diagnostic-agent/internal/tools"
)

var ErrApprovalDeclined = errors.New("tool execution approval declined")

type EventSink interface {
	Emit(event domain.Event)
}

type RunJournal interface {
	StartRun(run domain.Run, event domain.Event) error
	FinishRun(run domain.Run, runError string, contextJSON []byte, event domain.Event) error
	SaveContext(conversationID, sourceRunID string, contextJSON []byte) error
	SaveContextFact(fact domain.ContextFact) error
	ContextFacts(conversationID string, limit int) ([]domain.ContextFact, error)
	SavePlan(plan domain.DiagnosticPlan) error
	Plan(conversationID string) (domain.DiagnosticPlan, bool, error)
	SaveToolFingerprint(record domain.ToolFingerprint) error
	ToolFingerprint(conversationID, fingerprint string) (domain.ToolFingerprint, bool, error)
	// DeleteToolFingerprintsByConversation 清除指定 conversation 的所有 tool fingerprint，
	// 在写入工具成功后调用，防止后续只读 GET 命中写入前的旧缓存。
	DeleteToolFingerprintsByConversation(conversationID string) error
	Append(event domain.Event) error
	Context(conversationID string) ([]byte, bool, error)
	Evidence(runID, evidenceID string) (domain.Evidence, bool, error)
	// EvidenceByConversation 按 conversation_id + evidence_id 拿回旧 evidence，
	// 用于跨 Run 但同 conversation 的工具结果复用。
	// 比如用户重开会话继续问同一个事实，命中 SQLite 里的 tool_fingerprints 后，
	// 直接通过 evidence_id 拿回上一次的只读结果，不需要重新发 NBI/NETCONF 请求。
	EvidenceByConversation(conversationID, evidenceID string) (domain.Evidence, bool, error)
}

type ApprovalHandler interface {
	Open(request domain.ApprovalRequest) error
	Wait(ctx context.Context, callID string) (bool, error)
	Resolve(callID string, approved bool, approveConversation bool) error
	ConversationApproved(conversationID string) bool
}

type Runner struct {
	registry *tools.Registry
	policy   *policy.Engine
	journal  RunJournal
	sink     EventSink
	approval ApprovalHandler

	mu            sync.RWMutex
	runs          map[string]domain.Run
	toolExecution sync.RWMutex
	evidenceMu    sync.RWMutex
	reusable      map[string]domain.Evidence
}

func NewRunner(registry *tools.Registry, policyEngine *policy.Engine, journal RunJournal, sink EventSink, approval ApprovalHandler) *Runner {
	return &Runner{
		registry: registry,
		policy:   policyEngine,
		journal:  journal,
		sink:     sink,
		approval: approval,
		runs:     make(map[string]domain.Run),
		reusable: make(map[string]domain.Evidence),
	}
}

func (r *Runner) Start(goal, profileID, conversationID string, images ...domain.ImageAttachment) (domain.Run, error) {
	run := domain.Run{
		ID:             uuid.NewString(),
		ConversationID: conversationID,
		ProfileID:      profileID,
		Goal:           goal,
		Images:         images,
		Status:         domain.RunRunning,
		StartedAt:      time.Now().UTC(),
	}
	event := newRunEvent(run, domain.EventRunStarted, publicRun(run))
	if err := r.journal.StartRun(run, event); err != nil {
		return domain.Run{}, err
	}
	r.ResetEvidenceReads()
	r.mu.Lock()
	r.runs[run.ID] = run
	r.mu.Unlock()
	if err := r.journal.SaveContextFact(domain.ContextFact{
		ConversationID: run.ConversationID,
		Key:            "goal",
		Kind:           "goal",
		Status:         "confirmed",
		Content:        run.Goal,
		Confidence:     1,
		FirstSeen:      run.StartedAt,
		LastSeen:       run.StartedAt,
	}); err != nil {
		return domain.Run{}, err
	}
	if err := r.journal.SavePlan(domain.DiagnosticPlan{
		ConversationID: run.ConversationID,
		Goal:           run.Goal,
		NextAction:     "根据目标选择最小的只读证据源",
		UpdatedAt:      run.StartedAt,
	}); err != nil {
		return domain.Run{}, err
	}
	r.emit(event)
	return publicRun(run), nil
}

func (r *Runner) Fail(runID string, runErr error) (domain.Run, error) {
	r.mu.Lock()
	run, exists := r.runs[runID]
	if !exists {
		r.mu.Unlock()
		return domain.Run{}, fmt.Errorf("run not found: %s", runID)
	}
	if run.Status != domain.RunRunning {
		r.mu.Unlock()
		return run, nil
	}
	endedAt := time.Now().UTC()
	run.Status = domain.RunFailed
	run.EndedAt = &endedAt
	r.mu.Unlock()
	public := publicRun(run)
	event := newRunEvent(run, domain.EventRunFailed, map[string]any{
		"run":   public,
		"error": runErr.Error(),
	})
	if err := r.journal.FinishRun(run, runErr.Error(), nil, event); err != nil {
		return run, err
	}
	r.mu.Lock()
	r.runs[runID] = public
	r.mu.Unlock()
	r.emit(event)
	return public, nil
}

func (r *Runner) Cancel(runID string) (domain.Run, error) {
	r.mu.Lock()
	run, exists := r.runs[runID]
	if !exists {
		r.mu.Unlock()
		return domain.Run{}, fmt.Errorf("run not found: %s", runID)
	}
	if run.Status != domain.RunRunning {
		r.mu.Unlock()
		return run, nil
	}
	endedAt := time.Now().UTC()
	run.Status = domain.RunCancelled
	run.EndedAt = &endedAt
	r.mu.Unlock()
	public := publicRun(run)
	event := newRunEvent(run, domain.EventRunCancelled, public)
	if err := r.journal.FinishRun(run, "", nil, event); err != nil {
		return run, err
	}
	r.mu.Lock()
	r.runs[runID] = public
	r.mu.Unlock()
	r.emit(event)
	return public, nil
}

func (r *Runner) PublishAgentMessage(runID, content string) error {
	return r.publish(runID, domain.EventAgentMessage, map[string]string{"content": content})
}

func (r *Runner) Run(runID string) (domain.Run, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	run, exists := r.runs[runID]
	return run, exists
}

func (r *Runner) Context(conversationID string) ([]byte, bool, error) {
	return r.journal.Context(conversationID)
}

// ResetEvidenceReads 在每个新 run 开始时清空 read_evidence 的已读记录，
// 防止跨 run 误拦模型重新读取新 run 产生的证据。
func (r *Runner) ResetEvidenceReads() {
	if tool, ok := r.registry.Tool("read_evidence"); ok {
		if evidenceTool, ok := tool.(*tools.EvidenceTool); ok {
			evidenceTool.ResetReads()
		}
	}
}

func (r *Runner) ContextFacts(conversationID string, limit int) ([]domain.ContextFact, error) {
	return r.journal.ContextFacts(conversationID, limit)
}

func (r *Runner) SaveContextFact(fact domain.ContextFact) error {
	return r.journal.SaveContextFact(fact)
}

func (r *Runner) Plan(conversationID string) (domain.DiagnosticPlan, bool, error) {
	return r.journal.Plan(conversationID)
}

func (r *Runner) SavePlan(plan domain.DiagnosticPlan) error {
	return r.journal.SavePlan(plan)
}

func (r *Runner) ToolFingerprint(conversationID, fingerprint string) (domain.ToolFingerprint, bool, error) {
	return r.journal.ToolFingerprint(conversationID, fingerprint)
}

func (r *Runner) SaveToolFingerprint(record domain.ToolFingerprint) error {
	return r.journal.SaveToolFingerprint(record)
}

// SaveContext checkpoints a complete, model-sendable conversation boundary
// without changing the run status. It is deliberately separate from Complete
// so a process crash can still leave the latest safe context in SQLite.
func (r *Runner) SaveContext(runID string, contextJSON []byte) error {
	if len(contextJSON) == 0 {
		return nil
	}
	r.mu.RLock()
	run, exists := r.runs[runID]
	r.mu.RUnlock()
	if !exists {
		return fmt.Errorf("run not found: %s", runID)
	}
	return r.journal.SaveContext(run.ConversationID, run.ID, contextJSON)
}

func (r *Runner) Evidence(runID, evidenceID string) (domain.Evidence, bool, error) {
	run, exists := r.Run(runID)
	if !exists {
		return domain.Evidence{}, false, fmt.Errorf("run not found: %s", runID)
	}
	evidence, found, err := r.journal.Evidence(runID, evidenceID)
	if err != nil {
		return domain.Evidence{}, false, err
	}
	if found {
		return evidence, true, nil
	}
	// 当前 run 没找到，回退到跨 run 查找（同 conversation 内）。
	// 上下文压缩后保留的 evidence ID 可能来自之前的 run。
	return r.journal.EvidenceByConversation(run.ConversationID, evidenceID)
}

func (r *Runner) Execute(ctx context.Context, runID string, call domain.ToolCall) (domain.Evidence, error) {
	run, exists := r.Run(runID)
	if !exists {
		return domain.Evidence{}, fmt.Errorf("run not found: %s", runID)
	}
	if run.Status != domain.RunRunning {
		return domain.Evidence{}, fmt.Errorf("run is not active: %s", run.Status)
	}

	tool, prepared, err := r.registry.Prepare(call)
	if err != nil {
		if eventErr := r.failTool(runID, call, err); eventErr != nil {
			return domain.Evidence{}, errors.Join(err, eventErr)
		}
		return domain.Evidence{}, err
	}
	publicCall := map[string]any{
		"callId":      call.ID,
		"name":        call.Name,
		"summary":     prepared.Summary,
		"annotations": prepared.Annotations,
	}
	if err = r.publish(runID, domain.EventToolProposed, publicCall); err != nil {
		return domain.Evidence{}, err
	}

	decision := r.policy.Evaluate(prepared)
	if !decision.Allowed {
		err = errors.New(decision.Reason)
		if eventErr := r.failTool(runID, call, err); eventErr != nil {
			return domain.Evidence{}, errors.Join(err, eventErr)
		}
		return domain.Evidence{}, err
	}

	// Eino may execute independent tool calls concurrently. Keep the
	// explicitly supported read-only tools in a shared read section, while
	// writes, shell commands, sensitive reads, and unknown tools take the
	// exclusive section. The gate is acquired before approval so concurrent
	// state-changing calls cannot create multiple approval prompts or overlap
	// after approval.
	releaseToolExecution := r.acquireToolExecution(prepared)
	defer releaseToolExecution()

	if decision.ApprovalRequired {
		if r.approval != nil && r.approval.ConversationApproved(run.ConversationID) {
			if err = r.publish(runID, domain.EventApprovalResolved, map[string]any{
				"callId":    call.ID,
				"approved":  true,
				"automatic": true,
				"scope":     "conversation",
			}); err != nil {
				return domain.Evidence{}, err
			}
		} else {
			request := domain.ApprovalRequest{
				ConversationID: run.ConversationID,
				RunID:          runID,
				CallID:         call.ID,
				ToolName:       call.Name,
				Summary:        prepared.Summary,
				Reason:         decision.Reason,
				Preview:        prepared.Preview,
				Annotations:    prepared.Annotations,
			}
			if r.approval == nil {
				err = errors.New("approval handler is not configured")
				if eventErr := r.failTool(runID, call, err); eventErr != nil {
					return domain.Evidence{}, errors.Join(err, eventErr)
				}
				return domain.Evidence{}, err
			}
			if err = r.approval.Open(request); err != nil {
				if eventErr := r.failTool(runID, call, err); eventErr != nil {
					return domain.Evidence{}, errors.Join(err, eventErr)
				}
				return domain.Evidence{}, err
			}
			if err = r.publish(runID, domain.EventApprovalRequired, request); err != nil {
				_ = r.approval.Resolve(call.ID, false, false)
				return domain.Evidence{}, err
			}
			approved, approvalErr := r.approval.Wait(ctx, call.ID)
			if approvalErr != nil {
				if eventErr := r.failTool(runID, call, approvalErr); eventErr != nil {
					return domain.Evidence{}, errors.Join(approvalErr, eventErr)
				}
				return domain.Evidence{}, approvalErr
			}
			if !approved {
				if err = r.publish(runID, domain.EventApprovalResolved, map[string]any{"callId": call.ID, "approved": false}); err != nil {
					return domain.Evidence{}, err
				}
				if eventErr := r.failTool(runID, call, ErrApprovalDeclined); eventErr != nil {
					return domain.Evidence{}, errors.Join(ErrApprovalDeclined, eventErr)
				}
				return domain.Evidence{}, ErrApprovalDeclined
			}
			scope := "once"
			if r.approval.ConversationApproved(run.ConversationID) {
				scope = "conversation"
			}
			if err = r.publish(runID, domain.EventApprovalResolved, map[string]any{"callId": call.ID, "approved": true, "scope": scope}); err != nil {
				return domain.Evidence{}, err
			}
		}
	}
	fingerprint := toolCallFingerprint(call)
	if canReuseToolResult(prepared) {
		reuseKey := reusableEvidenceKey(run, fingerprint)
		r.evidenceMu.RLock()
		reused, found := r.reusable[reuseKey]
		r.evidenceMu.RUnlock()
		if found {
			if err = r.publish(runID, domain.EventToolStarted, publicCall); err != nil {
				return domain.Evidence{}, err
			}
			if err = r.publish(runID, domain.EventToolCompleted, map[string]any{
				"callId": call.ID, "evidenceId": reused.ID, "reused": true,
			}); err != nil {
				return domain.Evidence{}, err
			}
			return reused, nil
		}
		// 内存缓存未命中：查 SQLite 的 tool_fingerprints 表，看同 conversation 是否
		// 之前 Run 跑过相同的只读调用。命中后通过 evidence_id 直接拿回旧结果，
		// 避免重新发 NBI/NETCONF 请求或重新扫文件。这对"重开会话继续问"场景特别有用：
		// 用户重启应用后，in-memory 缓存清空，但 SQLite 里的 fingerprint 还在。
		//
		// 安全性：只对 canReuseToolResult 返回 true 的工具复用（ReadOnly+Idempotent+!Destructive+!Sensitive）。
		// 设备状态会随时间变化，但同 conversation 的连续对话通常在短时间内发生，
		// 复用上一次查询结果是可接受的（用户想刷新可以换一个查询参数，fingerprint 就不同）。
		storedRecord, storedFound, storedErr := r.journal.ToolFingerprint(run.ConversationID, fingerprint)
		if storedErr == nil && storedFound && storedRecord.Successful && storedRecord.EvidenceID != "" {
			storedEvidence, evidenceFound, evidenceErr := r.journal.EvidenceByConversation(run.ConversationID, storedRecord.EvidenceID)
			if evidenceErr == nil && evidenceFound {
				// 把跨 Run 复用的 evidence 也填进内存缓存，避免后续命中还要再查 SQLite。
				r.evidenceMu.Lock()
				r.reusable[reuseKey] = storedEvidence
				r.evidenceMu.Unlock()
				if err = r.publish(runID, domain.EventToolStarted, publicCall); err != nil {
					return domain.Evidence{}, err
				}
				if err = r.publish(runID, domain.EventToolCompleted, map[string]any{
					"callId": call.ID, "evidenceId": storedEvidence.ID, "reused": true, "source": "sqlite",
				}); err != nil {
					return domain.Evidence{}, err
				}
				return storedEvidence, nil
			}
		}
	}

	if err = r.publish(runID, domain.EventToolStarted, publicCall); err != nil {
		return domain.Evidence{}, err
	}
	result, err := tool.Execute(ctx, prepared)
	if err != nil {
		if eventErr := r.failTool(runID, call, err); eventErr != nil {
			return domain.Evidence{}, errors.Join(err, eventErr)
		}
		return domain.Evidence{}, err
	}

	evidence := domain.Evidence{
		ID:         uuid.NewString(),
		RunID:      runID,
		CallID:     call.ID,
		Kind:       call.Name,
		Summary:    result.Summary,
		Data:       result.Data,
		Message:    result.Message,
		Metadata:   result.Metadata,
		CapturedAt: time.Now().UTC(),
	}
	if err = r.publish(runID, domain.EventEvidenceCaptured, evidence); err != nil {
		return domain.Evidence{}, err
	}
	if err = r.publish(runID, domain.EventToolCompleted, map[string]any{
		"callId":     call.ID,
		"evidenceId": evidence.ID,
	}); err != nil {
		return domain.Evidence{}, err
	}
	if err = r.recordToolMemory(run, call, prepared, evidence); err != nil {
		return domain.Evidence{}, err
	}
	if canReuseToolResult(prepared) {
		reuseKey := reusableEvidenceKey(run, fingerprint)
		r.evidenceMu.Lock()
		r.reusable[reuseKey] = evidence
		r.evidenceMu.Unlock()
	}
	// 写入类工具（POST/PUT/PATCH/DELETE/edit-config/commit 等）成功后，必须清空
	// 该 conversation 的所有只读缓存。否则压缩后模型重新 GET 同一 URL 会命中旧缓存，
	// 返回写入前的快照，导致"写入成功但验证看不到结果"的幻觉。
	// canReuseToolResult 只对 ReadOnly+Idempotent 工具返回 true，写入工具不在此列，
	// 所以这里用 !ReadOnly 作为"写入类"的判据。
	if !prepared.Annotations.ReadOnly {
		r.invalidateReusableCache(run.ConversationID)
	}
	return evidence, nil
}

// MCPToolInfos returns the minimal description of every registered MCP tool so
// the agent engine can build dynamic Eino tools with the correct JSON schema.
func (r *Runner) MCPToolInfos() []tools.MCPToolInfo {
	return r.registry.MCPToolInfos()
}

// PrepareTool validates a proposed tool call without publishing events,
// requesting approval, authenticating, or contacting an external target.
// It is used by the Manual AI builder before it fills the executable form.
func (r *Runner) PrepareTool(call domain.ToolCall) (domain.PreparedCall, error) {
	_, prepared, err := r.registry.Prepare(call)
	return prepared, err
}

// invalidateReusableCache 清除指定 conversation 的所有只读工具缓存（内存 + SQLite）。
// 在写入工具成功后调用，确保后续 GET 不命中写入前的旧 evidence。
// 内存缓存 key 格式是 "ConversationID\x00ProfileID\x00fingerprint"，
// 所以按 ConversationID 前缀匹配即可清除该会话下的所有缓存。
// SQLite 的 tool_fingerprints 表也按 conversation_id 清除，防止跨 run 复用旧结果。
func (r *Runner) invalidateReusableCache(conversationID string) {
	r.evidenceMu.Lock()
	prefix := conversationID + "\x00"
	for key := range r.reusable {
		if strings.HasPrefix(key, prefix) {
			delete(r.reusable, key)
		}
	}
	r.evidenceMu.Unlock()
	if r.journal != nil {
		_ = r.journal.DeleteToolFingerprintsByConversation(conversationID)
	}
}

func (r *Runner) recordToolMemory(run domain.Run, call domain.ToolCall, prepared domain.PreparedCall, evidence domain.Evidence) error {
	fingerprint := toolCallFingerprint(call)
	content, err := json.Marshal(map[string]any{
		"tool":       call.Name,
		"summary":    evidence.Summary,
		"metadata":   evidence.Metadata,
		"evidenceId": evidence.ID,
	})
	if err != nil {
		return fmt.Errorf("encode diagnostic memory observation: %w", err)
	}
	now := evidence.CapturedAt
	if err = r.journal.SaveContextFact(domain.ContextFact{
		ConversationID: run.ConversationID,
		Key:            "tool:" + fingerprint,
		Kind:           "observed_state",
		Status:         "observed",
		Content:        string(content),
		EvidenceIDs:    []string{evidence.ID},
		SourceTool:     call.Name,
		SourceLocator:  prepared.Summary,
		Confidence:     1,
		FirstSeen:      now,
		LastSeen:       now,
	}); err != nil {
		return err
	}
	return r.journal.SaveToolFingerprint(domain.ToolFingerprint{
		ConversationID: run.ConversationID,
		Fingerprint:    fingerprint,
		ToolName:       call.Name,
		EvidenceID:     evidence.ID,
		Successful:     true,
		LastSeen:       now,
	})
}

func toolCallFingerprint(call domain.ToolCall) string {
	arguments := bytesTrimSpace(call.Arguments)
	var decoded any
	if json.Unmarshal(arguments, &decoded) == nil {
		if canonical, err := json.Marshal(decoded); err == nil {
			arguments = canonical
		}
	}
	digest := sha256.Sum256(append([]byte(strings.TrimSpace(call.Name)+"\n"), arguments...))
	return hex.EncodeToString(digest[:])
}

func bytesTrimSpace(value []byte) []byte {
	return bytes.TrimSpace(value)
}

// reusableEvidenceKey scopes in-memory evidence reuse to the conversation and
// selected target. The same RPC or NBI request can legitimately return a
// different result for another OLT, so a global fingerprint-only cache would
// leak evidence across targets.
func reusableEvidenceKey(run domain.Run, fingerprint string) string {
	return strings.Join([]string{run.ConversationID, run.ProfileID, fingerprint}, "\x00")
}

func (r *Runner) acquireToolExecution(call domain.PreparedCall) func() {
	if canExecuteToolConcurrently(call) {
		r.toolExecution.RLock()
		return r.toolExecution.RUnlock
	}
	r.toolExecution.Lock()
	return r.toolExecution.Unlock
}

func canExecuteToolConcurrently(call domain.PreparedCall) bool {
	if !call.Annotations.ReadOnly || call.Annotations.Destructive || call.Annotations.Sensitive {
		return false
	}
	switch call.Name {
	case "read_file", "search_files", "nbi_request", "collect_access_console_logs", "netconf_rpc", "web_search", "web_fetch", "search_code":
		return true
	default:
		return false
	}
}

func canReuseToolResult(call domain.PreparedCall) bool {
	return call.Annotations.ReadOnly && call.Annotations.Idempotent && !call.Annotations.Destructive && !call.Annotations.Sensitive
}

func (r *Runner) Complete(runID string, contextJSON []byte) (domain.Run, error) {
	r.mu.Lock()
	run, exists := r.runs[runID]
	if !exists {
		r.mu.Unlock()
		return domain.Run{}, fmt.Errorf("run not found: %s", runID)
	}
	if run.Status != domain.RunRunning {
		r.mu.Unlock()
		return run, nil
	}
	endedAt := time.Now().UTC()
	run.Status = domain.RunCompleted
	run.EndedAt = &endedAt
	r.mu.Unlock()
	public := publicRun(run)
	event := newRunEvent(run, domain.EventRunCompleted, public)
	if err := r.journal.FinishRun(run, "", contextJSON, event); err != nil {
		return run, err
	}
	r.mu.Lock()
	r.runs[runID] = public
	r.mu.Unlock()
	r.emit(event)
	return public, nil
}

func (r *Runner) failTool(runID string, call domain.ToolCall, err error) error {
	if publishErr := r.publish(runID, domain.EventToolFailed, map[string]any{
		"callId": call.ID,
		"error":  err.Error(),
	}); publishErr != nil {
		return publishErr
	}
	run, exists := r.Run(runID)
	if !exists {
		return fmt.Errorf("run not found: %s", runID)
	}
	return r.journal.SaveContextFact(domain.ContextFact{
		ConversationID: run.ConversationID,
		Key:            "error:" + toolCallFingerprint(call),
		Kind:           "error",
		Status:         "observed",
		Content:        err.Error(),
		SourceTool:     call.Name,
		Confidence:     1,
		FirstSeen:      time.Now().UTC(),
		LastSeen:       time.Now().UTC(),
	})
}

func (r *Runner) publish(runID string, eventType domain.EventType, payload any) error {
	run, exists := r.Run(runID)
	if !exists {
		return fmt.Errorf("run not found: %s", runID)
	}
	event := newRunEvent(run, eventType, payload)
	if err := r.journal.Append(event); err != nil {
		return err
	}
	r.emit(event)
	return nil
}

func (r *Runner) emit(event domain.Event) {
	if r.sink != nil {
		r.sink.Emit(event)
	}
}

func newRunEvent(run domain.Run, eventType domain.EventType, payload any) domain.Event {
	return domain.Event{
		ID:             uuid.NewString(),
		ConversationID: run.ConversationID,
		RunID:          run.ID,
		Type:           eventType,
		Timestamp:      time.Now().UTC(),
		Payload:        payload,
	}
}

// publicRun removes request-scoped binary input before a run is emitted, returned
// to the UI, or retained after reaching a terminal state.
func publicRun(run domain.Run) domain.Run {
	run.Images = nil
	return run
}
