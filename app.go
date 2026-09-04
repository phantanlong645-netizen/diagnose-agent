package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"olt-diagnostic-agent/internal/agent"
	"olt-diagnostic-agent/internal/application"
	"olt-diagnostic-agent/internal/domain"
	"olt-diagnostic-agent/internal/policy"
	"olt-diagnostic-agent/internal/target"
	"olt-diagnostic-agent/internal/tools"
)

const diagnosticEventName = "diagnostic:event"

type App struct {
	ctx          context.Context
	targets      *target.Store
	registry     *tools.Registry
	journal      *application.Journal
	approvals    *application.ApprovalBroker
	runner       *application.Runner
	engine       *agent.Engine
	mcp          *tools.MCPManager
	configPath   string
	databasePath string
	configMu     sync.Mutex
	cancelMu     sync.Mutex
	cancels      map[string]context.CancelFunc
	agentActive  bool
}

type appConfiguration struct {
	Version  int                    `json:"version"`
	Profiles []domain.TargetProfile `json:"profiles,omitempty"`
	Model    *domain.ModelSettings  `json:"model,omitempty"`
}

// NewApp 装配整个应用的依赖图并返回根组件。
//
// 顺序很关键，大致分为四段：
//  1. 加载用户配置（config.json）与本地 SQLite journal（diagnostics.db）；
//  2. 构造工具注册表并注册全部内置工具（NBI/NETCONF/文件 IO/搜索/写文件/shell/
//     联网/网页抓取/代码检索/MCP/长期记忆），单个工具初始化失败立即中止启动；
//  3. 用 registry + policy + journal + approval 组装 application.Runner，
//     再为 Runner 挂 read_evidence 工具（支持跨 Run 解析旧 evidence ID）；
//  4. 创建 agent.Engine 桥接器，并按需恢复持久化的模型配置；
//
// 返回的 *App 同时实现 EventSink（Emit 把事件通过 Wails 推给前端），
// 因此也被注入 Runner 作为事件出口，构成"后端事件流 -> 前端"的唯一通道。
func NewApp() (*App, error) {
	configPath, err := applicationConfigPath()
	if err != nil {
		return nil, err
	}
	configuration, err := readApplicationConfiguration(configPath)
	if err != nil {
		return nil, err
	}
	databasePath := filepath.Join(filepath.Dir(configPath), "diagnostics.db")
	journal, err := application.NewJournal(databasePath)
	if err != nil {
		return nil, err
	}
	app := &App{
		targets:      target.NewStore(),
		registry:     tools.NewRegistry(),
		journal:      journal,
		approvals:    application.NewApprovalBroker(),
		cancels:      make(map[string]context.CancelFunc),
		configPath:   configPath,
		databasePath: databasePath,
	}
	for _, profile := range configuration.Profiles {
		if err = app.targets.Save(profile); err != nil {
			return nil, fmt.Errorf("load saved target %q: %w", profile.Name, err)
		}
	}

	nbiTool, err := tools.NewNBITool(app.targets, nil)
	if err != nil {
		return nil, err
	}
	if err = app.registry.Register(nbiTool); err != nil {
		return nil, err
	}
	accessConsoleLogsTool, err := tools.NewAccessConsoleLogsTool(nbiTool)
	if err != nil {
		return nil, err
	}
	if err = app.registry.Register(accessConsoleLogsTool); err != nil {
		return nil, err
	}
	netconfTool, err := tools.NewNETCONFTool(app.targets, tools.NewSSHNETCONFTransport())
	if err != nil {
		return nil, err
	}
	if err = app.registry.Register(netconfTool); err != nil {
		return nil, err
	}
	fileReadTool, err := tools.NewFileReadTool(app.targets)
	if err != nil {
		return nil, err
	}
	if err = app.registry.Register(fileReadTool); err != nil {
		return nil, err
	}
	fileSearchTool, err := tools.NewFileSearchTool(app.targets)
	if err != nil {
		return nil, err
	}
	if err = app.registry.Register(fileSearchTool); err != nil {
		return nil, err
	}
	fileWriteTool, err := tools.NewFileWriteTool(app.targets)
	if err != nil {
		return nil, err
	}
	if err = app.registry.Register(fileWriteTool); err != nil {
		return nil, err
	}
	shellTool, err := tools.NewShellTool(app.targets)
	if err != nil {
		return nil, err
	}
	if err = app.registry.Register(shellTool); err != nil {
		return nil, err
	}
	// paicli-go 迁移能力：联网搜索 / 网页抓取（只读，走 ReadOnly+Idempotent 注解，
	// policy 自动放行；后端按 SEARXNG_BASE_URL > SERPAPI_API_KEY > DuckDuckGo 选择）。
	webSearchTool, err := tools.NewWebSearchTool()
	if err != nil {
		return nil, err
	}
	if err = app.registry.Register(webSearchTool); err != nil {
		return nil, err
	}
	webFetchTool, err := tools.NewWebFetchTool()
	if err != nil {
		return nil, err
	}
	if err = app.registry.Register(webFetchTool); err != nil {
		return nil, err
	}
	// paicli-go 迁移能力：RAG 语义代码检索（search_files 的语义兜底）。
	// 索引按 profile 持久化在应用配置目录 rag/ 下，root 漂移自动重建。
	codeSearchTool, err := tools.NewCodeSearchTool(app.targets, filepath.Dir(app.configPath))
	if err != nil {
		return nil, err
	}
	if err = app.registry.Register(codeSearchTool); err != nil {
		return nil, err
	}

	// paicli-go 迁移能力：MCP stdio/HTTP 双传输工具。配置在 config.json 同目录的
	// mcp.json（{"mcpServers":{...}}）。单个 server 连接失败只跳过，不影响应用启动。
	// 未分类 MCP 工具保持 OpenWorld 并由 policy 要求审批；只有本机配置明确
	// 标记为只读且绑定 Team 角色的工具才会进入对应 worker。
	app.mcp = tools.LoadMCP(context.Background(), filepath.Join(filepath.Dir(app.configPath), "mcp.json"))
	for _, mcpTool := range app.mcp.Tools() {
		if err = app.registry.Register(mcpTool); err != nil {
			return nil, err
		}
	}

	// paicli-go 迁移能力：长期记忆（跨会话 + 关键词检索 + JSON 持久化）。
	// 存储在 config.json 同目录的 memory.json，global 条目跨 profile 共享。
	memoryStore, err := tools.NewMemoryStore(filepath.Join(filepath.Dir(app.configPath), "memory.json"))
	if err != nil {
		return nil, fmt.Errorf("load long-term memory: %w", err)
	}
	memorySaveTool, err := tools.NewMemorySaveTool(memoryStore)
	if err != nil {
		return nil, err
	}
	if err = app.registry.Register(memorySaveTool); err != nil {
		return nil, err
	}
	memorySearchTool, err := tools.NewMemorySearchTool(memoryStore)
	if err != nil {
		return nil, err
	}
	if err = app.registry.Register(memorySearchTool); err != nil {
		return nil, err
	}

	app.runner = application.NewRunner(app.registry, policy.NewEngine(), app.journal, app, app.approvals)
	// read_evidence 接到 runner 而非 journal：runner.Evidence 在当前 run 未命中时
	// 会回退到 EvidenceByConversation，从而让压缩摘要 / diagnostic-memory 里跨 run
	// 残留的 evidence ID 仍可解析，避免 read_evidence 误报 "evidence not found"。
	evidenceTool, err := tools.NewEvidenceTool(app.runner)
	if err != nil {
		return nil, err
	}
	if err = app.registry.Register(evidenceTool); err != nil {
		return nil, err
	}
	app.engine, err = agent.NewEngine(app.runner, app.targets)
	if err != nil {
		return nil, err
	}
	if configuration.Model != nil {
		if _, err = app.engine.Restore(context.Background(), *configuration.Model); err != nil {
			return nil, fmt.Errorf("load saved model configuration: %w", err)
		}
	}
	return app, nil
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) Emit(event domain.Event) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, diagnosticEventName, event)
	}
}

