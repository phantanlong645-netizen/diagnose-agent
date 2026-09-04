package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RunStatus 表示诊断 run 的生命周期状态。
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// DiagnosticMode 选择单次 run 的编排策略：Agent 保持原有单一 ReAct 循环；
// Team 将复杂排查分发给隔离的只读 worker，并通过 DAG 汇总它们的证据。
type DiagnosticMode string

const (
	DiagnosticModeAgent  DiagnosticMode = "agent"
	DiagnosticModeTeam   DiagnosticMode = "team"
	DiagnosticModeManual DiagnosticMode = "manual"
)

// NormalizeDiagnosticMode 规范化诊断模式字符串：空串视为 agent，转为小写
// 后与三个合法值匹配，其余返回错误。
func NormalizeDiagnosticMode(mode DiagnosticMode) (DiagnosticMode, error) {
	switch DiagnosticMode(strings.ToLower(strings.TrimSpace(string(mode)))) {
	case "", DiagnosticModeAgent:
		return DiagnosticModeAgent, nil
	case DiagnosticModeTeam:
		return DiagnosticModeTeam, nil
	case DiagnosticModeManual:
		return DiagnosticModeManual, nil
	default:
		return "", fmt.Errorf("unsupported diagnostic mode: %s", mode)
	}
}

// EventType 表示诊断事件流中的事件类别（run、tool、approval、team 等）。
type EventType string

const (
	EventRunStarted       EventType = "run.started"
	EventAgentMessage     EventType = "agent.message"
	EventToolProposed     EventType = "tool.proposed"
	EventApprovalRequired EventType = "approval.required"
	EventApprovalResolved EventType = "approval.resolved"
	EventToolStarted      EventType = "tool.started"
	EventEvidenceCaptured EventType = "evidence.captured"
	EventToolCompleted    EventType = "tool.completed"
	EventToolFailed       EventType = "tool.failed"
	EventFindingProduced  EventType = "finding.produced"
	EventTeamPlanned      EventType = "team.planned"
	EventTeamStepStarted  EventType = "team.step.started"
	EventTeamStepFinished EventType = "team.step.finished"
	EventRunCompleted     EventType = "run.completed"
	EventRunFailed        EventType = "run.failed"
	EventRunCancelled     EventType = "run.cancelled"
)

// ToolAnnotations 描述工具调用的安全特性，供策略引擎与并发/缓存控制使用。
type ToolAnnotations struct {
	ReadOnly    bool `json:"readOnly"`
	Destructive bool `json:"destructive"`
	Idempotent  bool `json:"idempotent"`
	OpenWorld   bool `json:"openWorld"`
	Sensitive   bool `json:"sensitive"`
}

// ToolCall 是 agent 对一次工具调用的请求描述。
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// PreparedCall 是完成校验与注解后的工具调用，附带面向模型和 UI 的摘要与预览。
type PreparedCall struct {
	ToolCall
	Summary     string          `json:"summary"`
	Preview     string          `json:"-"`
	Annotations ToolAnnotations `json:"annotations"`
	Input       any             `json:"-"`
}

