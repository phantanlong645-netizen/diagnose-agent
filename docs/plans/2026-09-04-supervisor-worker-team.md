# Supervisor/Worker Team Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add an explicit deep-diagnosis mode that runs isolated, read-only sub-agents through a dependency-aware DAG and shares only structured results plus persisted evidence references.

**Architecture:** Standard diagnosis keeps the existing ReAct agent and parallel tool calls. Team diagnosis uses an LLM planner to create bounded role-based steps, executes ready steps concurrently with isolated Eino agents, stores every tool result under the parent run, passes dependency `StepResult` values to downstream steps, and lets a reviewer produce the single conversation answer.

**Tech Stack:** Go 1.25, CloudWeGo Eino ADK, SQLite journal, Wails v2, React/TypeScript.

---

### Task 1: Persist and expose the execution mode

**Files:**
- Modify: `internal/domain/types.go`
- Modify: `internal/application/runner.go`
- Modify: `internal/application/runtime.go`
- Modify: `app.go`

1. Add validated `agent`, `team`, and `manual` diagnostic modes.
2. Carry the mode on `DiagnosticRequest` and `Run`.
3. Add a schema migration for `runs.mode` and persist it with every run.
4. Route `team` runs to `Engine.RunTeam`; keep `agent` as the default.
5. Verify existing databases migrate and a new run records its selected mode.

### Task 2: Turn Team into a data-carrying DAG

**Files:**
- Modify: `internal/agent/team.go`
- Modify: `internal/agent/engine_test.go`

1. Replace string-only worker output with structured `TeamStepResult` containing status, summary, facts, evidence IDs, unknowns, and error text.
2. Pass dependency results explicitly in `TeamWorkInput`.
3. Restrict planner output to supported read-only roles and a bounded number of steps.
4. Preserve failed and skipped nodes for reviewer visibility instead of silently omitting them.
5. Emit structured team/step progress callbacks.

### Task 3: Implement isolated evidence workers

**Files:**
- Modify: `internal/agent/team.go`
- Modify: `internal/agent/engine.go`

1. Build one fresh Eino ChatModelAgent per DAG step with no parent conversation history.
2. Supply only the original goal, the step goal, selected target context, and dependency summaries/evidence IDs.
3. Derive tool allowlists on the host from the worker role; do not trust model-provided tool permissions.
4. Execute tools through the existing parent `Runner`, so policy, approval, events, evidence persistence, and `read_evidence` remain shared.
5. Collect evidence IDs directly from tool-result events and merge them into the structured worker result.
6. Bound each worker to a small iteration budget and propagate cancellation.

### Task 4: Complete and checkpoint team runs

**Files:**
- Modify: `internal/agent/team.go`
- Modify: `internal/application/runner.go`

1. Publish plan, step start, completion, failure, and skip events.
2. Review every step result, including partial/failed/skipped results.
3. Append only the user goal and final reviewed answer to conversation history; discard worker message histories.
4. Complete or fail the parent run through the existing lifecycle methods.

### Task 5: Add the explicit UI mode

**Files:**
- Modify: `frontend/src/App.tsx`
- Modify: `frontend/src/App.css`
- Modify: `frontend/wailsjs/go/models.ts`

1. Add a `STANDARD` / `DEEP TEAM` selector inside Agent mode.
2. Send `mode: agent|team` in `StartDiagnostic`.
3. Display structured team events with readable titles and details.
4. Keep Manual mode behavior unchanged.

### Task 6: Verify the integration

1. Format changed Go files with `gofmt`.
2. Run focused Go tests for planner validation, DAG dependency-result propagation, skip handling, and mode normalization.
3. Run `go test ./...` as a compile/integration check.
4. Run `npm run build` in `frontend` for TypeScript and production bundle validation.
5. Inspect `git diff` to ensure unrelated dirty-worktree changes were preserved.

### Task 7: Bound source-worker repository search