func (a *App) shutdown(_ context.Context) {
	if a.mcp != nil {
		a.mcp.Close()
	}
	if a.journal != nil {
		_ = a.journal.Close()
	}
}

func (a *App) SaveTargetProfile(profile domain.TargetProfile) (domain.TargetProfileSummary, error) {
	profile.ID = strings.TrimSpace(profile.ID)
	targetIdentityChanged := false
	if profile.ID == "" {
		profile.ID = uuid.NewString()
	} else if existing, exists := a.targets.Profile(profile.ID); exists {
		existingNBIBaseURL, existingOLTAddress := "", ""
		newNBIBaseURL, newOLTAddress := "", ""
		if existing.NBI != nil {
			existingNBIBaseURL = strings.TrimRight(strings.TrimSpace(existing.NBI.BaseURL), "/")
			existingOLTAddress = strings.TrimSpace(existing.NBI.OLTAddress)
		}
		if profile.NBI != nil {
			newNBIBaseURL = strings.TrimRight(strings.TrimSpace(profile.NBI.BaseURL), "/")
			newOLTAddress = strings.TrimSpace(profile.NBI.OLTAddress)
		}
		targetIdentityChanged = existingNBIBaseURL != newNBIBaseURL ||
			existingOLTAddress != newOLTAddress ||
			netconfIdentity(existing) != netconfIdentity(profile)
		if profile.NBI != nil && existing.NBI != nil && profile.NBI.Token == "" {
			profile.NBI.Token = existing.NBI.Token
		}
		if profile.NBI != nil && existing.NBI != nil && profile.NBI.Password == "" {
			profile.NBI.Password = existing.NBI.Password
		}
		if profile.NETCONF != nil && existing.NETCONF != nil && profile.NETCONF.Password == "" {
			profile.NETCONF.Password = existing.NETCONF.Password
		}
		if len(profile.NETCONFEndpoints) > 0 {
			previousEndpoints := make(map[string]domain.NETCONFEndpoint)
			for _, endpoint := range existing.NETCONFEndpoints {
				previousEndpoints[strings.ToLower(strings.TrimSpace(endpoint.ID))] = endpoint
			}
			if existing.NETCONF != nil {
				previousEndpoints["default"] = domain.NETCONFEndpoint{
					ID:              "default",
					Password:        existing.NETCONF.Password,
					Username:        existing.NETCONF.Username,
					KnownHostsFile:  existing.NETCONF.KnownHostsFile,
					InsecureHostKey: existing.NETCONF.InsecureHostKey,
				}
			}
			for index := range profile.NETCONFEndpoints {
				if profile.NETCONFEndpoints[index].Password == "" {
					previous, exists := previousEndpoints[strings.ToLower(strings.TrimSpace(profile.NETCONFEndpoints[index].ID))]
					if exists {
						profile.NETCONFEndpoints[index].Password = previous.Password
					}
				}
			}
		}
	}
	if targetIdentityChanged {
		a.cancelMu.Lock()
		active := a.agentActive
		a.cancelMu.Unlock()
		if active {
			return domain.TargetProfileSummary{}, errors.New("stop the active diagnostic run before changing the target identity")
		}
	}
	if err := a.targets.Save(profile); err != nil {
		return domain.TargetProfileSummary{}, err
	}
	if err := a.persistConfiguration(); err != nil {
		return domain.TargetProfileSummary{}, err
	}
	if targetIdentityChanged {
		if current, currentErr := a.journal.CurrentConversation(profile.ID); currentErr == nil {
			a.approvals.ClearConversation(current.ID)
		}
		if _, err := a.journal.NewConversation(profile.ID); err != nil {
			return domain.TargetProfileSummary{}, fmt.Errorf("start a clean conversation for the changed target: %w", err)
		}
	}
	for _, summary := range a.targets.Summaries() {
		if summary.ID == profile.ID {
			return summary, nil
		}
	}
	return domain.TargetProfileSummary{}, errors.New("saved target profile was not found")
}

