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

// ErrApprovalDeclined 表示工具调用被用户拒绝审批。
var ErrApprovalDeclined = errors.New("tool execution approval declined")

// EventSink 接收诊断事件并推送给 UI 客户端，用于实时展示运行进度。
type EventSink interface {
	Emit(event domain.Event)
}

// RunJournal 定义诊断运行在 SQLite 中的持久化接口：保存 run、事件、
// 上下文快照、上下文事实、诊断计划、工具指纹与 token 用量等。
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
	SaveTokenUsage(record domain.TokenUsageRecord) error
	TokenUsageByConversation(conversationID string) ([]domain.TokenUsageRecord, error)
}

// ApprovalHandler 处理工具调用的审批流程：弹出审批请求、等待用户决定，
// 并记忆会话级授权以跳过后续重复审批。
type ApprovalHandler interface {
	Open(request domain.ApprovalRequest) error
	Wait(ctx context.Context, callID string) (bool, error)
	Resolve(callID string, approved bool, approveConversation bool) error
	ConversationApproved(conversationID string) bool
}

// Runner 是诊断运行的执行器：负责工具准备、策略评估、审批、并发闸门、
// 只读结果复用，以及事件与证据的记录。
type Runner struct {
	registry *tools.Registry
	policy   *policy.Engine
	journal  RunJournal
	sink     EventSink
	approval ApprovalHandler

	mu            sync.RWMutex
	runs          map[string]domain.Run
	terminalMu    sync.Mutex
	toolExecution sync.RWMutex
	evidenceMu    sync.RWMutex
	reusable      map[string]domain.Evidence
}

type readOnlyExecutionKey struct{}
type teamWorkerExecutionKey struct{}

type teamWorkerExecution struct {
	StepID string
	Role   string
}

// WithReadOnlyExecution 将嵌套代理执行标记为"仅取证"模式。该守卫在工具
// 准备之后基于宿主派生的注解强制执行，因此模型无法通过修改提示词或
// 参数来绕过它。
func WithReadOnlyExecution(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyExecutionKey{}, true)
}

// WithTeamWorkerExecution 为工具与证据事件标记其归属的 worker（步骤与角色）。
// 该元数据由编排层持有，因此并发 worker 事件可被可靠分组，
// 无需从时间戳推断归属。
func WithTeamWorkerExecution(ctx context.Context, stepID, role string) context.Context {
	ctx = WithReadOnlyExecution(ctx)
	return context.WithValue(ctx, teamWorkerExecutionKey{}, teamWorkerExecution{
		StepID: strings.TrimSpace(stepID),
		Role:   strings.TrimSpace(role),
	})
}

// NewRunner 创建 Runner，注入工具注册表、策略引擎、持久化日志、事件推送
// 与审批处理器。
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

