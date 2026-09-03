# ADR-0001: Use SQLite for durable conversation history

## Status

Accepted

## Context

The desktop agent must continue a diagnostic conversation across multiple runs and application restarts. The history includes structured runs, ordered events, evidence payloads, approvals, and the effective Eino model context. It must remain isolated by target profile, survive crashes, support atomic updates, and avoid turning the credential-oriented `config.json` into an unbounded transcript file. The product is a single-user, local-first Windows application and does not need a database server.

## Decision

Use an embedded SQLite database at `%AppData%\OLT Diagnostic Agent\diagnostics.db`, accessed through `database/sql` and the pure-Go `modernc.org/sqlite` driver.

Store the append-only audit stream (`conversations`, `runs`, and `events`) separately from the replaceable, versioned model context (`conversation_contexts`). Allow one active conversation per target profile through a partial unique index. Persist run completion and its final model-context snapshot in one transaction. Keep target and model configuration in `config.json`.

Enable foreign keys, WAL journaling, and a busy timeout. Apply numbered, monotonic migrations recorded in `schema_migrations`.

## Consequences

### Positive

- Conversations survive application restarts without an external service.
- Transactions keep run state and model context consistent.
- Full audit history remains available after context summarization.
- Profile isolation prevents evidence from different OLTs entering the same prompt.
- Schema migrations support future conversation search and retention features.

### Negative

- The application gains a database dependency and lifecycle management.
- Evidence stored on disk may be sensitive and requires later encryption or retention controls.
- Corruption and migration failures must be surfaced during startup rather than silently ignored.

### Neutral

- The database is local and single-user; it is not designed for synchronization between machines.
- `config.json` remains the prototype credential store until OS-backed secret storage is implemented.

## Alternatives Considered

**Append history to `config.json`**

Rejected because updates are whole-file rewrites, growth is unbounded, querying is poor, and configuration and audit data have different lifecycles.

**Keep history only in memory**

Rejected because application restart or crash loses the conversation.

**Store every model-state snapshot as the transcript**

Rejected because each snapshot repeats the full prior context and grows quadratically. The chosen design stores an append-only event audit plus one current context snapshot.

**Run PostgreSQL or another external database**

Rejected because a server process adds deployment and operational complexity without benefit for a single-user desktop workload.