// ToolResult 是工具执行成功后的输出，包含摘要、数据与可选元信息。
type ToolResult struct {
	Summary  string            `json:"summary"`
	Data     any               `json:"data,omitempty"`
	Message  string            `json:"message,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ContextFact 是持久的诊断记忆。与聊天消息不同，事实按键可替换，
// 因此上下文压缩无需保留重复的叙述，也能保留精确的路径、错误或证据引用。
type ContextFact struct {
	ConversationID string    `json:"conversationId"`
	Key            string    `json:"key"`
	Kind           string    `json:"kind"`
	Status         string    `json:"status"`
	Content        string    `json:"content"`
	EvidenceIDs    []string  `json:"evidenceIds,omitempty"`
	SourceTool     string    `json:"sourceTool,omitempty"`
	SourceLocator  string    `json:"sourceLocator,omitempty"`
	Confidence     float64   `json:"confidence"`
	FirstSeen      time.Time `json:"firstSeen"`
	LastSeen       time.Time `json:"lastSeen"`
	Revision       int       `json:"revision"`
}

// DiagnosticPlan 记录诊断进度：已完成检查、未解决问题与下一步动作，
// 供恢复会话时继续推进。
type DiagnosticPlan struct {
	ConversationID      string    `json:"conversationId"`
	Goal                string    `json:"goal"`
	Target              string    `json:"target,omitempty"`
	CompletedChecks     []string  `json:"completedChecks,omitempty"`
	UnresolvedQuestions []string  `json:"unresolvedQuestions,omitempty"`
	NextAction          string    `json:"nextAction,omitempty"`
	DoNotRepeat         []string  `json:"doNotRepeat,omitempty"`
	Revision            int       `json:"revision"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

// ToolFingerprint 记录一次工具调用的指纹与对应证据 ID，用于同会话内
// 跨 run 的只读结果复用。
type ToolFingerprint struct {
	ConversationID string    `json:"conversationId"`
	Fingerprint    string    `json:"fingerprint"`
	ToolName       string    `json:"toolName"`
	EvidenceID     string    `json:"evidenceId,omitempty"`
	Successful     bool      `json:"successful"`
	LastSeen       time.Time `json:"lastSeen"`
}

// TokenUsageRecord 记录单次模型调用的 token 消耗与耗时，用于"每轮输入/输出/耗时"
// 统计。它按 run 与 conversation 双维度归属，既能在诊断过程中累计，也能在事后查询。
type TokenUsageRecord struct {
	ID             string    `json:"id"`
	RunID          string    `json:"runId"`
	ConversationID string    `json:"conversationId"`
	Iteration      int       `json:"iteration"`
	InputTokens    int       `json:"inputTokens"`
	OutputTokens   int       `json:"outputTokens"`
	TotalTokens    int       `json:"totalTokens"`
	ElapsedMS      int64     `json:"elapsedMs"`
	RecordedAt     time.Time `json:"recordedAt"`
}

// ApprovalRequest 描述一次需要用户审批的工具调用。
type ApprovalRequest struct {
	ConversationID string          `json:"conversationId"`
	RunID          string          `json:"runId"`
	CallID         string          `json:"callId"`
	ToolName       string          `json:"toolName"`
	Summary        string          `json:"summary"`
	Reason         string          `json:"reason"`
	Preview        string          `json:"preview,omitempty"`
	Annotations    ToolAnnotations `json:"annotations"`
}

// Evidence 是工具执行捕获的证据：携带原始数据、摘要与遥测元信息。
type Evidence struct {
	ID         string            `json:"id"`
	RunID      string            `json:"runId"`
	CallID     string            `json:"callId"`
	Kind       string            `json:"kind"`
	Summary    string            `json:"summary"`
	Data       any               `json:"data,omitempty"`
	Message    string            `json:"message,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	CapturedAt time.Time         `json:"capturedAt"`
}

// Finding 表示一条诊断结论，关联支撑它的证据 ID 列表。
type Finding struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Severity    string   `json:"severity"`
	Summary     string   `json:"summary"`
	EvidenceIDs []string `json:"evidenceIds"`
}

// Event 是持久化与推送的事件日志条目。
type Event struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversationId"`
	RunID          string    `json:"runId"`
	Type           EventType `json:"type"`
	Timestamp      time.Time `json:"timestamp"`
	Payload        any       `json:"payload,omitempty"`
}

// Run 是一次诊断执行会话的状态与元数据。
type Run struct {
	ID             string            `json:"id"`
	ConversationID string            `json:"conversationId"`
	ProfileID      string            `json:"profileId"`
	Goal           string            `json:"goal"`
	Mode           DiagnosticMode    `json:"mode"`
	Images         []ImageAttachment `json:"images,omitempty"`
	Status         RunStatus         `json:"status"`
	StartedAt      time.Time         `json:"startedAt"`
	EndedAt        *time.Time        `json:"endedAt,omitempty"`
}

// ImageAttachment 表示用户上传的图片附件，用于多模态消息。
// 图片以 base64 编码的 data URI 传给模型，模型可直接"看"图片内容。
type ImageAttachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mimeType"` // 如 "image/png"
	Data     string `json:"data"`     // base64 编码（不含 data: 前缀）
}

