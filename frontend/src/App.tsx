import {FormEvent, useEffect, useMemo, useRef, useState} from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import {EventsOff, EventsOn} from '../wailsjs/runtime/runtime';
import './App.css';

type TargetProfile = {
    id: string;
    name: string;
    organizationCode?: string;
    nbiBaseUrl?: string;
    oltAddress?: string;
    nbiUsername?: string;
    tokenHeader?: string;
    caFile?: string;
    insecureTls?: boolean;
    nbiTokenConfigured?: boolean;
    nbiPasswordConfigured?: boolean;
    netconfAddress?: string;
    netconfPort?: number;
    netconfUsername?: string;
    knownHostsFile?: string;
    insecureHostKey?: boolean;
    netconfPasswordConfigured?: boolean;
    netconfEndpoints?: NETCONFEndpoint[];
    workspaceRoots?: string[];
    skillPaths?: string[];
};

type NETCONFEndpoint = {
    id: string;
    name?: string;
    address?: string;
    port?: number;
    username?: string;
    knownHostsFile?: string;
    insecureHostKey?: boolean;
    passwordConfigured?: boolean;
};

type DiagnosticRun = {
    id: string;
    conversationId: string;
    profileId: string;
    goal: string;
    mode: 'agent' | 'team' | 'manual';
    status: string;
    startedAt: string;
};

type DiagnosticEvent = {
    id: string;
    conversationId: string;
    runId: string;
    type: string;
    timestamp: string;
    payload?: any;
};

type TeamStep = {
    id: string;
    goal: string;
    role: 'device' | 'platform' | 'source' | 'web' | 'correlator' | string;
    dependsOn?: string[];
    sourceSearch?: {
        exactTerms?: string[];
        ownerPaths?: string[];
        maxSearches?: number;
        runtimeDerived?: boolean;
    };
};

type TeamStepResult = {
    stepId: string;
    role: string;
    status: 'success' | 'partial' | 'failed' | 'skipped' | string;
    summary?: string;
    evidenceIds?: string[];
    referencedEvidenceIds?: string[];
    unknowns?: string[];
    error?: string;
};

// 证据 Inspector 里把字符串字面量转义符(\r\n 等)还原成真实字符。
function unescapeEvidence(value: string): string {
    return value
        .replace(/\\r\\n/g, '\n')
        .replace(/\\r/g, '\n')
        .replace(/\\n/g, '\n')
        .replace(/\\t/g, '\t')
        .replace(/\\\\/g, '\\');
}

function looksLikeXML(value: string): boolean {
    const trimmed = value.trim();
    return trimmed.startsWith('<?xml') || trimmed.startsWith('<');
}

function looksLikeJSON(value: string): boolean {
    const trimmed = value.trim();
    return (trimmed.startsWith('{') && trimmed.endsWith('}')) || (trimmed.startsWith('[') && trimmed.endsWith(']'));
}

// 简单 XML 缩进格式化,仅用于展示,不保证 100% 正确性。
function formatXML(xml: string): string {
    let formatted = '';
    let indent = '';
    const tab = '  ';
    // 先把标签之间的空白去掉,再按 < 或 > 切分
    const tokens = xml.replace(/>[\s\r\n]*</g, '><').split(/(?=<)|(?<=>)/g).filter(Boolean);
    for (const token of tokens) {
        if (token.startsWith('</')) {
            indent = indent.slice(tab.length);
        }
        if (token.trim().length > 0) {
            formatted += indent + token + '\n';
        }
        if (token.startsWith('<') && !token.startsWith('</') && !token.endsWith('/>') && !token.includes('</')) {
            indent += tab;
        }
    }
    return formatted.trimEnd();
}

// 尝试把单个字符串格式化为可读的 JSON/XML/文本。
function formatEvidenceString(value: string): { formatted: string; type: 'json' | 'xml' | 'text' } {
    const unescaped = unescapeEvidence(value);
    if (looksLikeXML(unescaped)) {
        return { formatted: formatXML(unescaped), type: 'xml' };
    }
    if (looksLikeJSON(unescaped)) {
        try {
            return { formatted: JSON.stringify(JSON.parse(unescaped), null, 2), type: 'json' };
        } catch {
            // 解析失败就当成文本
        }
    }
    return { formatted: unescaped, type: 'text' };
}

// 证据 Inspector 右侧主面板的数据渲染组件。
// 对 data 的每个字段:如果是含转义的长字符串(JSON/XML),则解转义并格式化显示;否则仍用 JSON.stringify。
function EvidenceDataView({ data }: { data: any }) {
    if (data === null || data === undefined) {
        return <pre className="raw-output">null</pre>;
    }
    if (typeof data !== 'object') {
        const { formatted } = formatEvidenceString(String(data));
        return <pre className="raw-output">{formatted}</pre>;
    }

    return (
        <div className="evidence-data-view">
            {Object.entries(data).map(([key, value]) => {
                if (typeof value === 'string' && value.length > 60) {
                    const { formatted, type } = formatEvidenceString(value);
                    return (
                        <div key={key} className="evidence-field">
                            <div className="evidence-field-label">{key} <span>{type}</span></div>
                            <pre className="raw-output">{formatted}</pre>
                        </div>
                    );
                }
                return (
                    <div key={key} className="evidence-field">
                        <div className="evidence-field-label">{key}</div>
                        <pre className="raw-output">{JSON.stringify(value, null, 2)}</pre>
                    </div>
                );
            })}
        </div>
    );
}

type Conversation = {
    id: string;
    profileId: string;
    title: string;
    status: string;
    createdAt: string;
    updatedAt: string;
};

type Evidence = {
    id: string;
    kind: string;
    summary: string;
    data?: any;
    metadata?: Record<string, string>;
    capturedAt: string;
};

type ModelSettings = {
    baseUrl?: string;
    model?: string;
    timeoutSeconds?: number;
    contextWindowTokens?: number;
    outputReserveTokens?: number;
    configured: boolean;
    apiKeyConfigured?: boolean;
};

type Attachment = {
    filename: string;
    mimeType: string;
    data: string;
    size: number;
};

type ManualDraft = {
    kind: 'nbi' | 'netconf';
    method: string;
    path: string;
    headers: string;
    body: string;
    endpoint: string;
    rpc: string;
    timeoutSeconds: number;
};

type ManualBuilderMessage = {role: 'user' | 'assistant'; content: string};

type ManualDraftResponse = {
    message: string;
    draft: Omit<ManualDraft, 'headers'> & {kind?: 'nbi' | 'netconf'; headers?: Record<string, string>};
    valid: boolean;
    validationErrors?: string[];
};

const defaultManualDraft: ManualDraft = {
    kind: 'nbi', method: 'GET', path: '', headers: '', body: '', endpoint: '',
    rpc: '<?xml version="1.0" encoding="UTF-8"?>\n<rpc xmlns="urn:ietf:params:xml:ns:netconf:base:1.0" message-id="diagnostic-1">\n  <get>\n    <filter type="subtree">\n    </filter>\n  </get>\n</rpc>', timeoutSeconds: 30,
};

type AgentReadiness = {
    ready: boolean;
    targetConfigured: boolean;
    modelConfigured: boolean;
    builtinSkillLoaded: boolean;
    externalSkillCount: number;
    toolNames: string[];
    mcp: {
        configPath: string;
        configured: boolean;
        configError?: string;
        servers?: Array<{name: string; transport: string; connected: boolean; toolCount: number; error?: string}>;
    };
    issues?: string[];
};

const api = () => {
    const backend = (window as any).go?.main?.App;
    if (!backend) throw new Error('Wails backend bridge is not available. Open the desktop application through wails dev or the packaged executable.');
    return backend;
};