func netconfIdentity(profile domain.TargetProfile) string {
	if len(profile.NETCONFEndpoints) > 0 {
		var identity strings.Builder
		for _, endpoint := range profile.NETCONFEndpoints {
			fmt.Fprintf(&identity, "%s\x00%s\x00%s\x00%d\x00", strings.ToLower(strings.TrimSpace(endpoint.ID)), strings.ToLower(strings.TrimSpace(endpoint.Name)), strings.TrimSpace(endpoint.Address), endpoint.Port)
		}
		return identity.String()
	}
	if profile.NETCONF == nil {
		return ""
	}
	return fmt.Sprintf("default\x00%s\x00%d", strings.TrimSpace(profile.NETCONF.Address), profile.NETCONF.Port)
}

func (a *App) TargetProfiles() []domain.TargetProfileSummary {
	return a.targets.Summaries()
}

func (a *App) ToolDefinitions() []tools.Definition {
	return a.registry.Definitions()
}

func (a *App) AgentReadiness(profileID string) domain.AgentReadiness {
	profileID = strings.TrimSpace(profileID)
	readiness := domain.AgentReadiness{
		BuiltinSkillLoaded: true,
		ModelConfigured:    a.engine.Settings().Configured,
	}
	if a.mcp != nil {
		readiness.MCP = a.mcp.Status()
	}
	for _, definition := range a.registry.Definitions() {
		readiness.ToolNames = append(readiness.ToolNames, definition.Name)
	}
	if profileID == "" || !a.targets.Exists(profileID) {
		readiness.Issues = append(readiness.Issues, "select a valid target profile")
	} else {
		readiness.TargetConfigured = true
		loadedSkills, _ := a.targets.Skills(profileID)
		readiness.ExternalSkillCount = len(loadedSkills)
		for _, profile := range a.targets.Profiles() {
			if profile.ID == profileID && len(profile.SkillPaths) != len(loadedSkills) {
				readiness.Issues = append(readiness.Issues, "one or more configured external skill files are unavailable")
				break
			}
		}
	}
	if !readiness.ModelConfigured {
		readiness.Issues = append(readiness.Issues, "configure and verify a chat model")
	}
	requiredTools := []string{"nbi_request", "collect_access_console_logs", "netconf_rpc", "search_files", "read_file", "write_file", "run_shell"}
	availableTools := make(map[string]bool, len(readiness.ToolNames))
	for _, name := range readiness.ToolNames {
		availableTools[name] = true
	}
	for _, name := range requiredTools {
		if !availableTools[name] {
			readiness.Issues = append(readiness.Issues, "required tool is unavailable: "+name)
		}
	}
	readiness.Ready = len(readiness.Issues) == 0
	return readiness
}

