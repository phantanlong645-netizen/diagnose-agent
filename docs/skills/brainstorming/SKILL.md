---
name: brainstorming
description: Requirements and design discovery for new OLT Diagnostic Agent features, tools, workflows, and UI behavior. Use before creating a substantial feature or changing architecture, tool contracts, persistence, or agent behavior.
---

# Requirements and design discovery

Use this skill before substantial feature work. Do not add architecture only because it sounds useful; first establish the user's goal, constraints, and success criteria.

## Understand the request

1. Inspect the current project structure, relevant code, documentation, and recent behavior.
2. Restate the problem in concrete terms, including the target user and the observable result.
3. Identify constraints: OLT/device scope, read-only versus write behavior, approval requirements, supported builds, persistence, UI limits, and unavailable live access.
4. Ask one focused question at a time only when the answer materially changes the design. Do not ask for information already present in the selected profile, workspace, source code, or conversation history.

## Explore options

Present two or three viable approaches when the design is not already determined. For each approach, state:

- how it fits the existing architecture;
- implementation and maintenance cost;
- failure and safety behavior;
- impact on the model context and user experience.

Lead with a recommendation and explain the evidence behind it. Prefer the smallest design that satisfies the actual requirement.

## Validate the design

Present the design in short sections:

1. Boundaries and responsibilities.
2. Data flow and tool contracts.
3. Error handling, approval, and retry behavior.
4. Persistence and UI behavior.
5. Verification and rollout plan.

Do not start implementation if a missing user decision would materially change the design. For a minor detail, make a safe assumption, record it, and continue.

## OLT-specific design rules

- Prefer existing Access Console routes, typed RPC builders, and verified `.tpl` recipes over model-invented XML.
- Keep read-only observation separate from configuration writes.
- Make destructive or state-changing actions explicit and approval-gated.
- Use exact filters and bounded projections; never design a workflow around an unrestricted datastore dump.
- Treat an OLT reply, source line, route, or template as evidence and distinguish observed facts from inference.
- Keep device-specific rules in a recipe/catalog or reference file instead of embedding them in every handler.

## After approval

Write an implementation plan or design note only when it is useful for the project. Then implement in small, independently verifiable steps. Do not commit or push the design or code unless the user explicitly requests it.
