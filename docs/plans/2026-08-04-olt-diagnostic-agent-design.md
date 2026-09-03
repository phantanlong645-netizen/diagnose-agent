# OLT Diagnostic Agent Design

## Goal

Build a local-first Windows desktop application that helps engineers diagnose OLT provisioning failures. The application combines an agent loop with direct NETCONF access, Access Console northbound API access, local documentation, YANG schemas, request collections, and controlled file or shell tools. Every conclusion must be backed by captured evidence such as an RPC reply, HTTP response, log excerpt, or configuration diff.

## Scope

The first version supports one local user and one diagnostic run at a time. It can manage persistent target and model settings, execute read-only tools, request approval for state-changing operations, record structured observations, and present a final diagnosis. Multi-agent orchestration, cloud synchronization, unrestricted shell access, Docker runtimes, and production NETCONF write workflows are deliberately excluded from the first milestone.

## Architecture

The product is a modular monolith packaged with Wails. React and TypeScript render the desktop interface. Go owns the application layer, policy enforcement, persistence, tools, and agent orchestration. Eino is an adapter behind an internal agent interface; domain and connector packages must not depend on Eino types.

The main boundaries are:

- `domain`: run, step, tool call, approval, evidence, finding, and target profile models.
- `application`: diagnostic run orchestration and event publication.
- `agent`: framework-neutral agent interface and Eino implementation.
- `tools`: typed NETCONF, NBI HTTP, filesystem, shell, and knowledge tools.
- `policy`: deterministic risk classification and approval decisions.
- `target`: in-memory validated target profiles restored from the local application configuration.
- `transport`: Wails bindings and UI event mapping.

## Execution Flow

1. The user selects an NBI and/or NETCONF target profile and describes the problem. A profile may contain multiple named NETCONF endpoints (for example `ihub`, `nt`, `lt1`, and `lt2`) that share an OLT chassis but use different SSH ports.
2. The agent proposes a typed tool call.
3. The policy layer classifies it independently of the model.
4. Read-only operations may run automatically. State-changing operations pause for approval.
5. The executor returns a structured observation and stores redacted raw evidence.
6. The agent continues until it produces findings or reaches a configured limit.
7. The UI presents the event timeline, evidence, and final diagnosis.

### Multi-endpoint NETCONF decision

One diagnostic conversation must be able to correlate state from IHUB, NT, and LT components. Keeping those ports as separate target profiles would split the conversation context and make a single diagnosis unnecessarily stateful. Therefore the profile stores a list of named NETCONF endpoints, and each `netconf_rpc` call selects an endpoint ID. Legacy profiles with one `netconf` object are treated as a single `default` endpoint during migration.

Live operational-state questions start with read-only NBI or NETCONF evidence. Workspace search is used only when the exact request is unknown, a live request fails, or the user explicitly asks for source or documentation analysis. A built-in workflow is always available; optional trusted skill files refine it but are never required and are not discovered by workspace search.

## Configuration Persistence

The runnable prototype stores target profiles and model configuration in `%AppData%\OLT Diagnostic Agent\config.json`. Secrets are deliberately stored as plain text for this milestone, while frontend summaries expose only whether a secret is configured. Leaving a secret field blank while editing preserves the saved value. Operating-system credential storage is required before distribution.

## Conversation Persistence

Conversation history is stored separately in `%AppData%\OLT Diagnostic Agent\diagnostics.db`. SQLite is the local system of record; process memory is only a cache for the currently executing run. Each target profile may have one active conversation and any number of archived conversations. A run always belongs to exactly one conversation.

Persistence has two deliberately separate representations:

- The audit stream is append-only: conversations contain runs, and runs contain ordered diagnostic events. It preserves user goals, approvals, tool activity, evidence, failures, and final answers for UI reconstruction and later diagnosis review.
- The effective model context is a versioned snapshot per conversation. It contains the Eino message sequence used by the next turn. Automatic summarization replaces this snapshot transactionally without deleting the full audit stream.

Run completion and context replacement are committed together. A failed or cancelled run remains visible in the audit stream but cannot replace the last known-good model context with a partial tool-call sequence. Database migrations are monotonic and recorded in `schema_migrations`. SQLite runs with foreign keys, WAL journaling, a busy timeout, and one process-managed connection pool. A new-chat operation archives the current conversation and creates a fresh active conversation without deleting historical data.

## Safety

Skills provide instructions and domain knowledge but never grant permissions. Each tool declares whether it is read-only, destructive, idempotent, or open-world. NETCONF `edit-config`, `commit`, REST `POST`/`PUT`/`PATCH`/`DELETE`, file writes, and non-read-only shell commands require explicit approval. Secrets must not be written to run logs, request collections, or evidence files.

## Evidence Model

Each tool execution records a request summary, target, timestamps, duration, result status, structured output, redacted raw artifact location, and optional correlation identifiers such as HTTP request IDs, NETCONF session IDs, and RPC message IDs. HTTP evidence includes status code, content type, redirect information, headers, and body metadata so that an HTML fallback response cannot be mistaken for a successful JSON API call.

## Initial Verification

- Unit tests for tool risk classification and approval decisions.
- Unit tests for HTTP response classification, including HTTP 200 with HTML content.
- Unit tests for event ordering and diagnostic run limits.
- Build verification for Go and React/TypeScript.
- A local demonstration tool that produces observable evidence without contacting an OLT.