func (a *App) ConfigureModel(settings domain.ModelSettings) (domain.ModelSettingsSummary, error) {
	summary, err := a.engine.Configure(a.appContext(), settings)
	if err != nil {
		return domain.ModelSettingsSummary{}, err
	}
	if err = a.persistConfiguration(); err != nil {
		return domain.ModelSettingsSummary{}, err
	}
	return summary, nil
}

func (a *App) ModelSettings() domain.ModelSettingsSummary {
	return a.engine.Settings()
}

func (a *App) ConfigurationPath() string {
	return a.configPath
}

func (a *App) DatabasePath() string {
	return a.databasePath
}

func (a *App) CurrentConversation(profileID string) (domain.Conversation, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" || !a.targets.Exists(profileID) {
		return domain.Conversation{}, errors.New("a valid target profile is required")
	}
	return a.journal.CurrentConversation(profileID)
}

func (a *App) Conversations(profileID string) ([]domain.Conversation, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" || !a.targets.Exists(profileID) {
		return nil, errors.New("a valid target profile is required")
	}
	return a.journal.Conversations(profileID)
}

func (a *App) OpenConversation(profileID, conversationID string) (domain.Conversation, error) {
	profileID = strings.TrimSpace(profileID)
	conversationID = strings.TrimSpace(conversationID)
	if profileID == "" || !a.targets.Exists(profileID) {
		return domain.Conversation{}, errors.New("a valid target profile is required")
	}
	if conversationID == "" {
		return domain.Conversation{}, errors.New("conversation id is required")
	}
	a.cancelMu.Lock()
	active := a.agentActive
	a.cancelMu.Unlock()
	if active {
		return domain.Conversation{}, errors.New("stop the active diagnostic run before switching conversations")
	}
	return a.journal.ActivateConversation(profileID, conversationID)
}