// ConversationStatus 表示会话生命周期状态（active/archived）。
type ConversationStatus string

const (
	ConversationActive   ConversationStatus = "active"
	ConversationArchived ConversationStatus = "archived"
)

// Conversation 是按目标档案隔离的一次连续诊断会话。
type Conversation struct {
	ID        string             `json:"id"`
	ProfileID string             `json:"profileId"`
	Title     string             `json:"title"`
	Status    ConversationStatus `json:"status"`
	CreatedAt time.Time          `json:"createdAt"`
	UpdatedAt time.Time          `json:"updatedAt"`
}

// DiagnosticRequest 是前端发起一次诊断的请求载荷。
type DiagnosticRequest struct {
	Goal        string         `json:"goal"`
	ProfileID   string         `json:"profileId"`
	Mode        DiagnosticMode `json:"mode,omitempty"`
	Attachments []Attachment   `json:"attachments,omitempty"`
}

// ManualDraftMessage is one turn in the short-lived AI request-builder chat.
// It intentionally contains only operator text, never target credentials or
// live device responses.
type ManualDraftMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ManualDraftRequest asks the model to construct a request for the existing
// Manual form. Drafting is separate from executing a tool call.
type ManualDraftRequest struct {
	ProfileID string               `json:"profileId"`
	Messages  []ManualDraftMessage `json:"messages"`
}

// ManualDraft mirrors the fields already accepted by the Manual NBI and
// NETCONF forms. Exactly one protocol shape is populated according to Kind.
type ManualDraft struct {
	Kind           string            `json:"kind"`
	Method         string            `json:"method,omitempty"`
	Path           string            `json:"path,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Body           string            `json:"body,omitempty"`
	Endpoint       string            `json:"endpoint,omitempty"`
	RPC            string            `json:"rpc,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
}

// ManualDraftResponse is returned after the model output has passed local
// structural validation. Validation never authenticates, opens SSH, or calls
// the target device.
type ManualDraftResponse struct {
	Message          string      `json:"message"`
	Draft            ManualDraft `json:"draft"`
	Valid            bool        `json:"valid"`
	ValidationErrors []string    `json:"validationErrors,omitempty"`
}

// Attachment 表示用户上传的附件文件。前端将文件内容 base64 编码后随诊断请求一起发送。
// 后端按 MIME 类型分类处理：文本/PDF 提取内容注入 goal，图片/二进制仅记录文件名。
type Attachment struct {
	Filename string `json:"filename"` // 原始文件名，如 "error.log"
	MimeType string `json:"mimeType"` // MIME 类型，如 "text/plain"、"application/pdf"
	Data     string `json:"data"`     // base64 编码的文件内容
}

type ModelSettings struct {
	BaseURL             string `json:"baseUrl,omitempty"`
	APIKey              string `json:"apiKey,omitempty"`
	Model               string `json:"model"`
	TimeoutSeconds      int    `json:"timeoutSeconds,omitempty"`
	ContextWindowTokens int    `json:"contextWindowTokens,omitempty"`
	OutputReserveTokens int    `json:"outputReserveTokens,omitempty"`
}

type ModelSettingsSummary struct {
	BaseURL             string `json:"baseUrl,omitempty"`
	Model               string `json:"model"`
	TimeoutSeconds      int    `json:"timeoutSeconds"`
	ContextWindowTokens int    `json:"contextWindowTokens"`
	OutputReserveTokens int    `json:"outputReserveTokens"`
	Configured          bool   `json:"configured"`
	APIKeyConfigured    bool   `json:"apiKeyConfigured"`
}

