# OLT Diagnostic Agent

Local-first Windows workbench for diagnosing OLT provisioning failures. It combines an LLM agent loop with evidence-backed analysis over Access Console NBI, NETCONF, source code, logs, YANG modules, and product documentation.

## Features

- **Wails v2 desktop shell** with a React + TypeScript diagnostic console.
- **Eino ReAct agent loop** driven by an OpenAI-compatible chat model.
- **Explicit Deep Team mode** — an LLM planner builds a bounded DAG, isolated read-only workers investigate NETCONF, REST/logs, source/docs, or public-web evidence in parallel, and a reviewer joins structured results plus persisted evidence IDs.
- **Built-in OLT diagnostic workflow** — works without any external skill files; optional trusted `SKILL.md` files can be attached per target profile.
- **Typed tool connectors**:
  - `nbi_request` — Access Console NBI (read-only HTTP requests)
  - `collect_access_console_logs` — OLT provisioning log collection
  - `netconf_rpc` — NETCONF over SSH
  - `search_files` / `read_file` / `write_file` / `run_shell` — workspace-scoped tools
  - `web_search` / `web_fetch` — read-only web search and page fetch (SearXNG, SerpAPI, or DuckDuckGo backends)
  - `code_search` — RAG-based semantic code search over configured workspace roots
  - `read_evidence` — resolve evidence collected during a run
  - MCP tools — stdio/HTTP MCP servers declared in the application config directory's `mcp.json`
- **Deterministic policy enforcement** outside the model:
  - Read-only NBI, NETCONF, and ordinary workspace reads run automatically.
  - HTTP writes, NETCONF configuration RPCs, local writes, shell commands, and sensitive file reads pause for explicit one-time approval (optionally per-conversation).
  - MCP tools are treated as open-world and always require user approval.
- **Attachments** — text and PDF attachments are injected into the goal; image attachments are sent to the model as multimodal content.
- **Structured run events** — approvals, redacted evidence, raw-response inspection, and a live trace that includes context compaction.
- **Manual diagnostic sessions** — run a single tool call manually, with natural-language draft generation that never contacts the OLT.
- **Persistent conversation history** — SQLite-backed, survives application restarts; `NEW CHAT` archives the active conversation without deleting it.

## Architecture

- Wails v2 application shell binding a Go backend to a React/TypeScript frontend.
- `internal/agent` — the Eino ReAct engine and built-in diagnostic workflow.
- `internal/application` — run orchestration, journal, approvals, and evidence.
- `internal/tools` — tool implementations and the tool registry.
- `internal/policy` — deterministic approval policy engine.
- `internal/domain` — shared domain types.
- `internal/target` — target profile store.
- `internal/rag` — semantic code search indexing.

Model responses use non-streaming chat completion. A standard diagnostic run may use up to 64 agent iterations with no whole-run deadline; it continues until the task is complete, the user cancels it, or a genuinely user-only blocker remains. Conversation history is compacted automatically when it exceeds 48 messages or 80% of the safe model-context budget, preserving the goal, evidence, failures, and pending work. Deep Team workers use isolated histories and at most 8 iterations each; only their structured results and evidence references are shared.

Model configuration is verified with a small chat request (20-second window) before a run starts; invalid base URLs, API keys, and model names are rejected early.

## Security model

- Relative NBI paths cannot escape the configured host.
- Deep Team workers are host-enforced read-only even when an exposed NBI or NETCONF tool also supports state-changing operations in standard mode.
- File tools cannot escape the configured workspace roots, including through symbolic links.
- Shell commands start in an allowed workspace and always require approval, but are not an OS sandbox and may access anything permitted to the desktop process.
- Target and model credentials are never returned in profile summaries, model settings, or diagnostic events.

> **Prototype notice**: during the prototype stage, credentials are persisted as plain text in the local user configuration file. Conversation history and audit data are kept in a SQLite database. Both must be migrated to OS credential storage before distribution.

## Data storage

- Configuration: `%AppData%\OLT Diagnostic Agent\config.json` (profiles + model settings).
- Conversation/run/event history: `%AppData%\OLT Diagnostic Agent\diagnostics.db` (append-only SQLite). A completed diagnostic turn updates its run state and context snapshot in one transaction, so the next turn for the same target resumes the conversation even after a restart.
- RAG index: persisted under the application config directory (`rag/`); workspace-root drift triggers automatic reindexing.

## Target profiles

A profile can contain:

- Access Console base URL, optional OLT IP substitution, token, and TLS policy.
- OLT NETCONF address, SSH credentials, and host-key policy (including named endpoints).
- Optional local workspace roots containing logs, source, YANG modules, or documents.
- Optional trusted `SKILL.md` instructions.

The application exposes a readiness gate (target, model, required tools, optional external skills) before a diagnostic run can start.

## Development

Requirements: Go, Node.js, npm, and Wails v2.

```powershell
cd frontend
npm install
cd ..
wails dev
```

Build a production bundle with `wails build`.

## Project layout

```
app.go, main.go      Wails application shell and startup
cmd/dbinspect        Temporary SQLite inspection helper (not shipped)
config/              Example MCP server configuration
docs/adr             Architecture decision records
docs/plans           Design documents
docs/skills          Bundled agent skills
frontend/            React + TypeScript console
internal/agent       Agent engine and built-in workflow
internal/application Run orchestration, journal, approvals, evidence
internal/domain      Domain types
internal/policy      Approval policy engine
internal/rag         Semantic code search index
internal/target      Target profile store
internal/tools       Tool implementations and registry
```

## Documentation

- Initial design: [docs/plans/2026-08-04-olt-diagnostic-agent-design.md](docs/plans/2026-08-04-olt-diagnostic-agent-design.md)
- Architecture decision records: [docs/adr](docs/adr)
- Bundled skills: [docs/skills](docs/skills)
