# NETCONF session close design

## Problem

Each diagnostic RPC opens a dedicated SSH NETCONF session. The transport closes
the SSH channel and TCP connection after reading the business reply, but it does
not perform the NETCONF `<close-session>` handshake. Some OLTs retain those
sessions as operational and eventually refuse new connections.

## Design

After every completed business RPC, send a base NETCONF `<close-session>` RPC on
the same session and wait for a matching `<rpc-reply><ok/></rpc-reply>`. Support
both NETCONF 1.0 delimiter framing and NETCONF 1.1 chunked framing. Give cleanup
up to five seconds while continuing to honor caller cancellation. Skip the
automatic handshake when the requested operation is already `close-session`.

A cleanup failure must not discard a successful business reply or encourage the
agent to repeat a potentially state-changing operation. Return the business
result with explicit close status and a cleanup warning; deferred SSH and TCP
closes remain the final fallback on every path.

## Verification

Unit tests cover NETCONF 1.0 and 1.1 request framing, acknowledgement parsing,
message-ID mismatch, and non-OK replies. The full Go test suite must pass.