function App() {
    const [profiles, setProfiles] = useState<TargetProfile[]>([]);
    const [profileId, setProfileId] = useState('');
    const [goal, setGoal] = useState('');
    const [conversation, setConversation] = useState<Conversation | null>(null);
    const [conversationHistory, setConversationHistory] = useState<Conversation[]>([]);
    const [showHistory, setShowHistory] = useState(false);
    const [run, setRun] = useState<DiagnosticRun | null>(null);
    const [events, setEvents] = useState<DiagnosticEvent[]>([]);
    const [selectedEvidence, setSelectedEvidence] = useState<Evidence | null>(null);
    const [showSetup, setShowSetup] = useState(true);
    const [setupTab, setSetupTab] = useState<'target' | 'model'>('target');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState('');
    const [mode, setMode] = useState<'agent' | 'manual'>('agent');
    const [diagnosticMode, setDiagnosticMode] = useState<'agent' | 'team'>('agent');
    const [manualKind, setManualKind] = useState<'nbi' | 'netconf'>('nbi');
    const [manualDraft, setManualDraft] = useState<ManualDraft>(defaultManualDraft);
    const [manualPrompt, setManualPrompt] = useState('');
    const [manualMessages, setManualMessages] = useState<ManualBuilderMessage[]>([]);
    // 附件状态：存储用户选择的文件（已转成 base64），提交诊断时一并发给后端
    const [attachments, setAttachments] = useState<Attachment[]>([]);
    const fileInputRef = useRef<HTMLInputElement | null>(null);
    const submittedInputRef = useRef<{runID: string; goal: string; attachments: Attachment[]} | null>(null);
    const terminalRunStatusRef = useRef<Record<string, string>>({});
    // 拖拽高亮：文件拖到 textarea 上方时显示蓝色边框
    const [dragActive, setDragActive] = useState(false);
    const [modelSettings, setModelSettings] = useState<ModelSettings>({configured: false});
    const [configurationPath, setConfigurationPath] = useState('');
    const [readiness, setReadiness] = useState<AgentReadiness | null>(null);
    const [conversationApproval, setConversationApproval] = useState(false);
    const timelineRef = useRef<HTMLDivElement | null>(null);
    const followTimelineRef = useRef(true);

    async function loadConversationState(current: Conversation) {
        const [storedEvents, approvalEnabled] = await Promise.all([
            api().ConversationEvents(current.id) as Promise<DiagnosticEvent[]>,
            api().ConversationApprovalEnabled(current.id) as Promise<boolean>,
        ]);
        setConversation(current);
        setEvents(storedEvents ?? []);
        setConversationApproval(Boolean(approvalEnabled));
        const latestEvidence = [...(storedEvents ?? [])].reverse().find(item => item.type === 'evidence.captured');
        setSelectedEvidence(latestEvidence?.payload as Evidence ?? null);
        setRun(null);
    }

    async function refreshConversationHistory(targetID = profileId) {
        if (!targetID) {
            setConversationHistory([]);
            return;
        }
        const items = await api().Conversations(targetID) as Conversation[];
        setConversationHistory(items ?? []);
    }

    useEffect(() => {
        try {
            const backend = api();
            void Promise.all([backend.TargetProfiles(), backend.ModelSettings(), backend.ConfigurationPath()])
                .then(([items, settings, configPath]: [TargetProfile[], ModelSettings, string]) => {
                    setProfiles(items ?? []);
                    setModelSettings(settings ?? {configured: false});
                    setConfigurationPath(configPath ?? '');
                    setProfileId(items?.[0]?.id ?? '');
                    if (settings?.configured && items?.length > 0) setShowSetup(false);
                })
                .catch(reason => setError(String(reason)));

            EventsOn('diagnostic:event', (event: DiagnosticEvent) => {
                setEvents(current => current.some(item => item.id === event.id) ? current : [...current, event]);
                if (event.type === 'evidence.captured') setSelectedEvidence(event.payload as Evidence);
                if (event.type === 'run.completed' || event.type === 'run.failed' || event.type === 'run.cancelled') {
                    const status = event.type.replace('run.', '');
                    terminalRunStatusRef.current[event.runId] = status;
                    setRun(current => current?.id === event.runId ? {...current, status} : current);
                }
                if (event.type === 'approval.resolved' && event.payload?.scope === 'conversation' && event.payload?.approved) {
                    setConversationApproval(true);
                }
                if (event.type === 'run.failed') setError(String(event.payload?.error ?? 'Diagnostic run failed'));
            });
            return () => EventsOff('diagnostic:event');
        } catch (reason) {
            setError(reason instanceof Error ? reason.message : String(reason));
        }
    }, []);

    useEffect(() => {
        try {
            void api().AgentReadiness(profileId)
                .then((value: AgentReadiness) => setReadiness(value))
                .catch((reason: unknown) => setError(String(reason)));
        } catch (reason) {
            setError(reason instanceof Error ? reason.message : String(reason));
        }
    }, [profileId, modelSettings.configured]);

    useEffect(() => {
        if (!profileId) {
            setConversation(null);
            setConversationHistory([]);
            setShowHistory(false);
            setEvents([]);
            setRun(null);
            setConversationApproval(false);
            return;
        }
        let cancelled = false;
        setError('');
        void Promise.all([
            api().CurrentConversation(profileId) as Promise<Conversation>,
            api().Conversations(profileId) as Promise<Conversation[]>,
        ])
            .then(async ([current, history]: [Conversation, Conversation[]]) => {
                if (cancelled) return;
                setConversationHistory(history ?? []);
                setShowHistory(false);
                await loadConversationState(current);
            })
            .catch((reason: unknown) => {
                if (!cancelled) setError(String(reason));
            });
            return () => { cancelled = true; };
    }, [profileId]);

    useEffect(() => {
        if (!followTimelineRef.current || !timelineRef.current) return;
        const timeline = timelineRef.current;
        requestAnimationFrame(() => timeline.scrollTo({top: timeline.scrollHeight, behavior: 'auto'}));
    }, [events]);

    useEffect(() => {
        if (!run || !['completed', 'failed', 'cancelled'].includes(run.status)) return;
        const submitted = submittedInputRef.current;
        if (!submitted || submitted.runID !== run.id) return;
        if (run.status === 'completed') {
            setGoal(current => current === submitted.goal ? '' : current);
            setAttachments(current => attachmentsEqual(current, submitted.attachments) ? [] : current);
        }
        delete terminalRunStatusRef.current[run.id];
        submittedInputRef.current = null;
    }, [run?.id, run?.status]);

    const activeProfile = profiles.find(profile => profile.id === profileId);
    const approval = useMemo(() => [...events].reverse().find(event => event.type === 'approval.required' &&
        !events.some(candidate => candidate.type === 'approval.resolved' && candidate.payload?.callId === event.payload?.callId)), [events]);
    const evidence = useMemo(() => events.filter(event => event.type === 'evidence.captured').map(event => event.payload as Evidence), [events]);

    async function refreshProfiles() {
        const items = await api().TargetProfiles() as TargetProfile[];
        setProfiles(items ?? []);
        setProfileId(current => current || items?.[0]?.id || '');
    }

    // 选择文件：读取为 base64，加入 attachments 列表。限制单文件 5MB，最多 10 个。
    function handleFileSelect(files: FileList | null) {
        if (!files || files.length === 0) return;
        const maxFiles = 10;
        const maxSize = 5 * 1024 * 1024; // 5MB
        const remaining = maxFiles - attachments.length;
        if (remaining <= 0) { setError(`最多上传 ${maxFiles} 个文件`); return; }
        const toProcess = Array.from(files).slice(0, remaining);
        for (const file of toProcess) {
            if (file.size > maxSize) { setError(`文件 ${file.name} 超过 5MB 限制`); return; }
        }
        for (const file of toProcess) {
            const reader = new FileReader();
            reader.onload = () => {
                const result = reader.result as string;
                // FileReader.readAsDataURL 返回 "data:mime;base64,XXXX"，取逗号后部分
                const base64 = result.includes(',') ? result.split(',')[1] : result;
                setAttachments(prev => [...prev, {
                    filename: file.name,
                    mimeType: file.type || 'application/octet-stream',
                    data: base64,
                    size: file.size,
                }]);
            };
            reader.onerror = () => setError(`读取文件 ${file.name} 失败`);
            reader.readAsDataURL(file);
        }
    }

    // 从附件列表中移除指定索引的文件
    function removeAttachment(index: number) {
        setAttachments(prev => prev.filter((_, i) => i !== index));
    }

    async function startDiagnostic() {
        if (!goal.trim() || !profileId) return;
		const submittedGoal = goal.trim();
		const submittedAttachments = attachments.map(attachment => ({...attachment}));
        setBusy(true);
        setError('');
        try {
            const currentReadiness = await api().AgentReadiness(profileId) as AgentReadiness;
            setReadiness(currentReadiness);
            if (!currentReadiness.ready) throw new Error(currentReadiness.issues?.join('; ') || 'Agent is not ready');
            // 构造请求：有附件时带上 attachments 字段
            const payload: {goal: string; profileId: string; mode: 'agent' | 'team'; attachments?: {filename: string; mimeType: string; data: string}[]} = {
                goal: submittedGoal,
                profileId,
                mode: diagnosticMode,
            };
            if (submittedAttachments.length > 0) {
                payload.attachments = submittedAttachments.map(a => ({filename: a.filename, mimeType: a.mimeType, data: a.data}));
            }
            const started = await api().StartDiagnostic(payload) as DiagnosticRun;
            submittedInputRef.current = {runID: started.id, goal: submittedGoal, attachments: submittedAttachments};
            const terminalStatus = terminalRunStatusRef.current[started.id];
            setRun(terminalStatus ? {...started, status: terminalStatus} : started);
            await refreshConversationHistory();
        } catch (reason) {
            setError(String(reason));
        } finally {
            setBusy(false);
        }
    }

    async function newConversation() {
        if (!profileId || run?.status === 'running') return;
        setBusy(true);
        setError('');
        try {
            const created = await api().NewConversation(profileId) as Conversation;
            setConversation(created);
            setEvents([]);
            setSelectedEvidence(null);
            setRun(null);
            setGoal('');
            setConversationApproval(false);
            await refreshConversationHistory();
        } catch (reason) {
            setError(String(reason));
        } finally {
            setBusy(false);
        }
    }

    async function openConversation(historyItem: Conversation) {
        if (!profileId || run?.status === 'running' || historyItem.id === conversation?.id) {
            setShowHistory(false);
            return;
        }
        setBusy(true);
        setError('');
        try {
            const opened = await api().OpenConversation(profileId, historyItem.id) as Conversation;
            await loadConversationState(opened);
            await refreshConversationHistory();
            setShowHistory(false);
        } catch (reason) {
            setError(String(reason));
        } finally {
            setBusy(false);
        }
    }

    function newTargetProfile() {
        if (run?.status === 'running') return;
        setProfileId('');
        setSetupTab('target');
        setError('');
        setShowSetup(true);
    }

    async function resolveApproval(approved: boolean, approveConversation = false) {
        if (!approval?.payload?.callId) return;
        try {
            await api().ResolveApproval(approval.payload.callId, approved, approveConversation);
            if (approved && approveConversation) setConversationApproval(true);
        } catch (reason) {
            setError(String(reason));
        }
    }

    async function cancelDiagnostic() {
        if (!run) return;
        try {
            const cancelled = await api().CancelDiagnostic(run.id) as DiagnosticRun;
            setRun(cancelled);
        } catch (reason) {
            setError(String(reason));
        }
    }

    async function executeManual(event: FormEvent<HTMLFormElement>) {
        event.preventDefault();
        if (!profileId) return;
        const values = new FormData(event.currentTarget);
        setBusy(true);
        setError('');
        try {
            let session = run;
            if (!session || session.goal !== 'Manual diagnostic session' || session.status !== 'running') {
                setEvents([]);
                setSelectedEvidence(null);
                session = await api().StartManualSession(profileId) as DiagnosticRun;
                setRun(session);
            }
            let argumentsValue: Record<string, any>;
            let toolName: string;
            if (manualKind === 'nbi') {
                toolName = 'nbi_request';
                const headerText = String(values.get('headers') ?? '').trim();
                argumentsValue = {
                    method: values.get('method'),
                    path: values.get('path'),
                    headers: headerText ? JSON.parse(headerText) : {},
                    body: values.get('body'),
                };
            } else {
                toolName = 'netconf_rpc';
                argumentsValue = {endpoint: values.get('endpoint'), rpc: values.get('rpc'), timeoutSeconds: Number(values.get('timeoutSeconds'))};
            }
            await api().ExecuteManualTool(session.id, toolName, JSON.stringify(argumentsValue));
        } catch (reason) {
            setError(String(reason));
        } finally {
            setBusy(false);
        }
    }

    async function buildManualDraft(event: FormEvent<HTMLFormElement>) {
        event.preventDefault();
        const prompt = manualPrompt.trim();
        if (!profileId || !prompt) return;
        const nextMessages: ManualBuilderMessage[] = [...manualMessages, {role: 'user', content: prompt}];
        setManualMessages(nextMessages);
        setManualPrompt('');
        setBusy(true);
        setError('');
        try {
            const response = await api().GenerateManualDraft({profileId, messages: nextMessages}) as ManualDraftResponse;
            const validation = response.validationErrors?.length ? `\n\nValidation: ${response.validationErrors.join(' ')}` : '';
            setManualMessages([...nextMessages, {role: 'assistant', content: `${response.message}${validation}`}]);
            if (response.valid && response.draft.kind) {
                const draft = response.draft;
                setManualKind(draft.kind);
                setManualDraft({
                    ...defaultManualDraft,
                    ...draft,
                    kind: draft.kind,
                    headers: Object.keys(draft.headers ?? {}).length ? JSON.stringify(draft.headers, null, 2) : '',
                });
            }
        } catch (reason) {
            setError(String(reason));
        } finally {
            setBusy(false);
        }
    }

    async function saveTarget(event: FormEvent<HTMLFormElement>) {
        event.preventDefault();
        const values = new FormData(event.currentTarget);
        const nbiEnabled = values.get('nbiEnabled') === 'on';
        const netconfEnabled = values.get('netconfEnabled') === 'on';
        const profile: any = {
            id: profileId,
            name: values.get('name'),
            organizationCode: values.get('organizationCode'),
            workspaceRoots: String(values.get('workspaceRoots') ?? '').split(/[;\n]/).map(item => item.trim()).filter(Boolean),
            skillPaths: String(values.get('skillPaths') ?? '').split(/[;\n]/).map(item => item.trim()).filter(Boolean),
        };
        if (nbiEnabled) {
            profile.nbi = {
                baseUrl: values.get('baseUrl'),
                oltAddress: values.get('oltAddress'),
                username: values.get('nbiUsername'),
                password: values.get('nbiPassword'),
                tokenHeader: values.get('tokenHeader'),
                token: values.get('token'),
                caFile: values.get('caFile'),
                insecureTls: values.get('insecureTls') === 'on',
            };
        }
        if (netconfEnabled) {
            const endpointIDs = values.getAll('netconfEndpointId').map(value => String(value));
            const endpointNames = values.getAll('netconfEndpointName').map(value => String(value));
            const endpointAddresses = values.getAll('netconfEndpointAddress').map(value => String(value));
            const endpointPorts = values.getAll('netconfEndpointPort').map(value => Number(value));
            const endpointUsernames = values.getAll('netconfEndpointUsername').map(value => String(value));
            const endpointPasswords = values.getAll('netconfEndpointPassword').map(value => String(value));
            const endpointKnownHosts = values.getAll('netconfEndpointKnownHosts').map(value => String(value));
            const endpointInsecureHostKeys = values.getAll('netconfEndpointInsecureHostKey').map(value => value === 'on');
            profile.netconfEndpoints = endpointIDs.map((id, index) => ({
                id,
                name: endpointNames[index] ?? '',
                address: endpointAddresses[index] ?? '',
                port: endpointPorts[index] || 830,
                username: endpointUsernames[index] ?? '',
                password: endpointPasswords[index] ?? '',
                knownHostsFile: endpointKnownHosts[index] ?? '',
                insecureHostKey: endpointInsecureHostKeys[index] ?? false,
            }));
        }
        setBusy(true);
        setError('');
        try {
            const saved = await api().SaveTargetProfile(profile) as TargetProfile;
            await refreshProfiles();
            setProfileId(saved.id);
            setReadiness(await api().AgentReadiness(saved.id) as AgentReadiness);
            setSetupTab('model');
        } catch (reason) {
            setError(String(reason));
        } finally {
            setBusy(false);
        }
    }

    async function saveModel(event: FormEvent<HTMLFormElement>) {
        event.preventDefault();
        const values = new FormData(event.currentTarget);
        setBusy(true);
        setError('');
        try {
            const settings = await api().ConfigureModel({
                baseUrl: values.get('modelBaseUrl'),
                apiKey: values.get('apiKey'),
                model: values.get('model'),
                timeoutSeconds: Number(values.get('modelTimeout')),
                contextWindowTokens: Number(values.get('contextWindowTokens')),
                outputReserveTokens: Number(values.get('outputReserveTokens')),
            }) as ModelSettings;
            setModelSettings(settings);
            setReadiness(await api().AgentReadiness(profileId) as AgentReadiness);
            setShowSetup(false);
        } catch (reason) {
            setError(String(reason));
        } finally {
            setBusy(false);
        }
    }

    return (
        <main className="shell">
            <header className="topbar">
                <div className="brand">
                    <span className="brand-mark">OD</span>
                    <div><strong>OLT DIAGNOSTIC</strong><small>evidence console</small></div>
                </div>
                <div className="target-switcher">
                    <span className={`pulse ${activeProfile ? 'online' : ''}`}/>
                    <select value={profileId} disabled={run?.status === 'running'} onChange={event => setProfileId(event.target.value)}>
                        <option value="">Select target ({profiles.length} saved)</option>
                        {profiles.map(profile => <option key={profile.id} value={profile.id}>{profile.name}</option>)}
                    </select>
                    <span className="profile-count">{profiles.length} SAVED</span>
                    {activeProfile && <code>{activeProfile.oltAddress || activeProfile.netconfAddress}</code>}
                </div>
                <div className="target-actions">
                    <button className="ghost-button" onClick={newTargetProfile}>NEW TARGET</button>
                    <button className="ghost-button" onClick={() => setShowSetup(true)}>CONFIGURE</button>
                </div>
            </header>

            <section className="workspace">
                <aside className={`rail rail-${mode}`}>
                    <div className="section-label">RUN CONTROL</div>
                    <div className={`readiness-card ${readiness?.ready ? 'ready' : 'blocked'}`}>
                        <div><span>{readiness?.ready ? 'AGENT READY' : readiness ? 'SETUP REQUIRED' : 'CHECKING'}</span><i/></div>
                        <small>{readiness?.toolNames?.length ?? 0} tools · built-in skill {readiness?.builtinSkillLoaded ? 'loaded' : 'missing'} · {readiness?.externalSkillCount ?? 0} external skills</small>
                        {readiness?.mcp?.configured && <small className={`mcp-status ${readiness.mcp.configError || readiness.mcp.servers?.some(server => server.error) ? 'error' : ''}`} title={readiness.mcp.configPath}>
                            MCP {readiness.mcp.servers?.filter(server => server.connected).length ?? 0}/{readiness.mcp.servers?.length ?? 0} connected · {readiness.mcp.servers?.reduce((total, server) => total + server.toolCount, 0) ?? 0} tools
                            {(readiness.mcp.configError || readiness.mcp.servers?.find(server => server.error)?.error) && ` · ${readiness.mcp.configError || readiness.mcp.servers?.find(server => server.error)?.error}`}
                        </small>}
                        {readiness?.mcp && !readiness.mcp.configured && <small className="mcp-status" title={readiness.mcp.configPath}>MCP not configured</small>}
                        {!!readiness?.issues?.length && <p>{readiness.issues.join(' · ')}</p>}
                    </div>
                    <div className="mode-tabs"><button className={mode === 'agent' ? 'active' : ''} onClick={() => setMode('agent')}>AGENT</button><button className={mode === 'manual' ? 'active' : ''} onClick={() => setMode('manual')}>MANUAL</button></div>
                    {mode === 'agent' ? <>
                        <div className="diagnostic-mode" aria-label="Diagnostic execution mode">
                            <button type="button" className={diagnosticMode === 'agent' ? 'active' : ''} disabled={run?.status === 'running'} onClick={() => setDiagnosticMode('agent')}>
                                <strong>STANDARD</strong><small>single ReAct agent</small>
                            </button>
                            <button type="button" className={diagnosticMode === 'team' ? 'active' : ''} disabled={run?.status === 'running'} onClick={() => setDiagnosticMode('team')}>
                                <strong>DEEP TEAM</strong><small>DAG evidence workers</small>
                            </button>
                        </div>
                        <div className="goal-heading">
                            <label className="goal-label" htmlFor="goal">What is failing?</label>
                            <button className={`history-toggle ${showHistory ? 'active' : ''}`} type="button" disabled={busy || !profileId} onClick={() => setShowHistory(current => !current)} aria-expanded={showHistory}>
                                HISTORY <span>{conversationHistory.length}</span>
                            </button>
                        </div>
                        {showHistory && <div className="history-panel">
                            <div className="history-panel-heading"><span>PAST CONVERSATIONS</span><small>{conversationHistory.length ? 'SELECT TO RESUME' : 'NO SAVED SESSIONS'}</small></div>
                            {conversationHistory.map(item => (
                                <button key={item.id} type="button" className={`history-item ${item.id === conversation?.id ? 'selected' : ''}`} onClick={() => openConversation(item)}>
                                    <span className="history-item-status">{item.status === 'active' ? '●' : '○'}</span>
                                    <div><strong>{item.title || 'Untitled diagnostic'}</strong><small>{new Date(item.updatedAt).toLocaleString([], {month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit'})}</small></div>
                                </button>
                            ))}
                        </div>}
                        <textarea id="goal" value={goal} onChange={event => setGoal(event.target.value)} placeholder="Example: OAM ACL IPv6 entry succeeds through RPC but is absent from the NBI response…"
                            onDragOver={e => { e.preventDefault(); setDragActive(true); }}
                            onDragLeave={e => { e.preventDefault(); setDragActive(false); }}
                            onDrop={e => { e.preventDefault(); setDragActive(false); handleFileSelect(e.dataTransfer.files); }}
                            onPaste={e => {
                                const items = e.clipboardData?.items;
                                if (!items) return;
                                const files: File[] = [];
                                for (const item of items) {
                                    if (item.kind === 'file') {
                                        const file = item.getAsFile();
                                        if (file) files.push(file);
                                    }
                                }
                                if (files.length > 0) {
                                    e.preventDefault();
                                    handleFileSelect(files as unknown as FileList);
                                }
                            }}
                            className={dragActive ? 'drag-active' : ''}
                        />
                        <div className="attachment-row">
                            <input ref={fileInputRef} type="file" multiple style={{display: 'none'}} onChange={e => { handleFileSelect(e.target.files); e.target.value = ''; }} />
                            <button type="button" className="attach-button" disabled={busy || attachments.length >= 10} onClick={() => fileInputRef.current?.click()}>
                                + ATTACH{attachments.length > 0 ? ` (${attachments.length})` : ''}
                            </button>
                            {attachments.map((att, idx) => (
                                <span key={idx} className="attachment-chip">
                                    <span>{att.filename}</span>
                                    <button type="button" onClick={() => removeAttachment(idx)} className="attachment-remove">×</button>
                                </span>
                            ))}
                        </div>
                        <button className="run-button" disabled={busy || run?.status === 'running' || !profileId || !goal.trim()} onClick={startDiagnostic}>
                            <span>{busy ? 'STARTING' : 'RUN DIAGNOSIS'}</span><b>↗</b>
                        </button>
                    </> : <ManualForm kind={manualKind} setKind={setManualKind} draft={manualDraft} setDraft={setManualDraft} onSubmit={executeManual} busy={busy || !profileId}/>} 
                    {error && <div className="error-strip">{error}</div>}

                    <div className="section-label spaced">EVIDENCE <b>{evidence.length}</b></div>
                    <div className="evidence-list">
                        {evidence.length === 0 && <p className="empty-copy">Captured RPC and HTTP observations will appear here.</p>}
                        {evidence.map((item, index) => (
                            <button key={item.id} className={selectedEvidence?.id === item.id ? 'evidence-item selected' : 'evidence-item'} onClick={() => setSelectedEvidence(item)}>
                                <span>{String(index + 1).padStart(2, '0')}</span>
                                <div><strong>{item.kind.replace('_', ' ')}</strong><small>{item.summary}</small></div>
                            </button>
                        ))}
                    </div>
                </aside>

                <section className="timeline-panel">
                    {mode === 'manual' ? <>
                        <div className="panel-heading">
                            <div><span className="eyebrow">MANUAL REQUEST BUILDER</span><h1>Describe the request you want to construct.</h1></div>
                            <div className="run-state"><button disabled={busy || manualMessages.length === 0} onClick={() => setManualMessages([])}>CLEAR CHAT</button><span className="status status-idle">draft only</span></div>
                        </div>
                        <div className="timeline manual-conversation">
                            <ManualBuilder messages={manualMessages} prompt={manualPrompt} setPrompt={setManualPrompt} onSubmit={buildManualDraft} busy={busy || !profileId}/>
                        </div>
                    </> : <>
                    <div className="panel-heading">
                        <div><span className="eyebrow">CONVERSATION · {conversation?.id.slice(0, 8) ?? 'loading'}</span><h1 title={run?.goal}>{run ? run.goal : conversation?.title || 'Ready for a diagnostic run'}</h1></div>
                        <div className="run-state"><button disabled={busy || run?.status === 'running' || !profileId} onClick={newConversation}>NEW CHAT</button>{conversationApproval && <span className="approval-mode" title="Later approval-required operations in this conversation run automatically">AUTO APPROVE</span>}<span className={`status status-${run?.status ?? 'idle'}`}>{run?.status ?? 'idle'}</span>{run?.status === 'running' && <button onClick={cancelDiagnostic}>STOP</button>}</div>
                    </div>
                    <div className="timeline" ref={timelineRef} onScroll={event => {
                        const element = event.currentTarget;
                        followTimelineRef.current = element.scrollHeight - element.scrollTop - element.clientHeight < 80;
                    }}>
                        {events.length === 0 && (
                            <div className="zero-state">
                                <div className="scope-grid"><i/><i/><i/><i/></div>
                                <span>NO SIGNAL YET</span>
                                <p>Select a target, state the symptom, and the agent will build an evidence trail before drawing a conclusion.</p>
                            </div>
                        )}
                        <TimelineFeed events={events} onEvidence={setSelectedEvidence}/>
                    </div>

                    {approval && run?.status === 'running' && (
                        <div className="approval-bar">
                            <div><span>APPROVAL REQUIRED</span><strong>{approval.payload.summary}</strong><small>{approval.payload.reason}</small>{approval.payload.preview && <pre>{approval.payload.preview}</pre>}</div>
                            <button onClick={() => resolveApproval(false)}>DENY</button>
                            <button className="approve" onClick={() => resolveApproval(true)}>APPROVE ONCE</button>
                            <button className="approve-chat" title="Approve this and later state-changing operations in this conversation until NEW CHAT or application restart" onClick={() => resolveApproval(true, true)}>APPROVE CHAT</button>
                        </div>
                    )}
                    </>}
                </section>

                <aside className="inspector">
                    <div className="section-label">EVIDENCE INSPECTOR</div>
                    {selectedEvidence ? (
                        <>
                            <div className="evidence-title"><span>{selectedEvidence.kind}</span><h2>{selectedEvidence.summary}</h2><code>{selectedEvidence.id}</code></div>
                            <dl className="metadata">
                                {Object.entries(selectedEvidence.metadata ?? {}).map(([key, value]) => <div key={key}><dt>{key}</dt><dd>{value}</dd></div>)}
                            </dl>
                            <EvidenceDataView data={selectedEvidence.data}/>
                        </>
                    ) : (
                        <div className="inspector-empty"><span>⌁</span><p>Choose an evidence item to inspect its raw, redacted response.</p></div>
                    )}
                </aside>
            </section>

            {showSetup && (
                <div className="setup-backdrop">
                    <section className="setup-sheet">
                        <button className="close-button" onClick={() => setShowSetup(false)}>×</button>
                        <div className="setup-intro"><span>WORKBENCH SETUP</span><h2>Connect the evidence sources.</h2><p>Prototype mode stores settings and credentials as plain text in <code>{configurationPath || 'the local application configuration file'}</code>. They are never emitted into diagnostic events.</p></div>
                        <div className="setup-tabs">
                            <button className={setupTab === 'target' ? 'active' : ''} onClick={() => setSetupTab('target')}>01 / TARGET</button>
                            <button className={setupTab === 'model' ? 'active' : ''} onClick={() => setSetupTab('model')}>02 / MODEL</button>
                        </div>
                        {setupTab === 'target' ? <TargetForm key={activeProfile?.id ?? 'new'} profile={activeProfile} onSubmit={saveTarget} busy={busy}/> : <ModelForm onSubmit={saveModel} busy={busy} settings={modelSettings}/>} 
                        {error && <div className="error-strip setup-error">{error}</div>}
                    </section>
                </div>
            )}
        </main>
    );
}

function attachmentsEqual(left: Attachment[], right: Attachment[]) {
    return left.length === right.length && left.every((attachment, index) => {
        const candidate = right[index];
        return candidate !== undefined && attachment.filename === candidate.filename && attachment.mimeType === candidate.mimeType &&
            attachment.size === candidate.size && attachment.data === candidate.data;
    });
}

function workerStepId(event: DiagnosticEvent): string {
    const payload = event.payload ?? {};
    return String(payload.workerStepId ?? payload.metadata?.['team.worker_step_id'] ?? '');
}

function TimelineFeed({events, onEvidence}: {events: DiagnosticEvent[]; onEvidence: (evidence: Evidence) => void}) {
    const teamPlanByRun = new Map<string, DiagnosticEvent>();
    const evidenceByID = new Map<string, Evidence>();
    for (const event of events) {
        if (event.type === 'team.planned' && !teamPlanByRun.has(event.runId)) teamPlanByRun.set(event.runId, event);
        if (event.type === 'evidence.captured' && event.payload?.id) evidenceByID.set(String(event.payload.id), event.payload as Evidence);
    }

    return <>{events.map(event => {
        if (event.type === 'team.planned') {
            const runEvents = events.filter(candidate => candidate.runId === event.runId);
            return <TeamExecutionBoard key={event.id} planEvent={event} events={runEvents} evidenceByID={evidenceByID} onEvidence={onEvidence}/>;
        }
        const isTeamExecutionEvent = event.type.startsWith('team.step.') || workerStepId(event) ||
            ['tool.proposed', 'tool.started', 'tool.completed', 'tool.failed', 'evidence.captured'].includes(event.type);
        if (teamPlanByRun.has(event.runId) && isTeamExecutionEvent) return null;
        return <TimelineEvent key={event.id} event={event} evidenceByID={evidenceByID} onEvidence={onEvidence}/>;
    })}</>;
}

function TeamExecutionBoard({planEvent, events, evidenceByID, onEvidence}: {
    planEvent: DiagnosticEvent;
    events: DiagnosticEvent[];
    evidenceByID: Map<string, Evidence>;
    onEvidence: (evidence: Evidence) => void;
}) {
    const steps = (Array.isArray(planEvent.payload?.steps) ? planEvent.payload.steps : []) as TeamStep[];
    const finishedByStep = new Map<string, DiagnosticEvent>();
    const startedByStep = new Map<string, DiagnosticEvent>();
    for (const event of events) {
        const stepID = String(event.payload?.stepId ?? '');
        if (event.type === 'team.step.started' && stepID) startedByStep.set(stepID, event);
        if (event.type === 'team.step.finished' && stepID) finishedByStep.set(stepID, event);
    }
    const completed = steps.filter(step => finishedByStep.has(step.id)).length;

    return (
        <article className="trace-event trace-team-board">
            <time>{new Date(planEvent.timestamp).toLocaleTimeString([], {hour12: false})}</time>
            <span className="trace-node"/>
            <div className="trace-content team-board">
                <header className="team-board-heading">
                    <div><small>team.planned</small><strong>Sub-agent steps</strong></div>
                    <b>{completed}/{steps.length} finished</b>
                </header>
                <div className="team-worker-grid">
                {steps.map((step, index) => {
                    const started = startedByStep.get(step.id);
                    const finished = finishedByStep.get(step.id);
                    const result = finished?.payload as TeamStepResult | undefined;
                    const status = result?.status ?? (started ? 'running' : 'queued');
                    const activities = events.filter(event => workerStepId(event) === step.id && ['tool.started', 'tool.failed', 'evidence.captured'].includes(event.type));
                    const evidenceIDs = Array.from(new Set([
                        ...(result?.evidenceIds ?? []),
                        ...activities.filter(event => event.type === 'evidence.captured').map(event => String(event.payload?.id ?? '')).filter(Boolean),
                    ]));
                    const referencedEvidenceIDs = Array.from(new Set(result?.referencedEvidenceIds ?? [])).filter(id => !evidenceIDs.includes(id));
                    const evidenceLinks = [
                        ...evidenceIDs.map(id => ({id, referenced: false})),
                        ...referencedEvidenceIDs.map(id => ({id, referenced: true})),
                    ];
                    const elapsed = started && finished ? Math.max(0, new Date(finished.timestamp).getTime() - new Date(started.timestamp).getTime()) : undefined;
                    return (
                        <article key={step.id} className={`team-worker-card role-${step.role} worker-${status}`}>
                            <div className="team-worker-topline">
                                <span className="worker-index">{String(index + 1).padStart(2, '0')}</span>
                                <span className="worker-role">{step.role}</span>
                                <span className={`worker-status ${status}`} title={result?.error ?? result?.unknowns?.join('\n')}>{status}</span>
                            </div>
                            <small>SUB AGENT · {step.id}</small>
                            <h3>{step.goal}</h3>
                            {(step.dependsOn?.length ?? 0) > 0 && <div className="worker-dependencies"><span>等待上游</span>{step.dependsOn!.map(id => <code key={id}>{id}</code>)}</div>}
                            <div className="worker-metrics">
                                <span>{activities.filter(item => item.type === 'tool.started').length} tools</span>
                                <span>{evidenceIDs.length} new evidence</span>
                                {referencedEvidenceIDs.length > 0 && <span>{referencedEvidenceIDs.length} referenced</span>}
                                {elapsed !== undefined && <span>{formatDuration(elapsed)}</span>}
                            </div>
                            {result?.summary && <details className="worker-result">
                                <summary>查看交接结果</summary>
                                <p>{result.summary}</p>
                                {!!result.unknowns?.length && <div className="worker-unknowns"><b>REMAINING GAPS</b>{result.unknowns.map((unknown, unknownIndex) => <span key={`${step.id}-unknown-${unknownIndex}`}>{unknown}</span>)}</div>}
                            </details>}
                            {result?.error && <p className="worker-error">{result.error}</p>}
                            {activities.length > 0 && <details className="worker-activity" open={status === 'running'}>
                                <summary>查看 {activities.length} 条执行记录</summary>
                                <div>{activities.map(activity => {
                                    const isEvidence = activity.type === 'evidence.captured';
                                    const label = isEvidence ? 'EVIDENCE' : activity.type === 'tool.failed' ? 'FAILED' : 'TOOL';
                                    const name = activity.payload?.name ?? activity.payload?.kind ?? 'operation';
                                    const detail = activity.payload?.summary ?? activity.payload?.error ?? '';
                                    return <button key={activity.id} type="button" className={isEvidence ? 'worker-activity-row evidence' : 'worker-activity-row'} disabled={!isEvidence} onClick={() => isEvidence && onEvidence(activity.payload as Evidence)}>
                                        <b>{label}</b><span><strong>{String(name).replaceAll('_', ' ')}</strong><small>{detail}</small></span>
                                    </button>;
                                })}</div>
                            </details>}
                            {evidenceLinks.length > 0 && <div className="worker-evidence-links">{evidenceLinks.map(({id, referenced}) => {
                                const evidence = evidenceByID.get(id);
                                const title = referenced ? `Referenced upstream evidence: ${id}` : `New evidence: ${id}`;
                                return <button key={id} type="button" className={referenced ? 'referenced' : undefined} disabled={!evidence} title={title} onClick={() => evidence && onEvidence(evidence)}>{id.slice(0, 8)}</button>;
                            })}</div>}
                        </article>
                    );
                })}
                </div>
            </div>
        </article>
    );
}

function formatDuration(milliseconds: number): string {
    if (milliseconds < 1000) return `${milliseconds} ms`;
    return `${(milliseconds / 1000).toFixed(milliseconds < 10000 ? 1 : 0)} s`;
}

function TimelineEvent({event, evidenceByID, onEvidence}: {event: DiagnosticEvent; evidenceByID: Map<string, Evidence>; onEvidence: (evidence: Evidence) => void}) {
    const payload = event.payload ?? {};
    const isEvidence = event.type === 'evidence.captured';
    const title: Record<string, string> = {
        'run.started': 'User request', 'agent.message': 'Agent analysis', 'tool.proposed': 'Tool proposed',
        'approval.required': 'Waiting for approval', 'approval.resolved': 'Approval resolved', 'tool.started': 'Tool executing', 'evidence.captured': 'Evidence captured',
        'tool.completed': 'Tool completed', 'tool.failed': 'Tool failed', 'team.planned': 'Team plan ready',
        'team.step.started': 'Evidence worker started', 'team.step.finished': 'Evidence worker finished',
        'run.completed': 'Diagnosis complete', 'run.failed': 'Run failed', 'run.cancelled': 'Run cancelled',
    };
    const detail = event.type === 'agent.message' ? payload.content : event.type === 'run.started' ? payload.goal : event.type === 'approval.resolved'
        ? payload.automatic ? 'Automatically approved for this conversation' : payload.scope === 'conversation' ? 'Approved; later requests in this conversation will run automatically' : payload.approved ? 'Approved once' : 'Denied'
        : payload.summary || payload.error || payload.name || '';
    const expandable = ['tool.proposed', 'tool.failed', 'approval.required', 'team.planned', 'team.step.finished', 'run.failed'].includes(event.type);
    return (
        <article className={`trace-event trace-${event.type.replace('.', '-')}`} onClick={() => isEvidence && onEvidence(payload as Evidence)}>
            <time>{new Date(event.timestamp).toLocaleTimeString([], {hour12: false})}</time>
            <span className="trace-node"/>
            <div className="trace-content"><small>{event.type}</small><strong>{title[event.type] ?? event.type}</strong>{detail && (event.type === 'agent.message' ? <MarkdownMessage content={String(detail)} evidenceByID={evidenceByID} onEvidence={onEvidence}/> : <p>{detail}</p>)}{isEvidence && <button>Inspect raw evidence</button>}{expandable && <details className="trace-details"><summary>Show details</summary><pre>{JSON.stringify(payload, null, 2)}</pre></details>}</div>
        </article>
    );
}

function TimelineEventLegacy({event, onEvidence}: {event: DiagnosticEvent; onEvidence: (evidence: Evidence) => void}) {
    const payload = event.payload ?? {};
    const isEvidence = event.type === 'evidence.captured';
    const title: Record<string, string> = {
        'run.started': 'User request', 'agent.message': 'Agent analysis', 'tool.proposed': 'Tool proposed',
        'approval.required': 'Waiting for approval', 'approval.resolved': 'Approval resolved', 'tool.started': 'Tool executing', 'evidence.captured': 'Evidence captured',
        'tool.completed': 'Tool completed', 'tool.failed': 'Tool failed', 'run.completed': 'Diagnosis complete', 'run.failed': 'Run failed', 'run.cancelled': 'Run cancelled',
    };
    const detail = event.type === 'agent.message' ? payload.content : event.type === 'run.started' ? payload.goal : event.type === 'approval.resolved'
        ? payload.automatic ? 'Automatically approved for this conversation' : payload.scope === 'conversation' ? 'Approved; later requests in this conversation will run automatically' : payload.approved ? 'Approved once' : 'Denied'
        : payload.summary || payload.error || payload.name || '';
    const expandable = ['tool.proposed', 'tool.failed', 'approval.required', 'run.failed'].includes(event.type);
    return (
        <article className={`trace-event trace-${event.type.replace('.', '-')}`} onClick={() => isEvidence && onEvidence(payload as Evidence)}>
            <time>{new Date(event.timestamp).toLocaleTimeString([], {hour12: false})}</time>
            <span className="trace-node"/>
            <div className="trace-content"><small>{event.type}</small><strong>{title[event.type] ?? event.type}</strong>{detail && <p>{detail}</p>}{isEvidence && <button>Inspect raw evidence →</button>}{expandable && <details className="trace-details"><summary>Show details</summary><pre>{JSON.stringify(payload, null, 2)}</pre></details>}</div>
        </article>
    );
}

function MarkdownMessage({content, evidenceByID, onEvidence}: {content: string; evidenceByID: Map<string, Evidence>; onEvidence: (evidence: Evidence) => void}) {
    return <div className="markdown-message">
        <ReactMarkdown
            remarkPlugins={[remarkGfm]}
            components={{
                a: ({node: _node, ...props}) => <a {...props} target="_blank" rel="noreferrer"/>,
                code: ({node: _node, children, ...props}) => {
                    const reference = resolveEvidenceReference(String(children), evidenceByID);
                    return reference
                        ? <button type="button" className="evidence-reference" title={`Inspect evidence ${reference.id}`} onClick={event => {
                            event.stopPropagation();
                            onEvidence(reference);
                        }}>{children}</button>
                        : <code {...props}>{children}</code>;
                },
                table: ({node: _node, ...props}) => <div className="markdown-table-wrap"><table className="markdown-table" {...props}/></div>,
            }}
        >{content}</ReactMarkdown>
    </div>;
}

function resolveEvidenceReference(value: string, evidenceByID: Map<string, Evidence>): Evidence | undefined {
    if (value.includes('\n')) return undefined;
    const reference = value.trim();
    if (!/^[0-9a-f]{8}(?:-[0-9a-f-]{27,})?$/i.test(reference)) return undefined;
    const exact = evidenceByID.get(reference);
    if (exact) return exact;
    const matches = Array.from(evidenceByID.entries()).filter(([id]) => id.toLowerCase().startsWith(reference.toLowerCase()));
    return matches.length === 1 ? matches[0][1] : undefined;
}

type NETCONFEndpointForm = NETCONFEndpoint & {password?: string};

function NETCONFEndpointsForm({profile}: {profile?: TargetProfile}) {
    const [enabled, setEnabled] = useState(!profile || Boolean(profile.netconfAddress) || Boolean(profile.netconfEndpoints?.length));
    const [endpoints, setEndpoints] = useState<NETCONFEndpointForm[]>(() => {
        if (profile?.netconfEndpoints?.length) {
            return profile.netconfEndpoints.map(endpoint => ({...endpoint, password: ''}));
        }
        if (profile?.netconfAddress) {
            return [{
                id: 'default',
                name: 'default',
                address: profile.netconfAddress,
                port: profile.netconfPort ?? 830,
                username: profile.netconfUsername ?? '',
                knownHostsFile: profile.knownHostsFile ?? '',
                insecureHostKey: profile.insecureHostKey,
                password: '',
                passwordConfigured: profile.netconfPasswordConfigured,
            }];
        }
        return [{id: '', name: '', address: '', port: 830, username: '', knownHostsFile: '', insecureHostKey: false, password: ''}];
    });

    function updateEndpoint(index: number, field: keyof NETCONFEndpointForm, value: string | number | boolean) {
        setEndpoints(current => current.map((endpoint, endpointIndex) => endpointIndex === index ? {...endpoint, [field]: value} : endpoint));
    }

    function addEndpoint() {
        setEndpoints(current => [...current, {id: '', name: '', address: '', port: 830, username: '', knownHostsFile: '', insecureHostKey: false, password: ''}]);
    }

    function removeEndpoint(index: number) {
        setEndpoints(current => current.filter((_, endpointIndex) => endpointIndex !== index));
    }

    return <fieldset className="netconf-endpoints-fieldset">
        <legend><label><input type="checkbox" name="netconfEnabled" checked={enabled} onChange={event => setEnabled(event.target.checked)}/> NETCONF / SSH</label></legend>
        <div className="endpoint-heading wide"><span>ONE PROFILE · MULTIPLE SSH/NETCONF PORTS</span><button type="button" className="small-action" onClick={addEndpoint}>+ ADD ENDPOINT</button></div>
        <div className="endpoint-list wide">
            {endpoints.map((endpoint, index) => <div className="endpoint-card" key={`${endpoint.id || 'new'}-${index}`}>
                <div className="endpoint-card-title"><strong>ENDPOINT {index + 1}</strong><button type="button" className="remove-action" onClick={() => removeEndpoint(index)} disabled={endpoints.length === 1}>REMOVE</button></div>
                <label><span>ID</span><input name="netconfEndpointId" value={endpoint.id} onChange={event => updateEndpoint(index, 'id', event.target.value)} placeholder="ihub" required={enabled} disabled={!enabled}/></label>
                <label><span>NAME</span><input name="netconfEndpointName" value={endpoint.name ?? ''} onChange={event => updateEndpoint(index, 'name', event.target.value)} placeholder="IHUB" disabled={!enabled}/></label>
                <label><span>ADDRESS</span><input name="netconfEndpointAddress" value={endpoint.address ?? ''} onChange={event => updateEndpoint(index, 'address', event.target.value)} placeholder="10.56.29.26" required={enabled} disabled={!enabled}/></label>
                <label><span>PORT</span><input name="netconfEndpointPort" type="number" value={endpoint.port ?? 830} onChange={event => updateEndpoint(index, 'port', Number(event.target.value))} min="1" max="65535" required={enabled} disabled={!enabled}/></label>
                <label><span>USERNAME</span><input name="netconfEndpointUsername" value={endpoint.username ?? ''} onChange={event => updateEndpoint(index, 'username', event.target.value)} autoComplete="username" required={enabled} disabled={!enabled}/></label>
                <label><span>PASSWORD</span><input name="netconfEndpointPassword" type="password" value={endpoint.password ?? ''} onChange={event => updateEndpoint(index, 'password', event.target.value)} autoComplete="new-password" placeholder={endpoint.passwordConfigured ? 'Saved — leave blank' : ''} disabled={!enabled}/></label>
                <label className="wide"><span>KNOWN HOSTS FILE</span><input name="netconfEndpointKnownHosts" value={endpoint.knownHostsFile ?? ''} onChange={event => updateEndpoint(index, 'knownHostsFile', event.target.value)} placeholder="C:\\Users\\you\\.ssh\\known_hosts" disabled={!enabled}/></label>
                <label className="check wide"><input name="netconfEndpointInsecureHostKey" type="checkbox" checked={Boolean(endpoint.insecureHostKey)} onChange={event => updateEndpoint(index, 'insecureHostKey', event.target.checked)} disabled={!enabled}/><span>Allow unknown host key for an isolated lab target</span></label>
            </div>)}
        </div>
    </fieldset>;
}

function TargetForm({profile, onSubmit, busy}: {profile?: TargetProfile; onSubmit: (event: FormEvent<HTMLFormElement>) => void; busy: boolean}) {
    const isNew = !profile;
    return <form className="setup-form" onSubmit={onSubmit}>
        <label className="wide"><span>PROFILE NAME</span><input name="name" required defaultValue={profile?.name ?? ''} placeholder="MF2 lab — 10.56.29.26"/></label>
        <label><span>ORGANIZATION CODE</span><input name="organizationCode" required defaultValue={profile?.organizationCode ?? 'A01'} placeholder="A01"/></label>
        <label className="wide"><span>ALLOWED LOCAL WORKSPACES (OPTIONAL; ONE PER LINE OR SEMICOLON-SEPARATED)</span><textarea name="workspaceRoots" defaultValue={(profile?.workspaceRoots ?? []).join('\n')} placeholder="C:\\Users\\you\\Documents\\OLT"/></label>
        <label className="wide"><span>TRUSTED SKILL FILES (OPTIONAL; MUST BE INSIDE A WORKSPACE)</span><textarea name="skillPaths" defaultValue={(profile?.skillPaths ?? []).join('\n')} placeholder="No skill file is required"/></label>
        <fieldset><legend><label><input type="checkbox" name="nbiEnabled" defaultChecked={isNew || Boolean(profile?.nbiBaseUrl)}/> NBI / REST</label></legend>
            <label className="wide"><span>ACCESS CONSOLE BASE URL</span><input name="baseUrl" defaultValue={profile?.nbiBaseUrl ?? ''} placeholder="https://10.56.29.26"/></label>
            <label><span>OLT IP</span><input name="oltAddress" defaultValue={profile?.oltAddress ?? ''} placeholder="169.254.1.253"/></label>
            <label><span>TOKEN HEADER</span><input name="tokenHeader" defaultValue={profile?.tokenHeader ?? 'token'}/></label>
            <label><span>NBI USERNAME</span><input name="nbiUsername" defaultValue={profile?.nbiUsername ?? ''} autoComplete="username"/></label>
            <label><span>NBI PASSWORD</span><input name="nbiPassword" type="password" autoComplete="new-password" placeholder={profile?.nbiPasswordConfigured ? 'Saved - leave blank to keep' : ''}/></label>
            <label className="wide"><span>STATIC TOKEN (OPTIONAL FALLBACK)</span><input name="token" type="password" autoComplete="off" placeholder={profile?.nbiTokenConfigured ? 'Saved — leave blank to keep' : 'Not needed when username and password are configured'}/></label>
            <label className="wide"><span>CUSTOM CA FILE</span><input name="caFile" defaultValue={profile?.caFile ?? ''} placeholder="C:\\certs\\lab-ca.pem"/></label>
            <label className="check wide"><input type="checkbox" name="insecureTls" defaultChecked={profile?.insecureTls}/><span>Skip TLS verification for an isolated lab target</span></label>
        </fieldset>
        <fieldset className="legacy-netconf-fieldset"><legend><label><input type="checkbox" name="legacyNetconfEnabled" defaultChecked={isNew || Boolean(profile?.netconfAddress) || Boolean(profile?.netconfEndpoints?.length)}/> NETCONF / SSH</label></legend>
            <label><span>OLT ADDRESS</span><input name="netconfAddress" defaultValue={profile?.netconfAddress ?? ''} placeholder="10.56.29.38"/></label>
            <label><span>PORT</span><input name="netconfPort" type="number" defaultValue={profile?.netconfPort ?? 830}/></label>
            <label><span>USERNAME</span><input name="username" defaultValue={profile?.netconfUsername ?? ''} autoComplete="username"/></label>
            <label><span>PASSWORD</span><input name="password" type="password" autoComplete="new-password" placeholder={profile?.netconfPasswordConfigured ? 'Saved — leave blank to keep' : ''}/></label>
            <label className="wide"><span>KNOWN HOSTS FILE</span><input name="knownHostsFile" defaultValue={profile?.knownHostsFile ?? ''} placeholder="C:\\Users\\you\\.ssh\\known_hosts"/></label>
            <label className="check wide"><input type="checkbox" name="insecureHostKey" defaultChecked={profile?.insecureHostKey}/><span>Allow unknown host key for an isolated lab target</span></label>
        </fieldset>
        <NETCONFEndpointsForm profile={profile}/>
        <button className="primary-action" disabled={busy}>{busy ? 'SAVING…' : 'SAVE TARGET →'}</button>
    </form>;
}

function ModelForm({onSubmit, busy, settings}: {onSubmit: (event: FormEvent<HTMLFormElement>) => void; busy: boolean; settings: ModelSettings}) {
    return <form className="setup-form model-form" onSubmit={onSubmit}>
        <label className="wide"><span>OPENAI-COMPATIBLE BASE URL</span><input name="modelBaseUrl" defaultValue={settings.baseUrl ?? ''} placeholder="https://api.minimaxi.com/v1"/></label>
        <label className="wide"><span>MODEL</span><input name="model" required defaultValue={settings.model ?? ''} placeholder="MiniMax-M2.7"/></label>
        <label className="wide"><span>API KEY</span><input name="apiKey" type="password" autoComplete="off" placeholder={settings.apiKeyConfigured ? 'Leave blank to keep the saved API key' : ''}/></label>
        <label><span>SETUP CHECK TIMEOUT (SECONDS)</span><input name="modelTimeout" type="number" defaultValue={settings.timeoutSeconds ?? 20} min="1" max="20"/></label>
        <label><span>MODEL CONTEXT WINDOW (TOKENS)</span><input name="contextWindowTokens" type="number" defaultValue={settings.contextWindowTokens ?? 32768} min="2048" max="2000000"/></label>
        <label><span>OUTPUT RESERVE (TOKENS)</span><input name="outputReserveTokens" type="number" defaultValue={settings.outputReserveTokens ?? 4096} min="256"/></label>
        <div className="model-note"><b>BUILT-IN OLT DIAGNOSTIC SKILL READY</b><p>External skill files are optional. The model proposes actions; the local policy engine controls execution and pauses state-changing requests for approval.</p></div>
        <button className="primary-action" disabled={busy}>{busy ? 'VERIFYING…' : 'VERIFY & SAVE AGENT →'}</button>
    </form>;
}

function ManualBuilder({messages, prompt, setPrompt, onSubmit, busy}: {
    messages: ManualBuilderMessage[];
    prompt: string;
    setPrompt: (value: string) => void;
    onSubmit: (event: FormEvent<HTMLFormElement>) => void;
    busy: boolean;
}) {
    return <form className="manual-builder" onSubmit={onSubmit}>
        <p className="manual-builder-note">Describe the read-only REST API or NETCONF RPC you need. The model drafts it with the active OLT skill, validates it locally, then fills the Direct Form. It does not log in, connect, or execute anything.</p>
        {messages.length > 0 && <div className="manual-builder-messages">
            {messages.map((message, index) => <div key={index} className={`manual-builder-message ${message.role}`}><span>{message.role === 'user' ? 'YOU' : 'BUILDER'}</span><p>{message.content}</p></div>)}
        </div>}
        <textarea value={prompt} onChange={event => setPrompt(event.target.value)} placeholder="Example: Build a REST request to list CVCs for OLT 169.254.1.253, or construct a narrow NETCONF get for an ONT." disabled={busy}/>
        <button className="run-button" disabled={busy || !prompt.trim()}><span>{busy ? 'BUILDING' : 'BUILD DRAFT'}</span><b>↗</b></button>
    </form>;
}

function ManualForm({kind, setKind, draft, setDraft, onSubmit, busy}: {
    kind: 'nbi' | 'netconf';
    setKind: (kind: 'nbi' | 'netconf') => void;
    draft: ManualDraft;
    setDraft: (draft: ManualDraft) => void;
    onSubmit: (event: FormEvent<HTMLFormElement>) => void;
    busy: boolean;
}) {
    const changeKind = (nextKind: 'nbi' | 'netconf') => {
        setKind(nextKind);
        setDraft({...draft, kind: nextKind});
    };
    return <form className="manual-form" onSubmit={onSubmit}>
        <div className="manual-form-viewport">
        <div className="manual-form-fields">
            <div className="protocol-tabs"><button type="button" className={kind === 'nbi' ? 'active' : ''} onClick={() => changeKind('nbi')}>NBI</button><button type="button" className={kind === 'netconf' ? 'active' : ''} onClick={() => changeKind('netconf')}>NETCONF</button></div>
            {kind === 'nbi' ? <>
                <label><span>METHOD</span><select name="method" value={draft.method} onChange={event => setDraft({...draft, method: event.target.value})}><option>GET</option><option>POST</option><option>PUT</option><option>PATCH</option><option>DELETE</option></select></label>
                <label><span>RELATIVE PATH</span><input name="path" required value={draft.path} onChange={event => setDraft({...draft, path: event.target.value})} placeholder="/northbound/olt/provisioning/.../{olt_ip}"/></label>
                <label><span>HEADERS (JSON)</span><textarea name="headers" value={draft.headers} onChange={event => setDraft({...draft, headers: event.target.value})} placeholder={'{"Content-Type":"application/json"}'}/></label>
                <label><span>BODY</span><textarea name="body" value={draft.body} onChange={event => setDraft({...draft, body: event.target.value})} className="manual-code" placeholder="{}"/></label>
            </> : <>
                <label><span>ENDPOINT ID</span><input name="endpoint" value={draft.endpoint} onChange={event => setDraft({...draft, endpoint: event.target.value})} placeholder="default / ihub / nt / lt1 / lt2"/></label>
                <label><span>COMPLETE RPC</span><textarea name="rpc" required className="manual-code rpc-input" value={draft.rpc} onChange={event => setDraft({...draft, rpc: event.target.value})}/></label>
                <label><span>TIMEOUT (SECONDS)</span><input name="timeoutSeconds" type="number" value={draft.timeoutSeconds} onChange={event => setDraft({...draft, timeoutSeconds: Number(event.target.value)})} min="1" max="300"/></label>
            </>}
        </div>
        </div>
        <button className="run-button" disabled={busy}><span>{busy ? 'EXECUTING' : 'EXECUTE'}</span><b>↗</b></button>
    </form>;
}

export default App;
