# System message normalization

## Problem

The main agent and summarization model each build requests from an instruction
and pinned diagnostic memory. Both sections currently use the `system` role, so
the resulting OpenAI-compatible request starts with two system messages. Some
providers require exactly one system message at the beginning and reject this
shape with HTTP 400.

## Design

Merge independently generated system sections into one leading system message
at the model request boundary. Preserve section order and separate non-empty
sections with blank lines. Keep the diagnostic memory and runtime soft
constraint unchanged so their instruction priority and refresh behavior remain
the same. Apply the normalization to both the main agent and summarization model
paths.

## Verification

Unit tests verify that instruction, diagnostic memory, and runtime constraint
all survive the merge, and that nil or empty sections do not create an empty
system message. The existing agent tests and the complete Go test suite must
continue to pass.