// Start 创建并启动一个新的诊断 run：规范化模式、持久化 run 与启动事件，
// 并清空跨 run 的只读证据已读记录。尚未被模型处理的用户输入只属于
// 当前 run，不得在这里覆盖上一轮已经建立的 confirmed facts / plan。
func (r *Runner) Start(goal, profileID, conversationID string, mode domain.DiagnosticMode, images ...domain.ImageAttachment) (domain.Run, error) {
	normalizedMode, err := domain.NormalizeDiagnosticMode(mode)
	if err != nil {
		return domain.Run{}, err
	}
	run := domain.Run{
		ID:             uuid.NewString(),
		ConversationID: conversationID,
		ProfileID:      profileID,
		Goal:           goal,
		Mode:           normalizedMode,
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
	r.emit(event)
	return publicRun(run), nil
}

// Fail 将运行中的 run 标记为失败，并持久化失败事件与错误信息。
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

// PublishAgentMessage 发布一条 agent 的文本消息事件。
func (r *Runner) PublishAgentMessage(runID, content string) error {
	return r.publish(runID, domain.EventAgentMessage, map[string]string{"content": content})
}

// PublishAgentMessageWithID 持久化一条已经完成的流式消息。messageId 只用于让
// 前端用完整消息替换同一张临时卡片，不参与对话上下文或 checkpoint。
func (r *Runner) PublishAgentMessageWithID(runID, messageID, content string) error {
	return r.publish(runID, domain.EventAgentMessage, map[string]string{
		"messageId": messageID,
		"content":   content,
	})
}

// EmitAgentMessageStarted/Delta 只推送到实时 UI，不写入 journal。SQLite 仍然只
// 保存完成的 agent.message，避免重放会话时出现大量 token 级事件或半截消息。
func (r *Runner) EmitAgentMessageStarted(runID, messageID string) error {
	return r.emitTransient(runID, domain.EventAgentMessageStarted, map[string]string{"messageId": messageID})
}

func (r *Runner) EmitAgentMessageDelta(runID, messageID, delta string) error {
	return r.emitTransient(runID, domain.EventAgentMessageDelta, map[string]string{
		"messageId": messageID,
		"delta":     delta,
	})
}

// PublishAgentReasoning 发布模型明确返回且已经过调用方脱敏、截断的调试推理。
// Team worker 的归属由宿主 context 注入，模型不能自行伪造 worker 标识。
func (r *Runner) PublishAgentReasoning(ctx context.Context, runID, stage, content string, reasoningTokens int) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	payload := addTeamWorkerEventFields(ctx, map[string]any{
		"stage":   strings.TrimSpace(stage),
		"content": content,
	})
	if reasoningTokens > 0 {
		payload["reasoningTokens"] = reasoningTokens
	}
	return r.publish(runID, domain.EventAgentReasoning, payload)
}

// RecordModelHTTPTrace 只把脱敏后的模型传输元数据写入 SQLite，不推送到 UI。
// 观测失败必须由调用方按 best-effort 处理，不能改变真实模型调用的结果。
func (r *Runner) RecordModelHTTPTrace(ctx context.Context, runID string, payload map[string]any) error {
	run, exists := r.Run(runID)
	if !exists {
		return fmt.Errorf("run not found: %s", runID)
	}
	payload = addTeamWorkerEventFields(ctx, payload)
	return r.journal.Append(newRunEvent(run, domain.EventModelHTTPTrace, payload))
}

// PublishTeamEvent 以父 run 为归属持久化一条结构化编排事件。
// worker 从不绕过父 Runner，因此 UI 与恢复日志看到的是单一有序的证据轨迹。
func (r *Runner) PublishTeamEvent(runID string, eventType domain.EventType, payload any) error {
	switch eventType {
	case domain.EventTeamPlanned, domain.EventTeamStepStarted, domain.EventTeamStepFinished:
		return r.publish(runID, eventType, payload)
	default:
		return fmt.Errorf("unsupported team event type: %s", eventType)
	}
}

// Run 返回内存中指定 runID 的当前运行状态。
func (r *Runner) Run(runID string) (domain.Run, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	run, exists := r.runs[runID]
	return run, exists
}

// Context 返回指定会话最近保存的上下文快照。
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

// ContextFacts 返回指定会话的上下文事实列表（按最近出现时间倒序）。
func (r *Runner) ContextFacts(conversationID string, limit int) ([]domain.ContextFact, error) {
	return r.journal.ContextFacts(conversationID, limit)
}

// SaveContextFact 持久化一条上下文事实（持久记忆）。
func (r *Runner) SaveContextFact(fact domain.ContextFact) error {
	return r.journal.SaveContextFact(fact)
}

// Plan 返回指定会话最近保存的诊断计划。
func (r *Runner) Plan(conversationID string) (domain.DiagnosticPlan, bool, error) {
	return r.journal.Plan(conversationID)
}

// SavePlan 持久化或更新诊断计划。
func (r *Runner) SavePlan(plan domain.DiagnosticPlan) error {
	return r.journal.SavePlan(plan)
}

// ToolFingerprint 查询指定会话与指纹对应的工具调用记录。
func (r *Runner) ToolFingerprint(conversationID, fingerprint string) (domain.ToolFingerprint, bool, error) {
	return r.journal.ToolFingerprint(conversationID, fingerprint)
}

// SaveTokenUsage 持久化一条由 agent 引擎 token 用量中间件采集的
// 单次模型生成用量记录。
func (r *Runner) SaveTokenUsage(record domain.TokenUsageRecord) error {
	return r.journal.SaveTokenUsage(record)
}

