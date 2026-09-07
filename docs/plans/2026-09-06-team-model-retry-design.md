# Team model retry design

## Problem

Team mode calls the Planner, Reviewer, and worker finalizer through direct
`ChatModel.Generate` calls. Unlike the standard Agent and Team workers, those
calls have no retry policy, so a transient provider `EOF` aborts the run before
workers start.

## Design

- Add one shared bounded retry helper for direct Team model generations.
- Attempt at most three times with context-aware exponential backoff.
- Reuse `modelErrorIsRetryable`: retry transport failures, EOF, 408/409/425/429,
  and 5xx responses; do not retry cancellation, deadline expiry, or provider
  4xx errors.
- Treat a nil model response as a transient empty response.
- Let each stage publish a concise retry message so the operator can distinguish
  a retry from a stalled run.
- Keep JSON parsing and validation outside the retry helper. Invalid plans are
  deterministic model output problems and must not resend the same request.

## Verification

- A Planner that returns `unexpected EOF` once succeeds on the second attempt.
- Cancellation is returned immediately without another provider call.
- Existing Planner/Reviewer reasoning and Team DAG tests remain green.
- `wails build` produces the Windows executable from the updated workspace.