func (a *App) NewConversation(profileID string) (domain.Conversation, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" || !a.targets.Exists(profileID) {
		return domain.Conversation{}, errors.New("a valid target profile is required")
	}
	a.cancelMu.Lock()
	active := a.agentActive
	a.cancelMu.Unlock()
	if active {
		return domain.Conversation{}, errors.New("stop the active diagnostic run before starting a new conversation")
	}
	current, err := a.journal.CurrentConversation(profileID)
	if err != nil {
		return domain.Conversation{}, err
	}
	conversation, err := a.journal.NewConversation(profileID)
	if err != nil {
		return domain.Conversation{}, err
	}
	a.approvals.ClearConversation(current.ID)
	return conversation, nil
}

func (a *App) ConversationApprovalEnabled(conversationID string) bool {
	return a.approvals.ConversationApproved(strings.TrimSpace(conversationID))
}

func (a *App) ConversationEvents(conversationID string) ([]domain.Event, error) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return nil, errors.New("conversation id is required")
	}
	return a.journal.Events(conversationID)
}

func (a *App) StartDiagnostic(request domain.DiagnosticRequest) (domain.Run, error) {
	request.Goal = strings.TrimSpace(request.Goal)
	request.ProfileID = strings.TrimSpace(request.ProfileID)
	if request.Goal == "" {
		return domain.Run{}, errors.New("diagnostic goal is required")
	}
	if readiness := a.AgentReadiness(request.ProfileID); !readiness.Ready {
		return domain.Run{}, fmt.Errorf("agent is not ready: %s", strings.Join(readiness.Issues, "; "))
	}
	mode, err := domain.NormalizeDiagnosticMode(request.Mode)
	if err != nil || mode == domain.DiagnosticModeManual {
		if err == nil {
			err = errors.New("manual mode must use StartManualSession")
		}
		return domain.Run{}, err
	}
	// 处理附件：将文本/PDF 内容提取后拼装到 goal 前面，图片/二进制只注文件名提示
	var images []domain.ImageAttachment
	if len(request.Attachments) > 0 {
		injection, err := tools.ProcessAttachments(request.Attachments)
		if err != nil {
			return domain.Run{}, fmt.Errorf("process attachments: %w", err)
		}
		if injection != "" {
			request.Goal = injection + "\n\n用户诊断目标：" + request.Goal
		}
		// 提取图片附件用于多模态消息，让模型直接"看"图片内容
		for _, att := range request.Attachments {
			if tools.IsImageType(att.MimeType) {
				images = append(images, domain.ImageAttachment{
					Filename: att.Filename,
					MimeType: att.MimeType,
					Data:     att.Data,
				})
			}
		}
	}
	a.cancelMu.Lock()
	if a.agentActive {
		a.cancelMu.Unlock()
		return domain.Run{}, errors.New("another diagnostic run is already active")
	}
	a.agentActive = true
	a.cancelMu.Unlock()
	conversation, err := a.journal.CurrentConversation(request.ProfileID)
	if err != nil {
		a.cancelMu.Lock()
		a.agentActive = false
		a.cancelMu.Unlock()
		return domain.Run{}, err
	}
	run, err := a.runner.Start(request.Goal, request.ProfileID, conversation.ID, mode, images...)
	if err != nil {
		a.cancelMu.Lock()
		a.agentActive = false
		a.cancelMu.Unlock()
		return domain.Run{}, err
	}
	runContext, cancel := context.WithCancel(a.appContext())
	a.cancelMu.Lock()
	a.cancels[run.ID] = cancel
	a.cancelMu.Unlock()
	go func() {
		defer func() {
			a.cancelMu.Lock()
			delete(a.cancels, run.ID)
			a.agentActive = false
			a.cancelMu.Unlock()
		}()
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				_, _ = a.runner.Fail(run.ID, fmt.Errorf("diagnostic agent stopped unexpectedly: %v", recovered))
			}
		}()
		var runErr error
		if mode == domain.DiagnosticModeTeam {
			runErr = a.engine.RunTeam(runContext, run.ID)
		} else {
			runErr = a.engine.Run(runContext, run.ID)
		}
		if runErr != nil {
			runtime.LogErrorf(a.ctx, "diagnostic run %s failed: %v", run.ID, runErr)
		}
	}()
	return run, nil
}

