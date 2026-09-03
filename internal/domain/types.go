package domain

import (
	"encoding/json"
	"time"
)

type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

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
	EventRunCompleted     EventType = "run.completed"
	EventRunFailed        EventType = "run.failed"
	EventRunCancelled     EventType = "run.cancelled"
)

type ToolAnnotations struct {
	ReadOnly    bool `json:"readOnly"`
	Destructive bool `json:"destructive"`
	Idempotent  bool `json:"idempotent"`
	OpenWorld   bool `json:"openWorld"`
	Sensitive   bool `json:"sensitive"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type PreparedCall struct {
	ToolCall
	Summary     string          `json:"summary"`
	Preview     string          `json:"-"`
	Annotations ToolAnnotations `json:"annotations"`
	Input       any             `json:"-"`
}

type ToolResult struct {
	Summary  string            `json:"summary"`
	Data     any               `json:"data,omitempty"`
	Message  string            `json:"message,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ContextFact is durable diagnostic memory. Unlike chat messages, facts are
// keyed and replaceable, so compaction does not have to preserve duplicate
// prose in order to retain an exact route, error, or evidence reference.
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

type ToolFingerprint struct {
	ConversationID string    `json:"conversationId"`
	Fingerprint    string    `json:"fingerprint"`
	ToolName       string    `json:"toolName"`
	EvidenceID     string    `json:"evidenceId,omitempty"`
	Successful     bool      `json:"successful"`
	LastSeen       time.Time `json:"lastSeen"`
}

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

type Finding struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Severity    string   `json:"severity"`
	Summary     string   `json:"summary"`
	EvidenceIDs []string `json:"evidenceIds"`
}

type Event struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversationId"`
	RunID          string    `json:"runId"`
	Type           EventType `json:"type"`
	Timestamp      time.Time `json:"timestamp"`
	Payload        any       `json:"payload,omitempty"`
}

type Run struct {
	ID             string            `json:"id"`
	ConversationID string            `json:"conversationId"`
	ProfileID      string            `json:"profileId"`
	Goal           string            `json:"goal"`
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

type ConversationStatus string

const (
	ConversationActive   ConversationStatus = "active"
	ConversationArchived ConversationStatus = "archived"
)

type Conversation struct {
	ID        string             `json:"id"`
	ProfileID string             `json:"profileId"`
	Title     string             `json:"title"`
	Status    ConversationStatus `json:"status"`
	CreatedAt time.Time          `json:"createdAt"`
	UpdatedAt time.Time          `json:"updatedAt"`
}

type DiagnosticRequest struct {
	Goal        string       `json:"goal"`
	ProfileID   string       `json:"profileId"`
	Attachments []Attachment `json:"attachments,omitempty"`
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
