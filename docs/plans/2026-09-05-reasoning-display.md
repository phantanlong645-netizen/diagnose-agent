# Model Reasoning Display Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Display provider-supplied Eino reasoning in a restrained, collapsed debug view for the main Agent and isolated Team workers without retaining it in conversation memory.

**Architecture:** Extract only reasoning explicitly returned in `schema.Message.ReasoningContent`, assistant reasoning parts, or `<think>` blocks. Redact and rune-limit the text before publishing a persisted `agent.reasoning` event; attach host-owned worker metadata through the existing context mechanism. Keep reasoning out of checkpoints and render events collapsed in the existing timeline and worker cards.

**Tech Stack:** Go 1.25, CloudWeGo Eino v0.9.13, SQLite event journal, Wails v2, React/TypeScript.

---

### Task 1: Define a safe reasoning event

**Files:**
- Modify: `internal/domain/types.go`
- Modify: `internal/application/runner.go`

1. Add the `agent.reasoning` event type.
2. Add a Runner publisher that accepts a stage, redacted content, and optional reasoning-token count.
3. Attach `workerStepId` and `workerRole` from the existing host context.

### Task 2: Extract and isolate provider reasoning

**Files:**
- Modify: `internal/agent/engine.go`
- Test: `internal/agent/engine_test.go`

1. Test extraction from `ReasoningContent`, assistant reasoning parts, and `<think>` blocks.
2. Test deduplication, secret redaction, and bounded Unicode-safe output.
3. Test that retained conversation messages clear reasoning fields and reasoning parts.
4. Publish reasoning from the Standard Agent event loop without changing visible assistant messages.

### Task 3: Cover Team model stages

**Files:**
- Modify: `internal/agent/team.go`
- Test: `internal/agent/engine_test.go`

1. Add optional reasoning callbacks to Planner and Reviewer.
2. Publish worker reasoning with host-owned worker metadata.
3. Publish no-tool worker-finalizer reasoning.
4. Keep reasoning unavailable when the provider returns no displayable content.

### Task 4: Render a restrained collapsed view

**Files:**
- Modify: `frontend/src/App.tsx`
- Modify: `frontend/src/App.css`

1. Render main, Planner, and Reviewer reasoning as collapsed timeline events.
2. Group worker reasoning inside the matching Sub-agent card.
3. Reuse the current typography, borders, spacing, and neutral color palette.
4. Label the view as debug model reasoning and show token count only when supplied.

### Task 5: Verify

1. Run focused Agent tests.
2. Run the frontend production build.
3. Run a project compile check.
4. Inspect the final diff and preserve the pre-existing `engine.go` comment-only changes.