// TokenUsageByConversation 返回每轮 token 用量与耗时记录。
func (r *Runner) TokenUsageByConversation(conversationID string) ([]domain.TokenUsageRecord, error) {
	return r.journal.TokenUsageByConversation(conversationID)
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

// Execute 是"单次工具调用"的执行管控入口，被 Eino 的工具调用(excutor)节点调用。
// 它把"预检 → 守卫 → 事件 → 策略 → 并发 → 审批 → 缓存复用 → 真执行 → 证据 → 记忆 → 缓存刷新"
// 这一整套生命周期全部串起来。返回值是本次调用捕获的 Evidence（完整证据）。
func (r *Runner) Execute(ctx context.Context, runID string, call domain.ToolCall) (domain.Evidence, error) {
	startedAt := time.Now()     // 记下开始时刻，用于事件里的 elapsedMs（耗时）统计
	run, exists := r.Run(runID) // ① 按 runID 取出当前运行对象
	if !exists {                // 这个 run 不存在
		return domain.Evidence{}, fmt.Errorf("run not found: %s", runID)
	}
	if run.Status != domain.RunRunning { // run 存在但不在 running 状态
		return domain.Evidence{}, fmt.Errorf("run is not active: %s", run.Status) // 拒绝执行
	}

	tool, prepared, err := r.registry.Prepare(call) // ② Prepare：从注册表解析出可执行 tool + 校验后的 prepared 上下文
	if err != nil {                                 // Prepare 失败（工具名未知/参数不合法等）
		if eventErr := r.failTool(ctx, runID, call, err); eventErr != nil { // 发 tool.failed 事件本身也出错
			return domain.Evidence{}, errors.Join(err, eventErr) // 合并两个错误返回
		}
		return domain.Evidence{}, err // 事件发成功，但本次调用失败
	}
	if readOnly, _ := ctx.Value(readOnlyExecutionKey{}).(bool); readOnly && !prepared.Annotations.ReadOnly { // ③ 只读守卫：当前是只读 ctx 且工具非只读
		err = fmt.Errorf("team worker is read-only; tool call %s would change or control external state", call.Name) // 构造拦截原因
		if eventErr := r.failTool(ctx, runID, call, err); eventErr != nil {                                          // 同样走 failTool 兜底
			return domain.Evidence{}, errors.Join(err, eventErr)
		}
		return domain.Evidence{}, err // 拦截：只读 worker 不允许改状态的工具
	}
	publicCall := map[string]any{ // 组装"对外公开"的工具调用描述（不含内部 client 等敏感字段）
		"callId":      call.ID,              // 本次调用的唯一 ID
		"name":        call.Name,            // 工具名
		"summary":     prepared.Summary,     // Prepare 时算好的一句话摘要
		"annotations": prepared.Annotations, // 工具注解（ReadOnly/Idempotent 等），供前端展示
	}
	addTeamWorkerEventFields(ctx, publicCall)                                     // 若在 Team worker ctx 里，给 publicCall 附上 step_id/role
	if err = r.publish(runID, domain.EventToolProposed, publicCall); err != nil { // ④ 发布"提议执行"事件
		return domain.Evidence{}, err
	}

	decision := r.policy.Evaluate(prepared) // ⑤ 策略评估：该工具+参数是否允许、是否需审批
	if !decision.Allowed {                  // 策略直接不允许
		err = errors.New(decision.Reason)                                   // 用决策理由构造错误
		if eventErr := r.failTool(ctx, runID, call, err); eventErr != nil { // 发 tool.failed 事件
			return domain.Evidence{}, errors.Join(err, eventErr)
		}
		return domain.Evidence{}, err // 策略拒绝：不执行
	}

	// Eino may execute independent tool calls concurrently. Keep the
	// explicitly supported read-only tools in a shared read section, while
	// writes, shell commands, sensitive reads, and unknown tools take the
	// exclusive section. The gate is acquired before approval so concurrent
	// state-changing calls cannot create multiple approval prompts or overlap
	// after approval.
	releaseToolExecution := r.acquireToolExecution(prepared) // ⑥ 并发闸门：按读写类型加共享读锁或互斥锁
	defer releaseToolExecution()                             // 函数返回时无论成败都自动解锁

	if decision.ApprovalRequired { // ⑦ 需要人工审批才进这个分支
		if r.approval != nil && r.approval.ConversationApproved(run.ConversationID) { // 本会话已被用户整会话授权过
			if err = r.publish(runID, domain.EventApprovalResolved, map[string]any{ // 就此自动放行
				"callId":    call.ID,
				"approved":  true,
				"automatic": true, // 标记"自动通过"（因会话级授权）
				"scope":     "conversation",
			}); err != nil {
				return domain.Evidence{}, err
			}
		} else { // 没有会话级授权 → 逐次弹审批请求等用户
			request := domain.ApprovalRequest{ // 组装审批请求体
				ConversationID: run.ConversationID,   // 归属会话
				RunID:          runID,                // 归属 run
				CallID:         call.ID,              // 本次调用 ID
				ToolName:       call.Name,            // 工具名（展示给用户）
				Summary:        prepared.Summary,     // 一句话说明做什么
				Reason:         decision.Reason,      // 为什么需要审批
				Preview:        prepared.Preview,     // 预览（如 XML/命令）
				Annotations:    prepared.Annotations, // 注解
			}
			if r.approval == nil { // 没配审批 handler
				err = errors.New("approval handler is not configured") // 无法审批
				if eventErr := r.failTool(ctx, runID, call, err); eventErr != nil {
					return domain.Evidence{}, errors.Join(err, eventErr)
				}
				return domain.Evidence{}, err
			}
			if err = r.approval.Open(request); err != nil { // 把审批请求打开（前端弹出）
				if eventErr := r.failTool(ctx, runID, call, err); eventErr != nil {
					return domain.Evidence{}, errors.Join(err, eventErr)
				}
				return domain.Evidence{}, err
			}
			if err = r.publish(runID, domain.EventApprovalRequired, request); err != nil { // 发布"需要审批"事件
				_ = r.approval.Resolve(call.ID, false, false) // 发布失败 → 兜底：把该请求回滚为拒绝
				return domain.Evidence{}, err
			}
			approved, approvalErr := r.approval.Wait(ctx, call.ID) // 阻塞等待用户点"通过/拒绝"
			if approvalErr != nil {                                // 等审批过程自身出错
				if eventErr := r.failTool(ctx, runID, call, approvalErr); eventErr != nil {
					return domain.Evidence{}, errors.Join(approvalErr, eventErr)
				}
				return domain.Evidence{}, approvalErr
			}
			if !approved { // 用户点了"拒绝"
				if err = r.publish(runID, domain.EventApprovalResolved, map[string]any{"callId": call.ID, "approved": false}); err != nil {
					return domain.Evidence{}, err
				}
				if eventErr := r.failTool(ctx, runID, call, ErrApprovalDeclined); eventErr != nil { // 记为被拒
					return domain.Evidence{}, errors.Join(ErrApprovalDeclined, eventErr)
				}
				return domain.Evidence{}, ErrApprovalDeclined // 返回"审批被拒"错误
			}
			scope := "once"                                          // 默认本次一次性授权
			if r.approval.ConversationApproved(run.ConversationID) { // 若用户在批准时勾选了"本会话都允许"
				scope = "conversation" // 标记为会话级授权
			}
			if err = r.publish(runID, domain.EventApprovalResolved, map[string]any{"callId": call.ID, "approved": true, "scope": scope}); err != nil { // 发布"已批准"
				return domain.Evidence{}, err
			}
		}
	}
	fingerprint := toolCallFingerprint(call) // ⑧ 计算本次调用的指纹（工具名+参数 的 SHA-256）
	if canReuseToolResult(prepared) {        // 仅"安全只读"工具才尝试复用缓存
		reuseKey := reusableEvidenceKey(run, fingerprint) // 本会话+目标+指纹 组成内存缓存 key
		r.evidenceMu.RLock()                              // 读缓存加读锁
		reused, found := r.reusable[reuseKey]             // 内存缓存里查
		r.evidenceMu.RUnlock()
		if found { // 内存命中 → 直接复用旧证据，不再执行
			if err = r.publish(runID, domain.EventToolStarted, publicCall); err != nil { // 仍发布 started
				return domain.Evidence{}, err
			}
			if err = r.publish(runID, domain.EventToolCompleted, addTeamWorkerEventFields(ctx, map[string]any{
				"callId": call.ID, "name": call.Name, "successful": true,
				"summary": prepared.Summary, "evidenceId": reused.ID, "reused": true, // 标记 reused=true
				"elapsedMs": time.Since(startedAt).Milliseconds(),
			})); err != nil {
				return domain.Evidence{}, err
			}
			return reused, nil // 直接返回复用到的证据
		}
		// 内存缓存未命中：查 SQLite 的 tool_fingerprints 表，看同 conversation 是否
		// 之前 Run 跑过相同的只读调用。命中后通过 evidence_id 直接拿回旧结果，
		// 避免重新发 NBI/NETCONF 请求或重新扫文件。这对"重开会话继续问"场景特别有用：
		// 用户重启应用后，in-memory 缓存清空，但 SQLite 里的 fingerprint 还在。
		//
		// 安全性：只对 canReuseToolResult 返回 true 的工具复用（ReadOnly+Idempotent+!Destructive+!Sensitive）。
		// 设备状态会随时间变化，但同 conversation 的连续对话通常在短时间内发生，
		// 复用上一次查询结果是可接受的（用户想刷新可以换一个查询参数，fingerprint 就不同）。
		storedRecord, storedFound, storedErr := r.journal.ToolFingerprint(run.ConversationID, fingerprint) // 查 SQLite 指纹
		if storedErr == nil && storedFound && storedRecord.Successful && storedRecord.EvidenceID != "" {   // 命中且上次成功、有证据ID
			storedEvidence, evidenceFound, evidenceErr := r.journal.EvidenceByConversation(run.ConversationID, storedRecord.EvidenceID) // 按 evidence_id 从 events 表取完整证据
			if evidenceErr == nil && evidenceFound {                                                                                    // 证据也取到了
				// 把跨 Run 复用的 evidence 也填进内存缓存，避免后续命中还要再查 SQLite。
				r.evidenceMu.Lock()
				r.reusable[reuseKey] = storedEvidence // 回填内存缓存加速后续命中
				r.evidenceMu.Unlock()
				if err = r.publish(runID, domain.EventToolStarted, publicCall); err != nil {
					return domain.Evidence{}, err
				}
				if err = r.publish(runID, domain.EventToolCompleted, addTeamWorkerEventFields(ctx, map[string]any{
					"callId": call.ID, "name": call.Name, "successful": true,
					"summary": prepared.Summary, "evidenceId": storedEvidence.ID, "reused": true, "source": "sqlite", // 来源标记 SQLite
					"elapsedMs": time.Since(startedAt).Milliseconds(),
				})); err != nil {
					return domain.Evidence{}, err
				}
				return storedEvidence, nil // 返回复用的证据
			}
		}
	}

	if err = r.publish(runID, domain.EventToolStarted, publicCall); err != nil { // ⑨ 宣布真正开始执行
		return domain.Evidence{}, err
	}
	result, err := tool.Execute(ctx, prepared) // 真正执行工具（连设备/发请求/扫文件）
	if err != nil {                            // 执行失败
		if eventErr := r.failTool(ctx, runID, call, err); eventErr != nil { // 发 tool.failed + 记 error 指纹
			return domain.Evidence{}, errors.Join(err, eventErr)
		}
		return domain.Evidence{}, err
	}

	evidence := domain.Evidence{ // ⑩ 构造本次调用捕获的证据
		ID:         uuid.NewString(), // 全新唯一 evidence id
		RunID:      runID,            // 归属 run
		CallID:     call.ID,          // 归属调用
		Kind:       call.Name,        // 工具名
		Summary:    result.Summary,   // 工具返回的摘要
		Data:       result.Data,      // 完整正文（data.content 等）
		Message:    result.Message,   // 附加消息
		Metadata:   result.Metadata,  // 元信息
		CapturedAt: time.Now().UTC(), // 捕获时间
	}
	if worker, ok := teamWorkerFromContext(ctx); ok { // 若在 Team worker 上下文中
		if evidence.Metadata == nil { // 没有元信息则先建一个
			evidence.Metadata = make(map[string]string, 2)
		}
		evidence.Metadata["team.worker_step_id"] = worker.StepID // 记录归属 worker 步骤
		evidence.Metadata["team.worker_role"] = worker.Role      // 记录归属 worker 角色
	}
	if err = r.publish(runID, domain.EventEvidenceCaptured, evidence); err != nil { // 发布"抓到证据"事件 → events 表
		return domain.Evidence{}, err
	}
	if err = r.publish(runID, domain.EventToolCompleted, addTeamWorkerEventFields(ctx, map[string]any{ // 发布"工具完成"
		"callId":     call.ID,
		"name":       call.Name,
		"successful": true,
		"summary":    prepared.Summary,
		"evidenceId": evidence.ID, // 本次证据 id
		"elapsedMs":  time.Since(startedAt).Milliseconds(),
	})); err != nil {
		return domain.Evidence{}, err
	}
	if err = r.recordToolMemory(run, call, prepared, evidence); err != nil { // ⑪ 沉淀长期记忆 fact(含 evidenceId)
		return domain.Evidence{}, err
	}
	if canReuseToolResult(prepared) { // 只读工具执行成功 → 缓存本次证据供下次复用
		reuseKey := reusableEvidenceKey(run, fingerprint)
		r.evidenceMu.Lock()
		r.reusable[reuseKey] = evidence // 写进内存缓存
		r.evidenceMu.Unlock()
	}
	// 写入类工具（POST/PUT/PATCH/DELETE/edit-config/commit 等）成功后，必须清空
	// 该 conversation 的所有只读缓存。否则压缩后模型重新 GET 同一 URL 会命中旧缓存，
	// 返回写入前的快照，导致"写入成功但验证看不到结果"的幻觉。
	// canReuseToolResult 只对 ReadOnly+Idempotent 工具返回 true，写入工具不在此列，
	// 所以这里用 !ReadOnly 作为"写入类"的判据。
	if !prepared.Annotations.ReadOnly { // ⑫ 写工具成功 → 作废整个会话的只读缓存，防脏读
		r.invalidateReusableCache(run.ConversationID)
	}
	return evidence, nil // 返回捕获的证据（成功）
}

// MCPToolInfos returns the minimal description of every registered MCP tool so
// the agent engine can build dynamic Eino tools with the correct JSON schema.
func (r *Runner) MCPToolInfos() []tools.MCPToolInfo {
	return r.registry.MCPToolInfos()
}

// MCPToolInfosForRole returns locally authorized read-only MCP tools for one
// isolated Team worker role.
func (r *Runner) MCPToolInfosForRole(role string) []tools.MCPToolInfo {
	return r.registry.MCPToolInfosForRole(role)
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
	r.terminalMu.Lock()
	defer r.terminalMu.Unlock()
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

func (r *Runner) failTool(ctx context.Context, runID string, call domain.ToolCall, err error) error {
	if publishErr := r.publish(runID, domain.EventToolFailed, addTeamWorkerEventFields(ctx, map[string]any{
		"callId":     call.ID,
		"name":       call.Name,
		"successful": false,
		"error":      err.Error(),
	})); publishErr != nil {
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

func teamWorkerFromContext(ctx context.Context) (teamWorkerExecution, bool) {
	worker, ok := ctx.Value(teamWorkerExecutionKey{}).(teamWorkerExecution)
	return worker, ok && worker.StepID != ""
}

func addTeamWorkerEventFields(ctx context.Context, payload map[string]any) map[string]any {
	if worker, ok := teamWorkerFromContext(ctx); ok {
		payload["workerStepId"] = worker.StepID
		payload["workerRole"] = worker.Role
	}
	return payload
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

func (r *Runner) emitTransient(runID string, eventType domain.EventType, payload any) error {
	run, exists := r.Run(runID)
	if !exists {
		return fmt.Errorf("run not found: %s", runID)
	}
	r.emit(newRunEvent(run, eventType, payload))
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
