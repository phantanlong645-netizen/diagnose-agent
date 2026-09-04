export namespace domain {
	export class MCPServerStatus {
	    name: string;
	    transport: string;
	    connected: boolean;
	    toolCount: number;
	    error?: string;

	    static createFrom(source: any = {}) {
	        return new MCPServerStatus(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.transport = source["transport"];
	        this.connected = source["connected"];
	        this.toolCount = source["toolCount"];
	        this.error = source["error"];
	    }
	}
	export class MCPStatus {
	    configPath: string;
	    configured: boolean;
	    configError?: string;
	    servers?: MCPServerStatus[];

	    static createFrom(source: any = {}) {
	        return new MCPStatus(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.configPath = source["configPath"];
	        this.configured = source["configured"];
	        this.configError = source["configError"];
	        this.servers = this.convertValues(source["servers"], MCPServerStatus);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class AgentReadiness {
	    ready: boolean;
	    targetConfigured: boolean;
	    modelConfigured: boolean;
	    builtinSkillLoaded: boolean;
	    externalSkillCount: number;
	    toolNames: string[];
	    mcp: MCPStatus;
	    issues?: string[];
	
	    static createFrom(source: any = {}) {
	        return new AgentReadiness(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ready = source["ready"];
	        this.targetConfigured = source["targetConfigured"];
	        this.modelConfigured = source["modelConfigured"];
	        this.builtinSkillLoaded = source["builtinSkillLoaded"];
	        this.externalSkillCount = source["externalSkillCount"];
	        this.toolNames = source["toolNames"];
	        this.mcp = this.convertValues(source["mcp"], MCPStatus);
	        this.issues = source["issues"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Attachment {
	    filename: string;
	    mimeType: string;
	    data: string;
	
	    static createFrom(source: any = {}) {
	        return new Attachment(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.filename = source["filename"];
	        this.mimeType = source["mimeType"];
	        this.data = source["data"];
	    }
	}
	export class Conversation {
	    id: string;
	    profileId: string;
	    title: string;
	    status: string;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    updatedAt: any;
	
	    static createFrom(source: any = {}) {
	        return new Conversation(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.profileId = source["profileId"];
	        this.title = source["title"];
	        this.status = source["status"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.updatedAt = this.convertValues(source["updatedAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class DiagnosticRequest {
	    goal: string;
	    profileId: string;
	    mode?: string;
	    attachments?: Attachment[];
	
	    static createFrom(source: any = {}) {
	        return new DiagnosticRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.goal = source["goal"];
	        this.profileId = source["profileId"];
	        this.mode = source["mode"];
	        this.attachments = this.convertValues(source["attachments"], Attachment);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Event {
	    id: string;
	    conversationId: string;
	    runId: string;
	    type: string;
	    // Go type: time
	    timestamp: any;
	    payload?: any;
	
	    static createFrom(source: any = {}) {
	        return new Event(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.conversationId = source["conversationId"];
	        this.runId = source["runId"];
	        this.type = source["type"];
	        this.timestamp = this.convertValues(source["timestamp"], null);
	        this.payload = source["payload"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Evidence {
	    id: string;
	    runId: string;
	    callId: string;
	    kind: string;
	    summary: string;
	    data?: any;
	    message?: string;
	    metadata?: Record<string, string>;
	    // Go type: time
	    capturedAt: any;
	
	    static createFrom(source: any = {}) {
	        return new Evidence(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.runId = source["runId"];
	        this.callId = source["callId"];
	        this.kind = source["kind"];
	        this.summary = source["summary"];
	        this.data = source["data"];
	        this.message = source["message"];
	        this.metadata = source["metadata"];
	        this.capturedAt = this.convertValues(source["capturedAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ImageAttachment {
	    filename: string;
	    mimeType: string;
	    data: string;
	
	    static createFrom(source: any = {}) {
	        return new ImageAttachment(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.filename = source["filename"];
	        this.mimeType = source["mimeType"];
	        this.data = source["data"];
	    }
	}
	export class ManualDraft {
	    kind: string;
	    method?: string;
	    path?: string;
	    headers?: Record<string, string>;
	    body?: string;
	    endpoint?: string;
	    rpc?: string;
	    timeoutSeconds?: number;
	
	    static createFrom(source: any = {}) {
	        return new ManualDraft(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.method = source["method"];
	        this.path = source["path"];
	        this.headers = source["headers"];
	        this.body = source["body"];
	        this.endpoint = source["endpoint"];
	        this.rpc = source["rpc"];
	        this.timeoutSeconds = source["timeoutSeconds"];
	    }
	}
	export class ManualDraftMessage {
	    role: string;
	    content: string;
	
	    static createFrom(source: any = {}) {
	        return new ManualDraftMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.role = source["role"];
	        this.content = source["content"];
	    }
	}
	export class ManualDraftRequest {
	    profileId: string;
	    messages: ManualDraftMessage[];
	
	    static createFrom(source: any = {}) {
	        return new ManualDraftRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.profileId = source["profileId"];
	        this.messages = this.convertValues(source["messages"], ManualDraftMessage);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ManualDraftResponse {
	    message: string;
	    draft: ManualDraft;
	    valid: boolean;
	    validationErrors?: string[];
	
	    static createFrom(source: any = {}) {
	        return new ManualDraftResponse(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.message = source["message"];
	        this.draft = this.convertValues(source["draft"], ManualDraft);
	        this.valid = source["valid"];
	        this.validationErrors = source["validationErrors"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ModelSettings {
	    baseUrl?: string;
	    apiKey?: string;
	    model: string;
	    timeoutSeconds?: number;
	    contextWindowTokens?: number;
	    outputReserveTokens?: number;
	
	    static createFrom(source: any = {}) {
	        return new ModelSettings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.baseUrl = source["baseUrl"];
	        this.apiKey = source["apiKey"];
	        this.model = source["model"];
	        this.timeoutSeconds = source["timeoutSeconds"];
	        this.contextWindowTokens = source["contextWindowTokens"];
	        this.outputReserveTokens = source["outputReserveTokens"];
	    }
	}
	export class ModelSettingsSummary {
	    baseUrl?: string;
	    model: string;
	    timeoutSeconds: number;
	    contextWindowTokens: number;
	    outputReserveTokens: number;
	    configured: boolean;
	    apiKeyConfigured: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ModelSettingsSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.baseUrl = source["baseUrl"];
	        this.model = source["model"];
	        this.timeoutSeconds = source["timeoutSeconds"];
	        this.contextWindowTokens = source["contextWindowTokens"];
	        this.outputReserveTokens = source["outputReserveTokens"];
	        this.configured = source["configured"];
	        this.apiKeyConfigured = source["apiKeyConfigured"];
	    }
	}
	export class NBITarget {
	    baseUrl: string;
	    oltAddress?: string;
	    username?: string;
	    password?: string;
	    tokenHeader?: string;
	    token?: string;
	    caFile?: string;
	    insecureTls: boolean;
	
	    static createFrom(source: any = {}) {
	        return new NBITarget(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.baseUrl = source["baseUrl"];
	        this.oltAddress = source["oltAddress"];
	        this.username = source["username"];
	        this.password = source["password"];
	        this.tokenHeader = source["tokenHeader"];
	        this.token = source["token"];
	        this.caFile = source["caFile"];
	        this.insecureTls = source["insecureTls"];
	    }
	}
	export class NETCONFEndpoint {
	    id: string;
	    name?: string;
	    address: string;
	    port: number;
	    username: string;
	    password?: string;
	    knownHostsFile?: string;
	    insecureHostKey: boolean;
	
	    static createFrom(source: any = {}) {
	        return new NETCONFEndpoint(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.address = source["address"];
	        this.port = source["port"];
	        this.username = source["username"];
	        this.password = source["password"];
	        this.knownHostsFile = source["knownHostsFile"];
	        this.insecureHostKey = source["insecureHostKey"];
	    }
	}
	export class NETCONFEndpointSummary {
	    id: string;
	    name?: string;
	    address?: string;
	    port?: number;
	    username?: string;
	    knownHostsFile?: string;
	    insecureHostKey: boolean;
	    passwordConfigured: boolean;
	
	    static createFrom(source: any = {}) {
	        return new NETCONFEndpointSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.address = source["address"];
	        this.port = source["port"];
	        this.username = source["username"];
	        this.knownHostsFile = source["knownHostsFile"];
	        this.insecureHostKey = source["insecureHostKey"];
	        this.passwordConfigured = source["passwordConfigured"];
	    }
	}
	export class NETCONFTarget {
	    address: string;
	    port: number;
	    username: string;
	    password?: string;
	    knownHostsFile?: string;
	    insecureHostKey: boolean;
	
	    static createFrom(source: any = {}) {
	        return new NETCONFTarget(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.address = source["address"];
	        this.port = source["port"];
	        this.username = source["username"];
	        this.password = source["password"];
	        this.knownHostsFile = source["knownHostsFile"];
	        this.insecureHostKey = source["insecureHostKey"];
	    }
	}
	export class Run {
	    id: string;
	    conversationId: string;
	    profileId: string;
	    goal: string;
	    mode: string;
	    images?: ImageAttachment[];
	    status: string;
	    // Go type: time
	    startedAt: any;
	    // Go type: time
	    endedAt?: any;
	
	    static createFrom(source: any = {}) {
	        return new Run(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.conversationId = source["conversationId"];
	        this.profileId = source["profileId"];
	        this.goal = source["goal"];
	        this.mode = source["mode"];
	        this.images = this.convertValues(source["images"], ImageAttachment);
	        this.status = source["status"];
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.endedAt = this.convertValues(source["endedAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class TargetProfile {
	    id: string;
	    name: string;
	    organizationCode?: string;
	    nbi?: NBITarget;
	    netconf?: NETCONFTarget;
	    netconfEndpoints?: NETCONFEndpoint[];
	    workspaceRoots?: string[];
	    skillPaths?: string[];
	
	    static createFrom(source: any = {}) {
	        return new TargetProfile(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.organizationCode = source["organizationCode"];
	        this.nbi = this.convertValues(source["nbi"], NBITarget);
	        this.netconf = this.convertValues(source["netconf"], NETCONFTarget);
	        this.netconfEndpoints = this.convertValues(source["netconfEndpoints"], NETCONFEndpoint);
	        this.workspaceRoots = source["workspaceRoots"];
	        this.skillPaths = source["skillPaths"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class TargetProfileSummary {
	    id: string;
	    name: string;
	    organizationCode?: string;
	    nbiBaseUrl?: string;
	    oltAddress?: string;
	    nbiUsername?: string;
	    tokenHeader?: string;
	    caFile?: string;
	    insecureTls: boolean;
	    nbiTokenConfigured: boolean;
	    nbiPasswordConfigured: boolean;
	    netconfAddress?: string;
	    netconfPort?: number;
	    netconfUsername?: string;
	    knownHostsFile?: string;
	    insecureHostKey: boolean;
	    netconfPasswordConfigured: boolean;
	    netconfEndpoints?: NETCONFEndpointSummary[];
	    workspaceRoots?: string[];
	    skillPaths?: string[];
	
	    static createFrom(source: any = {}) {
	        return new TargetProfileSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.organizationCode = source["organizationCode"];
	        this.nbiBaseUrl = source["nbiBaseUrl"];
	        this.oltAddress = source["oltAddress"];
	        this.nbiUsername = source["nbiUsername"];
	        this.tokenHeader = source["tokenHeader"];
	        this.caFile = source["caFile"];
	        this.insecureTls = source["insecureTls"];
	        this.nbiTokenConfigured = source["nbiTokenConfigured"];
	        this.nbiPasswordConfigured = source["nbiPasswordConfigured"];
	        this.netconfAddress = source["netconfAddress"];
	        this.netconfPort = source["netconfPort"];
	        this.netconfUsername = source["netconfUsername"];
	        this.knownHostsFile = source["knownHostsFile"];
	        this.insecureHostKey = source["insecureHostKey"];
	        this.netconfPasswordConfigured = source["netconfPasswordConfigured"];
	        this.netconfEndpoints = this.convertValues(source["netconfEndpoints"], NETCONFEndpointSummary);
	        this.workspaceRoots = source["workspaceRoots"];
	        this.skillPaths = source["skillPaths"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace tools {
	
	export class Definition {
	    name: string;
	    description: string;
	
	    static createFrom(source: any = {}) {
	        return new Definition(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.description = source["description"];
	    }
	}

}
