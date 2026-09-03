# Access Console Business Log Tool Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a model-callable tool that downloads the authenticated Access Console business-log ZIP, safely extracts it in memory, and returns bounded redacted excerpts as diagnostic evidence.

**Architecture:** Keep `nbi_request` restricted to `/northbound/`. Add a separate `collect_access_console_logs` tool that shares the existing `NBITool` target, TLS client, login, token cache, token renewal, and retry behavior, while owning the fixed `/nms/v1/log/download` route and ZIP/text processing. Do not persist the raw archive or place it in SQLite; persist only its digest, manifest, and filtered excerpts.

**Tech Stack:** Go, `net/http`, `archive/zip`, Eino tool schemas, existing Runner/Evidence pipeline, Access Console JWT authentication.

---

## Requirements

### Functional

- Authenticate using the selected target profile without asking for credentials again.
- Download only `GET /nms/v1/log/download` from the configured Access Console origin.
- Select known business-log files and search them with explicit keywords and bounded context.
- Return source filename, line range, redacted text, archive hash, file manifest, and truncation state.
- Make the tool available to the diagnostic agent and document when to use it in both skill copies.

### Non-functional

- Keep `/northbound/` validation unchanged for ordinary NBI calls.
- Limit compressed archive size to 64 MiB, each entry to 16 MiB, total extracted text to 64 MiB, and model-visible evidence to 64 KiB.
- Reject malformed ZIPs, unsafe entry names, unexpected binary entries, and cross-origin redirects.
- Never store the raw ZIP, JWT, password, cookies, or authorization headers in evidence.
- Treat collection as read-only but non-idempotent so the Runner cannot reuse stale logs across turns.

## High-level architecture

```text
Target Profile
     |
     v
shared NBITool session (origin + TLS + JWT cache + renewal)
     |--------------------------|
     v                          v
nbi_request              collect_access_console_logs
/northbound/*             fixed /nms/v1/log/download
JSON/XML text             ZIP bytes (memory only)
                                |
                                v
                     validate -> extract -> filter -> redact
                                |
                                v
                      bounded structured Evidence
```

## ADR-001: Separate tool, shared authenticated session

**Status:** Accepted

**Context:** The existing NBI tool accepts arbitrary verified `/northbound/` paths and reads at most 2 MiB as text. The log endpoint is an authenticated internal UI route that returns a potentially much larger ZIP and generates a fresh server-side snapshot.

**Decision:** Add a separate logical tool and inject the existing `NBITool` into it. Reuse private session methods inside the same tools package, but do not relax the NBI route prefix.

**Consequences:** Authentication stays single-sourced and token refresh behavior remains consistent. Binary limits and stale-result behavior can differ safely. The log tool is intentionally coupled to Access Console rather than becoming a generic downloader.

**Alternatives considered:** Extending `nbi_request` was rejected because it would mix binary and text contracts and weaken the path boundary. Shell/curl was rejected because it would duplicate authentication and risk exposing credentials. SSH/SFTP was rejected because these are Access Console application logs, not OLT filesystem logs.

## Failure handling

| Failure | Handling |
|---|---|
| Expired JWT | Clear the matching cached token, log in once, retry once |
| Cross-origin redirect | Reject using the existing NBI redirect policy |
| HTTP error or HTML page | Return a redacted bounded error; do not attempt ZIP parsing |
| Oversized archive/entry | Stop before unbounded allocation and report the exact limit |
| Invalid ZIP or unsafe name | Reject the archive; do not partially trust it |
| No keyword matches | Return the manifest and an explicit zero-match result |
| Excess matches | Return the first bounded excerpts and mark `truncated=true` |

### Task 1: Add ZIP collection and analysis

**Files:**
- Modify: `internal/tools/nbi.go`
- Test: `internal/tools/local_test.go`

1. Add failing tests for authenticated ZIP download, token reuse, redaction, file selection, and limits.
2. Run `go test ./internal/tools` and confirm the tests fail before implementation.
3. Implement the tool in `nbi.go`, sharing the existing `NBITool` instance.
4. Run `go test ./internal/tools` and confirm it passes.

### Task 2: Register and expose the tool

**Files:**
- Modify: `app.go`
- Modify: `internal/agent/engine.go`
- Test: `internal/agent/engine_test.go`

1. Register the concrete tool beside `nbi_request`.
2. Add its Eino schema and handler to the agent tool list.
3. Add regression assertions for the tool guidance and schema.
4. Run `go test ./internal/agent ./internal/application`.

### Task 3: Update diagnostic guidance

**Files:**
- Modify: `internal/agent/builtin/skill.md`
- Modify: `docs/skills/olt-netconf-diagnostic/SKILL.md`

1. Document the verified route, included logs, application-log semantics, and focused single-call workflow.
2. Validate the external skill with `quick_validate.py` in UTF-8 mode.
3. Run `go test ./...`, `go vet ./...`, and `npm run build`.