1. Add a host-validated `sourceSearch` brief with exact terms, likely owner paths, runtime-evidence dependency, and a maximum search count.
2. Wire runtime-derived source steps behind a live platform/device result without introducing DAG cycles.
3. Reuse the profile's persistent `search_code` index, but cap source workers at four search calls and eight semantic candidates per call.
4. Keep reads available after the search cap so the worker can inspect and report the strongest matches instead of spending its entire budget searching.
5. Verify normalization, traversal rejection, dependency wiring, cycle avoidance, and concurrent budget enforcement.

#### Architecture decision: guided agent search, not a scripted search tool

- **Decision:** Keep Source as an isolated reasoning worker, but give it a structured `sourceSearch` contract and enforce search/result budgets in the host.
- **Why:** A plain tool chain is faster when every query is known in advance, but it cannot decide which definition resolves a newly observed device state. A fully autonomous worker handles that ambiguity but tends to repeat broad searches. The bounded contract preserves the useful judgment while limiting cost and latency.
- **Data flow:** Planner supplies known literals and likely owner paths. If a literal must come from live data, the DAG passes the upstream platform/device result and evidence IDs first. The worker then uses the shared profile index and returns only its evidence report.
- **Failure behavior:** Unsafe owner paths are rejected, dependency wiring avoids cycles, semantic search rebuilds are disabled inside workers, and exceeding the search budget produces an explicit tool observation so the worker can finalize a partial result.
- **Trade-off:** Owner paths are guidance rather than a second filesystem authorization boundary; the existing workspace-root policy remains authoritative. This avoids duplicating path-policy logic while the hard search and result caps prevent repository-wide fanout.

### Task 8: Add role-scoped MCP tools

**Goal:** Make external MCP tools observable and usable without weakening the Team worker read-only boundary.

**Architecture:** `mcp.json` remains the host-authoritative configuration. Every enabled MCP tool is available to Standard Agent; unclassified tools retain open-world approval, while locally classified read-only tools follow the existing read-only policy. A tool enters Deep Team only when its policy explicitly sets `readOnly: true` and binds one or more supported worker roles; remote MCP annotations alone never grant Team access.

1. Extend each MCP server with per-remote-tool `toolPolicies` containing `enabled`, `readOnly`, `idempotent`, `sensitive`, and `teamRoles`.
2. Preserve default behavior for unlisted tools: enabled for Standard Agent, open-world, approval required, and unavailable to Team.
3. Filter configured read-only MCP tools into the matching Team role while retaining the role's built-in allowlist.
4. Send `notifications/initialized` after the legacy MCP handshake and make stdio response dispatch safe for concurrent calls.
5. Record config, connection, discovery, and tool-count status without exposing header or environment secrets.
6. Surface a compact MCP status line in Agent readiness; MCP remains optional and does not block built-in diagnosis.
7. Add policy, role-filtering, lifecycle, and concurrent-response tests in existing test files; run focused Go tests and the frontend production build.

#### Security decision: local policy overrides remote claims

- **Decision:** Only local `toolPolicies` may grant Team access. Server-provided `annotations.readOnlyHint` can be parsed for diagnostics later, but cannot authorize execution.
- **Why:** An MCP server is an external trust boundary and may mislabel a mutating tool. Host-owned least-privilege policy keeps worker execution deterministic and auditable.
- **Failure behavior:** Missing configuration means no MCP tools; malformed configuration or a failed server is reported in readiness; one failed server does not prevent other servers or built-in tools from loading.

### Task 9: Distinguish collected and referenced evidence

1. Keep `evidenceIds` as the evidence newly captured by a worker.
2. Add host-validated `referencedEvidenceIds` for dependency evidence actually cited in the worker report.
3. Allow a correlator with a normal report and valid upstream references to succeed without making redundant tool calls.
4. Mark tool failures, iteration-limit exits, and source-only empty search results as partial.
5. Show new and referenced evidence separately while keeping both kinds clickable in the existing evidence inspector.