func (a *App) StartManualSession(profileID string) (domain.Run, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" || !a.targets.Exists(profileID) {
		return domain.Run{}, errors.New("a valid target profile is required")
	}
	conversation, err := a.journal.CurrentConversation(profileID)
	if err != nil {
		return domain.Run{}, err
	}
	return a.runner.Start("Manual diagnostic session", profileID, conversation.ID, domain.DiagnosticModeManual)
}

// GenerateManualDraft turns a natural-language request into a locally
// validated Manual-form draft. It does not create a run or contact an OLT.
func (a *App) GenerateManualDraft(request domain.ManualDraftRequest) (domain.ManualDraftResponse, error) {
	return a.engine.GenerateManualDraft(a.appContext(), request)
}

func (a *App) ExecuteManualTool(runID, toolName, arguments string) (domain.Evidence, error) {
	run, exists := a.runner.Run(strings.TrimSpace(runID))
	if !exists || run.Status != domain.RunRunning {
		return domain.Evidence{}, errors.New("an active manual session is required")
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil {
		return domain.Evidence{}, fmt.Errorf("decode tool arguments: %w", err)
	}
	input["profileId"] = run.ProfileID
	preparedArguments, err := json.Marshal(input)
	if err != nil {
		return domain.Evidence{}, fmt.Errorf("encode tool arguments: %w", err)
	}
	return a.runner.Execute(a.appContext(), run.ID, domain.ToolCall{
		ID:        uuid.NewString(),
		Name:      strings.TrimSpace(toolName),
		Arguments: preparedArguments,
	})
}

func (a *App) CancelDiagnostic(runID string) (domain.Run, error) {
	a.cancelMu.Lock()
	cancel := a.cancels[runID]
	a.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return a.runner.Cancel(runID)
}

func (a *App) DiagnosticRun(runID string) (domain.Run, error) {
	run, exists := a.runner.Run(runID)
	if !exists {
		return domain.Run{}, errors.New("diagnostic run not found")
	}
	return run, nil
}

func (a *App) DiagnosticEvents(runID string) ([]domain.Event, error) {
	run, exists := a.runner.Run(strings.TrimSpace(runID))
	if !exists {
		return nil, errors.New("diagnostic run not found")
	}
	events, err := a.journal.Events(run.ConversationID)
	if err != nil {
		return nil, err
	}
	filtered := make([]domain.Event, 0)
	for _, event := range events {
		if event.RunID == run.ID {
			filtered = append(filtered, event)
		}
	}
	return filtered, nil
}

func (a *App) appContext() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func (a *App) ResolveApproval(callID string, approved bool, approveConversation bool) error {
	return a.approvals.Resolve(callID, approved, approveConversation)
}

func (a *App) CompleteDiagnostic(runID string) (domain.Run, error) {
	return a.runner.Complete(runID, nil)
}

func applicationConfigPath() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user configuration directory: %w", err)
	}
	return filepath.Join(root, "OLT Diagnostic Agent", "config.json"), nil
}

func readApplicationConfiguration(path string) (appConfiguration, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return appConfiguration{Version: 1}, nil
	}
	if err != nil {
		return appConfiguration{}, fmt.Errorf("read application configuration: %w", err)
	}
	var configuration appConfiguration
	if err = json.Unmarshal(content, &configuration); err != nil {
		return appConfiguration{}, fmt.Errorf("decode application configuration %s: %w", path, err)
	}
	if configuration.Version != 1 {
		return appConfiguration{}, fmt.Errorf("unsupported application configuration version: %d", configuration.Version)
	}
	return configuration, nil
}

func (a *App) persistConfiguration() error {
	a.configMu.Lock()
	defer a.configMu.Unlock()

	configuration := appConfiguration{Version: 1, Profiles: a.targets.Profiles()}
	if modelSettings, configured := a.engine.Configuration(); configured {
		configuration.Model = &modelSettings
	}
	content, err := json.MarshalIndent(configuration, "", "  ")
	if err != nil {
		return fmt.Errorf("encode application configuration: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(a.configPath), 0700); err != nil {
		return fmt.Errorf("create application configuration directory: %w", err)
	}
	if err = os.WriteFile(a.configPath, append(content, '\n'), 0600); err != nil {
		return fmt.Errorf("write application configuration: %w", err)
	}
	return nil
}
