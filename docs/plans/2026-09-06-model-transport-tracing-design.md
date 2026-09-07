# Model transport tracing and failed-run continuation

## Goal

Make model-provider failures diagnosable from the real packaged application, and keep a failed diagnostic goal available when the user continues with a short command such as `go on`.

## Design

The configured Eino chat model is wrapped in the existing agent package. Each non-streaming generation creates a trace ID and records one terminal SQLite event, `model.http.trace`. The payload contains only operational metadata: run ID association, stage, call sequence, logical call ID, an explicit retry attempt when the host can identify it, model name, message count and estimated bytes, bound tool count, elapsed time, HTTP milestones, status/protocol, response byte count, and a redacted error classification. It never records prompts, response bodies, authorization headers, API keys, cookies, query strings, or resolved IP addresses.

The HTTP client uses a tracing `RoundTripper` around Go's default transport. `net/http/httptrace` records DNS, connect, TLS, connection reuse, request-write, and first-response-byte milestones. A counting response body records truncation without retaining content. The model wrapper combines transport state with the final Eino error so a failure can be classified as connection/TLS/request-write/waiting-for-headers/response-body/response-decode/HTTP-status. Trace persistence is best effort and never changes the model call outcome. Trace events remain hidden from the normal timeline and are available through the existing database inspector.

Before the first model call, the engine checkpoints the current user message after the retained conversation history. Team completion reuses that checkpoint instead of appending the user message twice. `Runner.Start` no longer treats pending input as a confirmed fact or overwrites an established plan. The Team Planner receives a bounded, true-role conversation history with the latest raw user message last; it decides from that ordered context whether the message continues prior work or starts a new self-contained request. There is deliberately no continuation-phrase dictionary or automatic goal substitution.

## Verification

Unit tests cover response-header EOF, truncated response bodies, empty/malformed JSON classification, HTTP errors, success, trace redaction, wrapper tool-count preservation, and Planner conversation ordering/filtering. The full Go and frontend builds must pass before producing the executable.