type AgentReadiness struct {
	Ready              bool     `json:"ready"`
	TargetConfigured   bool     `json:"targetConfigured"`
	ModelConfigured    bool     `json:"modelConfigured"`
	BuiltinSkillLoaded bool     `json:"builtinSkillLoaded"`
	ExternalSkillCount int      `json:"externalSkillCount"`
	ToolNames          []string `json:"toolNames"`
	Issues             []string `json:"issues,omitempty"`
}

type NBITarget struct {
	BaseURL     string `json:"baseUrl"`
	OLTAddress  string `json:"oltAddress,omitempty"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	TokenHeader string `json:"tokenHeader,omitempty"`
	Token       string `json:"token,omitempty"`
	CAFile      string `json:"caFile,omitempty"`
	InsecureTLS bool   `json:"insecureTls"`
}

type NETCONFTarget struct {
	Address         string `json:"address"`
	Port            int    `json:"port"`
	Username        string `json:"username"`
	Password        string `json:"password,omitempty"`
	KnownHostsFile  string `json:"knownHostsFile,omitempty"`
	InsecureHostKey bool   `json:"insecureHostKey"`
}

// NETCONFEndpoint identifies one logical SSH/NETCONF entry point on an OLT
// chassis (for example ihub, nt, lt1, or lt2).
type NETCONFEndpoint struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	Address         string `json:"address"`
	Port            int    `json:"port"`
	Username        string `json:"username"`
	Password        string `json:"password,omitempty"`
	KnownHostsFile  string `json:"knownHostsFile,omitempty"`
	InsecureHostKey bool   `json:"insecureHostKey"`
}

type NETCONFEndpointSummary struct {
	ID                 string `json:"id"`
	Name               string `json:"name,omitempty"`
	Address            string `json:"address,omitempty"`
	Port               int    `json:"port,omitempty"`
	Username           string `json:"username,omitempty"`
	KnownHostsFile     string `json:"knownHostsFile,omitempty"`
	InsecureHostKey    bool   `json:"insecureHostKey"`
	PasswordConfigured bool   `json:"passwordConfigured"`
}

type TargetProfile struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	OrganizationCode string            `json:"organizationCode,omitempty"`
	NBI              *NBITarget        `json:"nbi,omitempty"`
	NETCONF          *NETCONFTarget    `json:"netconf,omitempty"`
	NETCONFEndpoints []NETCONFEndpoint `json:"netconfEndpoints,omitempty"`
	WorkspaceRoots   []string          `json:"workspaceRoots,omitempty"`
	SkillPaths       []string          `json:"skillPaths,omitempty"`
}

type TargetProfileSummary struct {
	ID                        string                   `json:"id"`
	Name                      string                   `json:"name"`
	OrganizationCode          string                   `json:"organizationCode,omitempty"`
	NBIBaseURL                string                   `json:"nbiBaseUrl,omitempty"`
	OLTAddress                string                   `json:"oltAddress,omitempty"`
	NBIUsername               string                   `json:"nbiUsername,omitempty"`
	TokenHeader               string                   `json:"tokenHeader,omitempty"`
	CAFile                    string                   `json:"caFile,omitempty"`
	InsecureTLS               bool                     `json:"insecureTls"`
	NBITokenConfigured        bool                     `json:"nbiTokenConfigured"`
	NBIPasswordConfigured     bool                     `json:"nbiPasswordConfigured"`
	NETCONFAddress            string                   `json:"netconfAddress,omitempty"`
	NETCONFPort               int                      `json:"netconfPort,omitempty"`
	NETCONFUsername           string                   `json:"netconfUsername,omitempty"`
	KnownHostsFile            string                   `json:"knownHostsFile,omitempty"`
	InsecureHostKey           bool                     `json:"insecureHostKey"`
	NETCONFPasswordConfigured bool                     `json:"netconfPasswordConfigured"`
	NETCONFEndpoints          []NETCONFEndpointSummary `json:"netconfEndpoints,omitempty"`
	WorkspaceRoots            []string                 `json:"workspaceRoots,omitempty"`
	SkillPaths                []string                 `json:"skillPaths,omitempty"`
}
