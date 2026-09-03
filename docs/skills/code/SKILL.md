---
name: code
description: Structured coding workflow for the OLT Diagnostic Agent and its Go, React/TypeScript, configuration, and documentation changes. Use when the user asks to implement, refactor, debug, review, or verify project code.
---

# Coding workflow

Use this workflow for implementation and code changes. Keep the work evidence-driven and scoped to the user's request.

## Before editing

1. Identify the active repository and current working directory.
2. Inspect the relevant files, call sites, types, configuration, and existing tests before proposing a change.
3. Preserve unrelated or pre-existing worktree changes.
4. State the intended scope and identify any uncertainty that could change the design.
5. For a new feature or behavior change, make a short implementation plan before editing. For a small, obvious fix, keep the plan inline and proceed.

## Implementation rules

- Follow the existing Go, React/TypeScript, naming, error-handling, and file-layout conventions.
- Prefer the smallest coherent change. Do not introduce a framework or abstraction merely to avoid a few lines of code.
- Keep domain models, application orchestration, tools, policy, and UI responsibilities separate.
- Use typed tool arguments and deterministic validation for operations that affect an OLT or the local filesystem.
- Do not put credentials, tokens, or passwords into logs, evidence, prompts, source files, or documentation.
- Do not commit, amend, push, rebase, or change remote review state unless the user explicitly asks.
- Do not delegate to another agent unless the user explicitly asks for delegation.
- Preserve XML, JSON, protocol names, and device errors exactly when explaining them; translate the surrounding explanation into Chinese by default.

## Verification

Choose verification proportional to risk:

- Go changes: run `gofmt` and targeted package tests or compilation when permitted.
- React/TypeScript changes: run the TypeScript check or frontend build when permitted.
- UI changes: inspect the affected layout and verify long text, tables, errors, and narrow windows.
- RPC/NBI changes: validate request shape and route/template source before contacting a live target.
- Documentation-only changes: check links, code blocks, paths, and examples.

If the user says not to test or compile, do not run those commands; perform only safe static checks and clearly report what remains unverified.

## Delivery

Report:

1. What changed and why.
2. The files that changed.
3. What was verified and what was not.
4. Any required restart, configuration, approval, or manual live-device check.

Never claim a live OLT behavior is confirmed when only source inspection or XML validation was performed.
