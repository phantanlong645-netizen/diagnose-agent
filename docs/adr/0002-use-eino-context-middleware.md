# ADR 0002: Use Eino middleware for bounded diagnostic context

## Status

Accepted

## Context

The diagnostic agent needs to keep a conversation usable after several NBI,
NETCONF, file, and shell tool calls. Raw XML and file output can be much larger
than the model needs for the next decision. Replaying the complete tool output
also makes provider context limits unpredictable.

The project already uses CloudWeGo Eino for the agent loop and tool execution.
Reimplementing the loop or introducing another agent framework would duplicate
state handling and make interrupt/approval behavior harder to reason about.

## Decision

Use Eino ADK and middleware for model-facing orchestration:

- `adk.NewChatModelAgent` and `adk.NewRunner` own the agent loop.
- `compose.ToolsNode` and `toolutils.InferTool` own model tool calls.
- Eino summarization middleware owns compaction timing and summary generation.
- A custom Eino `TokenCounter` counts the original context, including tool
  schemas and raw tool output, so large responses trigger compaction.
- A custom Eino `GenModelInput` projects history before summary generation. It
  preserves the original request, recent message blocks, evidence IDs, and
  errors while removing duplicated system prompts and large raw payloads.
- The SQLite journal remains the durable source for complete evidence. A
  read-only `read_evidence` Eino tool retrieves a specific raw evidence item on
  demand.

The application layer is limited to OLT-specific concerns: target profiles,
approval policy, SQLite persistence, and evidence lookup. It does not contain a
second model/tool loop.

## Consequences

Positive:

- Eino remains the single orchestration framework.
- Large responses no longer have to be sent to the summary model unchanged.
- The model can recover exact raw data by evidence ID when needed.
- Approval and tool policy remain enforced by the local host.

Trade-offs:

- The compact projection is intentionally lossy; raw evidence must be fetched
  when exact XML or file content is required.
- Token thresholds remain model-profile-dependent and should be made
  configurable when provider context limits are known.
- SQLite event lookup is linear within one run; this is adequate for the local
  diagnostic workload and can later be indexed with a dedicated evidence table.
