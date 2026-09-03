<!--
此文件是内置诊断技能的编译进二进制版本。
外部参考副本位于 docs/skills/olt-netconf-diagnostic/SKILL.md。
修改任一份时请同步另一份，避免行为不一致。
-->

---
name: olt-netconf-diagnostic
description: Evidence-driven diagnostics for Nokia OLTs through Access Console NBI, NETCONF, typed RPC builders, and verified XML templates. Use for OLT state/configuration investigations, route or YANG discovery, NETCONF RPC generation, TPL rendering, RPC failures, and comparing frontend/API/device configuration.
---

# OLT NETCONF/NBI diagnostic workflow

Use this skill when investigating an OLT, writing or validating a NETCONF RPC, using an Access Console `.tpl`, diagnosing an NBI/NETCONF failure, or comparing configured and operational state.

## Response language

Write all visible progress messages, explanations, findings, and final answers in Simplified Chinese by default. Keep XML, JSON keys, code identifiers, file paths, protocol names, and original OLT/NBI error text unchanged, then explain them in Chinese. Follow another language only when the user explicitly requests it.

## Non-negotiable query rule

Never send an unfiltered `<get-config>` or `<get>` through `netconf_rpc`. The runtime rejects these requests because they commonly return the entire datastore and can exhaust the model context. A request is not made safe merely by adding `<source><running/></source>`; it still needs a narrow `<filter>`.

Before every NETCONF call, state internally:

```text
goal -> exact object -> exact path -> exact filter -> expected fields
```

If any of these is unknown, search the configured Access Console source or documentation first. Do not replace a missing exact path with a full configuration dump. If a filtered query returns `<data/>`, broaden the filter's content progressively — drop one discriminator, then move up to the parent container. Accepting some redundant siblings in the reply is preferred over chasing the exact leaf node (small path/namespace mismatches often return empty). The runtime requires a `<filter>` element; removing it is blocked, but widening the filter's content is always allowed.

## Source-of-truth order

Use evidence in this order:

1. For an unverified REST request, the preferred current REST API document entry, found by exact filename plus a focused path/business query.
2. The registered Access Console route/controller for writes, ambiguity, missing documentation, version drift, or a live response that conflicts with the document; typed RPC builders and existing service methods remain the first authority for NETCONF behavior.
3. A version/device-specific Access Console template (`.tpl`).
4. The configured YANG documentation and schema files.
5. A narrowly filtered live NETCONF/NBI query.
6. Model-generated XML only as a fallback when no verified recipe exists.

Do not invent Nokia namespaces, node names, component names, interface names, or parameter values when source evidence is available.

## Access Console source map and request lookup

Use this map before selecting a REST path, NETCONF endpoint, or XML shape. The Access Console source is the authority; do not treat a similarly named UI label as a protocol contract.

| Need | Verify first | Then follow |
|---|---|---|
| NBI REST path, method, parameters, and body | focused `search_files` query in `server/internal/routers/nbi/doc/REST_API_Doc_V0618.md` | for writes/ambiguity/version drift, `server/internal/routers/nbi/ext.go`, then matching `{olt,onu,service,system}/routers.go` and controller/service |
| OLT/chassis behavior | `server/internal/routers/nbi/olt/` | `server/internal/services/olt/{inventory,provisioning,...}` and `template/olt/` |
| ONU/user-side behavior | `server/internal/routers/nbi/onu/` | `server/internal/services/onu/{inventory,provisioning,...}` and `template/onu/` |
| Service instance behavior | `server/internal/routers/nbi/service/` | `server/internal/services/internal/service/`, `common/models/dto_service_instance.go`, and `template/service/<release>/` |
| XML rendering and parameter semantics | `server/internal/pkgs/rpc/rpc.go` | template paths are rendered through `RunStaticFSRPC`/`RunStaticRPC`; preserve their declared placeholders and helper semantics |
| Endpoint/port mapping | `server/internal/pkgs/mapper/olt/mapper.go` | use `GetLtPortFromNode` for the selected LT; do not infer component ownership from the port number alone |
| ONU/service naming | `server/internal/pkgs/naming/naming.go` | derive names using the owning naming function; do not synthesize a near-looking identifier |

The NBI root is `/northbound`. Router groups produce paths beneath `/northbound/olt`, `/northbound/onu`, `/northbound/service`, and `/northbound/system`. Build a path from router registration, not from controller names.

### REST API lookup fast path

Before executing a REST request whose exact contract has not already been verified in the current conversation, call `search_files` once with `pattern="REST_API_Doc_V0618.md"`, a distinctive path fragment or business keyword in `query`, and a small result limit. Choose the detailed match rather than the early table-of-contents match, then call `read_file` with that `startLine` and a bounded `maxLines` (normally 80-150). Never load or summarize the whole document. Record the matched method, path pattern, path/query parameters, request body, and documented response as the verified contract so later turns reuse it.

- A complete document entry is enough to execute a read-only GET/HEAD/OPTIONS request.
- Before POST/PUT/PATCH/DELETE, inspect the matching router/controller after the document lookup to verify validation, side effects, write-lease behavior, and implementation changes not reflected in the document.
- If the document has no exact match, change the focused query once (for example path fragment to handler/business term), then inspect the owning router. Do not broaden to every Markdown or Go file.
- If the user supplies an exact path, search by the stable path fragment without concrete IP, serial number, or ID values. If the user supplies only intent, search by the documented object/action term.
- If the document conflicts with the registered source or live response, treat the source/live behavior as implementation evidence and report the documentation drift.
- Do not re-read a document entry already verified in the current conversation.

### Access Console business-log evidence

Use `collect_access_console_logs` when a live Access Console operation fails and application-side evidence can distinguish routing, validation, orchestration, device-reply, job, or panic failures. This tool reads the Access Console server's business logs; it does **not** read files from the OLT filesystem.

The verified download chain is `GET /nms/v1/log/download` in `server/internal/routers/inner/v1/log/routers.go`, followed by its controller/service and `server/internal/pkgs/log/logger.go`. The route is authenticated but is outside `/northbound/`, so never construct it with `nbi_request`. The host reuses the selected profile's Access Console login session, downloads the ZIP in memory, validates archive names and size limits, and returns only bounded redacted excerpts. Raw archives, passwords, and tokens must never enter the model context or diagnostic database.

The current download service bundles `alarm.log`, `olt.log`, `ont.log`, `oss.log`, `syslog.log`, `chain.log`, `panic.log`, `panicN.log`, and `job.log`. Logger source may define other files, but do not assume they are present in the downloaded ZIP. A missing selected file is evidence about the server bundle, not permission to guess a filesystem path or use the shell.

Choose filters from facts already observed:

- REST/NBI failure: start with the exact route fragment, status/error text, OLT address, and object identifier; prefer `olt.log`, `ont.log`, `oss.log`, `chain.log`, or `job.log` according to the owning operation.
- ONU/ONT failure: include the serial number, AID, LT endpoint, or service identifier and prefer `ont.log` plus the owning orchestration log.
- Crash or unexplained HTTP 500: include the operation/path and timestamp fragment when known; add `panic.log` and `panicN.log` rather than downloading them repeatedly.
- Alarm/syslog question: select `alarm.log` or `syslog.log` and use the exact alarm name, equipment identifier, or message fragment.

Make one focused collection after the route, object, and failure text are known. Keywords are case-insensitive OR matches, so use a small set of distinctive values rather than generic words such as `error` alone. Use the returned context lines and evidence ID to correlate the failure with source, NBI, or NETCONF evidence. Do not poll the tool, request unrestricted dumps, repeat an unchanged collection, or claim that absence of a keyword proves absence of the failure.

#### Efficient log-analysis funnel

Treat log analysis as a bounded evidence search, not a reading task. Never ask the model to inspect every line or summarize an entire archive. Follow this sequence:

1. **Build a failure fingerprint before collecting logs.** Extract the operation, exact REST path or RPC name, HTTP/RPC error text, target OLT, ONU/ONT/AVC/service identifier, component, and reported timestamp from the user message and existing evidence. Normalize only obvious formatting differences; preserve original identifiers and errors.
2. **Choose the smallest log set.** Select one or two owning logs from the mapping above. Add `panic.log`/`panicN.log` only for HTTP 500, crash, stack trace, or unexplained process failure. Do not select all files merely because the bundle contains them.
3. **Use high-information keywords.** Prefer, in order: unique object identifier; exact route/RPC/handler fragment; distinctive device error; OLT address; narrow timestamp fragment. Use 2-6 terms. Avoid weak terms by themselves, including `error`, `failed`, `request`, `response`, `OLT`, `ONU`, `HTTP 500`, or a common source filename.
4. **Bound the first pass.** Normally request `contextLines=2` and `maxMatches=40`. Increase context to at most 5 only when a multiline stack trace or request/reply sequence is visible. Do not increase limits simply because no match was found.
5. **Rank evidence instead of retelling it.** Group returned excerpts mentally by the same error signature, source location, object identifier, and adjacent timestamp. Report the most relevant 1-3 signatures with occurrence count visible in the evidence, first/last observed time when available, and one representative excerpt/evidence ID. Do not restate duplicate lines.
6. **Correlate before expanding.** Compare the leading signature with the verified router/controller/service/template path and the failed REST or NETCONF operation. If that explains the failure, stop. Expand only one unresolved branch with a new distinctive keyword or one additional owning log.
7. **Use controlled relaxation on zero matches.** Relax exactly one dimension: remove the least reliable keyword, try the owning adjacent log, or shorten an over-precise route/object fragment. Never fall back to empty keywords, every log file, unrestricted shell/file reads, or a repeated identical collection.

Use this decision gate before every collection:

```text
known operation + (object ID or distinctive error/path) -> focused collection
known operation but no distinctive term                 -> inspect route/source or ask for failure detail first
unknown operation and no timestamp/object/error          -> do not collect logs yet
```

Stop log analysis when one of these conditions is met:

- a log signature plus source/live evidence supports a defensible root cause;
- the focused search has no match after one controlled relaxation;
- the remaining question requires a missing timestamp, object identifier, target, or user action;
- further log collection would only duplicate an existing signature.

In the final diagnosis, state **Observed**, **Inferred**, and **Unresolved** separately. Absence of a match means only that the selected files and keywords did not match the downloaded snapshot; it never proves that the event did not occur.

### Verified read-only REST starting points

Use these only when the operator's intent matches exactly; preserve each path parameter's meaning from the router/controller.

| Operator intent | Verified method and path pattern | Required identity |
|---|---|---|
| List CVCs for an OLT | `GET /northbound/olt/provisioning/cvc/:olt_ip` | OLT IP |
| Inspect one ONU's service list | `GET /northbound/onu/services/:sn` | ONU serial number |
| Find the PON/ONU mapping | `GET /northbound/onu/pon_onu_map/:olt_ip/:lt_port` | OLT IP and LT port |
| Inspect static IPv4/IPv6 entries on an ONU | `GET /northbound/onu/static_ipv4/:sn` or `GET /northbound/onu/static_ipv6/:sn` | ONU serial number |
| Inspect service instances for a device | `GET /northbound/service/service_instances/:sn_ip` | the route's combined device identifier |
| Inspect a channel termination | `GET /northbound/olt/provisioning/channel_termination/:lt/:cage/:olt_ip` | LT, cage, and OLT IP |

Do not turn a route which is registered as `POST`, `PUT`, or `DELETE` into a read query. For a request absent from this table, locate its exact router registration before producing a REST draft. If source lookup is unavailable in a Manual Builder turn, ask for a supported route/recipe or request a diagnostic investigation instead of inventing a path.

### Component and template ownership

| Object family or wording | Correct component | Primary source/template area | Minimum identity before an RPC |
|---|---|---|---|
| Chassis, hardware, fan, power, software, IHUB service state | `ihub` | `template/olt/`, OLT inventory/service sources | component or requested field |
| CVC, network-side forwarding, NBI-facing configuration, uplink/network port | `nt` unless the verified source says otherwise | OLT provisioning and NT templates | CVC/uplink/interface identity |
| ONU, PON, channel termination, UNI, GEM, T-CONT, v-enet, subscriber profile, user-side VSI | hosting `lt1`/`lt2` | `server/internal/pkgs/rpc/oltAction.go`, `template/onu/`, ONU provisioning, ONU service templates | LT endpoint; require ONT/AID and port/service identity only for an individual resource, not an ONT list |
| Service provisioning recipe | LT for ONU-facing resource; correlate NT/IHUB only when the service source explicitly does so | `template/service/<release>/` and ONU service templates | OLT release, parent service/template, ONT and service keys |
| GBC / baseline configuration | component-specific GBC file; not a default diagnostic recipe | `template/gbc/<release>/<board>/` | release and board type |

### Verified recipe: list all online ONTs on one LT

Access Console implements this directly in `server/internal/pkgs/rpc/oltAction.go` as `GetOnlineOnt`. It sends the RPC to one LT and selects `interfaces-state/interface` entries whose type is `bbf-xponift:channel-termination`. The LT endpoint is the collection boundary, so an ONT name/AID is neither required nor appropriate. This returns ONTs currently present on the LT's channel terminations; describe it as an online/detected ONT list.

When the operator says “all ONTs”, “ONT list”, or “query the ONTs on lt1”, use the known LT endpoint and construct this read-only draft immediately:

```xml
<rpc message-id="1" xmlns="urn:ietf:params:xml:ns:netconf:base:1.0">
  <get>
    <filter type="subtree">
      <interfaces-state xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces">
        <interface>
          <type xmlns:bbf-xponift="urn:bbf:yang:bbf-xpon-if-type">bbf-xponift:channel-termination</type>
        </interface>
      </interfaces-state>
    </filter>
  </get>
</rpc>
```

Do not ask for an ONT name, AID, channel-termination name, or a different recipe after the LT endpoint is known. Those identifiers are required only when the requested result concerns one ONT or one subordinate resource. If the user asks for configured ONT objects rather than online/detected ONTs, state that semantic difference and use a separately verified configured-ONT recipe.

For a user-side VSI, the source naming function constructs the ONU interface form as `PortId + "/VSI_" + RealServiceType`; RB service inspection uses `template/onu/service/getServiceRB_revert.tpl` and includes this value beneath the ONU mounted interface. A user-side VSI is therefore not an IHUB VPLS or VP object. Do not emit a raw VSI RPC until the hosting LT and a bounded ONT/port/service identity are known.

## Select the RPC construction mode

Classify the operation before calling a tool:

- **Typed recipe**: use a code-defined RPC builder when the repository has one. This is preferred for stable, structured queries because the XML shape and fields are explicit.
- **Template recipe**: use a known `.tpl` when the original Access Console already encodes device/version-specific XML. The template is executable XML source, not a prose example.
- **Raw RPC**: generate a complete NETCONF `<rpc>` only when no typed or template recipe matches, or when the user explicitly requests expert RPC experimentation.

Never ask the model to edit an arbitrary template path or send template source directly to the OLT. Select a logical recipe ID; trusted Go code maps that ID to an allowlisted template or builder. Do not choose backup/config-snapshot templates for ordinary state questions; they are broad by design.

## Render a template recipe

Treat an Access Console template as an operation body that must be rendered by code:

1. Resolve a whitelisted template path under the configured Access Console source tree, normally `server/internal/pkgs/template_fs/template/...`.
2. Load it with the same semantics as `RunStaticFSRPC`/`RunStaticRPC`: Go `text/template`, the repository's approved helper functions, and only the recipe's declared parameters.
3. Reject missing parameters, unresolved `{{...}}` or `[[...]]` delimiters, unexpected template functions, and paths outside the allowlist.
4. Parse the rendered result as XML. Require exactly one operation root such as `get`, `get-config`, `edit-config`, `commit`, or another explicitly approved NETCONF operation.
5. Verify the operation and namespaces against the recipe metadata.
6. The current Agent transport expects a complete `<rpc>` document. If the template renders an operation body, wrap it in an RFC 6241 `<rpc>` with a generated message ID. If it already renders `<rpc>`, validate and use it without nesting another envelope.
7. Send the validated XML through the NETCONF tool and retain the rendered request as redacted evidence.

The template defines the XML structure. The recipe metadata defines the parameter schema, allowed values, target component mapping, read/write classification, and response projection. Do not infer all of those constraints from placeholder names alone.

## NETCONF endpoint selection

One target profile can contain multiple named SSH/NETCONF endpoints for the same chassis. MF2 commonly exposes `ihub`, `nt`, `lt1`, and `lt2` on different ports. Pass the endpoint ID with every `netconf_rpc` call when more than one endpoint is configured. A task that needs evidence from several components must issue separate narrow RPCs and correlate their replies; never assume the LT port contains IHUB or NT state. Legacy profiles with one endpoint use the ID `default`.

Example recipe metadata:

```text
id: olt.lt.info
source: template
template: template/olt/getLtInfo.tpl
parameters: LtIndex(string, required, lt1|lt2|...)
operation: get
readOnly: true
projection: hardware-state.component
```

For a template containing `Slot-Lt-{{.LtIndex}}_board`, pass the validated value `LtIndex=2`; do not let the model rewrite the XML structure.

## Generate a raw RPC only when necessary

The `netconf_rpc` tool requires `rpc` to be a JSON **string**, not an object. Use this shape:

```json
{
  "endpoint": "lt2",
  "rpc": "<?xml version=\"1.0\" encoding=\"UTF-8\"?><rpc xmlns=\"urn:ietf:params:xml:ns:netconf:base:1.0\" message-id=\"1\"><get>...</get></rpc>",
  "timeoutSeconds": 30
}
```

Before sending a raw RPC, verify:

- the document root is exactly `<rpc>`;
- `message-id` is present;
- there is one valid NETCONF operation inside;
- all namespaces are explicit and sourced from repository/YANG evidence;
- no unresolved template placeholders remain;
- a read query has a narrow subtree/filter. For `get` and `get-config`, a filter is mandatory; do not call the tool without one.

Use a filter that identifies the requested object, for example an exact interface name, ONT name/AID, LT/cage, filter name plus entry ID, or the exact YANG leaf being compared. Do not use a top-level container alone if it can return every object below it.

Never pass `{"rpc":{"timeoutSeconds":120}}` or put `timeoutSeconds` inside the XML/string. If the tool rejects the JSON shape, correct the shape once; the rejected call did not reach the OLT.

## Query-size and state rules

Prefer `get` for operational state and `get-config` with an exact subtree for running configuration. Do not start with an unrestricted `get-config`; the runtime rejects it. Templates such as backup/config snapshots may return megabytes and are diagnostic-only, never the default first query. Do not work around the rejection by switching to an unfiltered `get` or by asking the shell to fetch the datastore.

When a reply is large:

1. Preserve the redacted full reply as evidence for the UI/database.
2. Return only a bounded summary and the requested fields to the model.
3. Replace broad queries with an exact interface, ONU, LT, cage, entry ID, or filter.

An empty `<data/>` means that the filter did not match or the data is absent. It is not permission to scan the entire datastore without first narrowing or checking the exact source definition. A large reply is evidence that the filter is still too broad; narrow it before continuing.

## Read/write and retry policy

- Start with read-only NBI/NETCONF evidence.
- `edit-config`, `commit`, REST `POST`/`PUT`/`PATCH`/`DELETE`, shell writes, and file writes require explicit approval from the host policy.
- Access Console REST writes also require the single-holder NBI write lease. After login, call `POST /northbound/auth/permissions` with the same JWT and a body such as `{"Duration":"600"}` before the first REST write (the permission call itself is the prerequisite exception), then reuse that lease and JWT until its returned expiry. The permission call is itself state-changing and approval-gated; do not reacquire an active lease, and treat HTTP 403 lease-occupied responses as a blocker to report rather than retry unchanged. Release it with `DELETE /northbound/auth/permissions` after the approved write sequence when appropriate. This lease rule is for NBI REST only, not NETCONF RPC writes.
- Do not repeat an unchanged failed request.
- After an RPC error, classify it as syntax, namespace/path, parameter/value, capability, authentication, transport, or device-policy failure; then consult the relevant route/template/YANG source and make one materially narrower or corrected attempt.
- A successful `<ok/>` proves acceptance by NETCONF, not necessarily that the service is operational. Follow with a precise state query when the user asks whether it took effect.

## Evidence-based answer

For every conclusion, distinguish:

- **Observed**: exact RPC/NBI request, reply, status, error, source line, or template content.
- **Inferred**: the likely reason based on observed evidence.
- **Unresolved**: what requires another query, a different device target, or user approval.

Report the recipe ID, rendered operation type, target component, relevant filter, and evidence ID. Never expose credentials, tokens, or passwords.

## Known failure patterns

- A tool argument error saying `Mismatch type string with value object` means the model violated the typed tool contract; fix the JSON shape before retrying.
- A context-window failure after a `get-config` usually indicates an overly broad response, not that the OLT is necessarily broken. Keep the raw evidence, project only the required fields, and issue an exact filtered query.
- HTTP 200 containing the React application HTML is a route/base-path failure, not a successful NBI response.

## RPC construction and troubleshooting checklist

Use this section whenever a `netconf_rpc` call returns an error, returns empty `<data/>`, or "doesn't get the expected node". Most failures fall into one of the categories below; do not retry the same RPC unchanged after a failure.

### Pre-flight checklist (run through this BEFORE every netconf_rpc call)

1. **Did you search `server/internal/pkgs/template_fs/template/olt/**` first?** A typed/template recipe is always preferred over hand-written XML. Only fall back to raw RPC when no recipe matches.
2. **Is `rpc` a JSON **string**, not an object?** The tool rejects `{"rpc": {...}}` with `Mismatch type string with value object`. Pass `"rpc": "<?xml...?>"` (escaped string).
3. **Is the document root exactly `<rpc>`?** Not `<hello>`, `<rpc-reply>`, or the operation itself. The tool returns `NETCONF document root must be rpc` otherwise.
4. **Does `<rpc>` have `message-id="..."`?** Required by RFC 6241 and the runtime. Use a stable identifier like `message-id="diagnostic-1"`.
5. **Does `<rpc>` carry `xmlns="urn:ietf:params:xml:ns:netconf:base:1.0"`?** Missing namespace causes `unknown-namespace` from the OLT.
6. **For `get`/`get-config`: is there a `<filter>` child?** The runtime blocks unfiltered `get`/`get-config` with `unfiltered NETCONF get is blocked`. The `<filter>` element MUST sit at depth 2 (i.e. `<rpc><get><filter>...</filter></get></rpc>`). A filter placed elsewhere is not detected.
7. **Is `endpoint` passed when more than one endpoint is configured?** Otherwise the tool returns `NETCONF endpoint is required; choose one of: ...` and lists the available IDs.
8. **Is `timeoutSeconds` between 1 and 300?** Default 30. Outside the range the call is rejected before it reaches the OLT.

### Endpoint selection by component

MF2 chassis typically exposes several SSH/NETCONF endpoints on different ports. Picking the wrong endpoint is the most common cause of "empty `<data/>`" — querying IHUB nodes on an LT port returns nothing, not an error.

| Want to inspect | endpoint ID (typical) | Notes |
|---|---|---|
| Chassis/system-level state, fan, power, software | `ihub` | IHUB runs the chassis manager |
| Network-side config, NBI proxy, CVC, uplinks | `nt` | NT handles northbound/network |
| LT board #1 (line termination, ONU ports on slot 1) | `lt1` | ONU-facing state on slot 1 lives here |
| LT board #2 (line termination, ONU ports on slot 2) | `lt2` | ONU-facing state on slot 2 lives here |
| Single-endpoint legacy profile | `default` or omit | Old profile with one NETCONF target |

If the user's profile uses custom endpoint IDs, list endpoints first by reading the profile or asking; do not guess. The endpoint ID is matched case-insensitively against both `ID` and `Name` fields.

A diagnostic that needs evidence from multiple components MUST issue separate RPCs per endpoint and correlate the replies; never assume `lt1` sees `ihub` state.

### User-side VSI routing (mandatory)

Treat **user-side VSI**, **ONU VSI**, **vlan-sub-interface on an ONU**, `OnuInterfaceVsiName`, GEM, T-CONT, v-enet, UNI, subscriber profile, and PON/ONT service state as **LT-owned** objects. They belong on the LT board that hosts that ONT, never on `ihub` or `nt`.

In Access Console, `GetOnuInterfaceVsiName` builds the ONU-side VSI as `PortId + "/VSI_" + RealServiceType`; the service revert template queries it through the ONU mounted interface. This is distinct from network-side VSI, CVC, uplink, and VPLS objects. In particular, never reinterpret `VSI` as `VPLS` and never choose an IHUB VPLS/VP query for a user-side VSI request.

To construct a user-side VSI RPC, require the exact LT endpoint (`lt1`, `lt2`, or a profile-specific LT ID) and enough object identity to make a bounded filter: normally ONT name/AID plus UNI/port and service type or VSI name. If the operator only says “query user-side VSI” and does not identify the ONT's LT board, return a clarification request instead of guessing an endpoint or emitting a generic IHUB RPC.

### Filter construction rules (prefer mid-granularity, accept redundancy)

- The `<filter>` element MUST be a direct child of `<get>` or `<get-config>`: `<rpc><get-config><filter type="subtree">...</filter></get-config></rpc>`. The runtime blocks unfiltered `get`/`get-config`, but the filter's *content* can be loose.
- `type="subtree"` is the safest default. Use `type="xpath"` only when you have confirmed the OLT advertises `:xpath` in its hello capabilities AND you know the exact XPath.
- Every element inside `<filter>` MUST carry its own `xmlns="..."` matching the YANG module namespace. Missing or wrong namespace is the #1 cause of `unknown-element` / `data-missing`.
- **Start with a mid-granularity filter**: a parent container plus at most one discriminator (e.g. module + container name, or interface type + index). It is fine if the reply contains redundant siblings — pick the needed fields from the reply. Chasing the exact leaf node often returns empty `<data/>` because the path or namespace is slightly off; prefer a slightly wider filter that succeeds over a precise one that fails.
- A top-level container alone inside `<filter>` is an acceptable fallback when narrower attempts return empty. The reply may be large; keep it as evidence and project only the relevant fields to the model.
- Do not nest `<get>` or `<get-config>` inside another operation. The tool rejects multi-operation RPCs with `NETCONF RPC must contain exactly one operation`.

### rpc-error → fix mapping

When the reply contains `<rpc-error>`, classify by `error-tag` before retrying:

| error-tag | Meaning | Fix |
|---|---|---|
| `unknown-element` | Element name not in any YANG module the OLT knows | Verify the element name against the YANG schema or the matching `.tpl` template; do not invent names |
| `unknown-namespace` | xmlns URI is wrong or missing | Re-check the YANG module's namespace declaration; copy it verbatim into the filter element |
| `data-missing` | Filter path is valid but no instance exists | Confirm the object exists on this endpoint/component (e.g. ONT on `lt1` not `ihub`); try a broader-but-still-bounded filter |
| `operation-not-supported` | OLT does not support this operation on this node | Use a different operation (e.g. `get` instead of `get-config` for operational state) or a different node |
| `data-not-unique` | Filter matched multiple objects but operation needs one | Add more specific selection criteria (instance name, index) |
| `lock-denied` | Another NETCONF session holds the lock | Wait or ask the user to clear the other session; do not retry unchanged |
| `access-denied` | RBAC disallows this user for this node | Report the blocker; do not retry with a different user |
| `malformed-message` | XML is not well-formed | Re-render the RPC; check for unescaped `&`, `<`, `>` in attribute values |

### Empty `<data/>` diagnostic flow

A successful reply with `<data/>` (or `<ok/>` for writes with no output) is NOT a tool failure — it means the filter did not match. Walk this flow:

1. **Is the endpoint right for this component?** IHUB nodes queried on an LT port return empty data, not an error.
2. **Is the namespace right?** Compare against the YANG module's `namespace` statement or the matching template's `xmlns`.
3. **Is the element path right?** Compare against a working template under `template_fs/template/olt/...`.
4. **Is the operation right?** Operational state (current values, alarms, statistics) lives under `<get>`; running configuration lives under `<get-config>` with `<source><running/></source>`. Querying config for a state-only node returns empty.
5. **Broaden the filter progressively.** Drop one discriminator (e.g. remove the leaf key, keep the parent container); if still empty, move up to the next parent container. A top-level container filter is an acceptable last resort — it may return many instances, but you can pick the relevant ones from the reply. Accepting redundancy is preferred over failing.

The runtime requires a `<filter>` element for `get`/`get-config`. Broadening the filter's *content* (removing discriminators, moving up to a parent container) is always allowed and is the correct response to `<data/>`. Only removing the `<filter>` element entirely is blocked.

## Real template patterns from Access Console

130+ templates under `server/internal/pkgs/template_fs/template/olt/**`. Match these conventions when writing or repairing a raw RPC. The runtime wraps the operation body below in an `<rpc xmlns="urn:ietf:params:xml:ns:netconf:base:1.0" message-id="...">...</rpc>` envelope; templates render the operation body only.

### Operations and targets

- `<get>` — operational state (alarms, hardware, port status, statistics). No `<source>`.
- `<get-config>` with `<source><running/></source>` — running configuration.
- `<edit-config>` with `<target><candidate/></target>` — Nokia SROS uses candidate config; a successful `<ok/>` only means the candidate was updated. You MUST issue a `<commit>` afterwards for the change to take effect on the OLT.

### Namespaces in actual templates

- `<get>` / `<get-config>` / `<edit-config>` / `<filter>`: `urn:ietf:params:xml:ns:netconf:base:1.0`
- Hardware state (`hardware-state/component/model-name`): `urn:ietf:params:xml:ns:yang:ietf-hardware`
- Interface state (`interfaces-state/interface`): `urn:ietf:params:xml:ns:yang:ietf-interfaces`
- Nokia SROS config (`configure/service/vpls/lag/filter/sap/ies`): `urn:nokia.com:sros:ns:yang:sr:conf`

### Filter granularity (real examples — copy these patterns)

Templates deliberately use different granularities. Use the widest one that still returns a bounded, useful reply.

**Precise (one instance by key)** — `getLtInfo.tpl`:
```xml
<get><filter type="subtree">
  <hardware-state xmlns="urn:ietf:params:xml:ns:yang:ietf-hardware">
    <component><name>Slot-Lt-{{.LtIndex}}_board</name><model-name></model-name></component>
  </hardware-state>
</filter></get>
```

**Mid (parent + empty child, returns all instances of a type)** — `getInterfacesStateInterface.tpl`:
```xml
<get><filter type="subtree">
  <interfaces-state xmlns="urn:ietf:params:xml:ns:yang:ietf-interfaces">
    <interface></interface>
  </interfaces-state>
</filter></get>
```

**Wide (single container, returns every instance below)** — `lag/getLagInfo.tpl`:
```xml
<get><filter type="subtree">
  <configure xmlns="urn:nokia.com:sros:ns:yang:sr:conf"><lag/></configure>
</filter></get>
```

**Multi-branch (one filter, several related nodes)** — `oam_acl/getihuboamacl.tpl` queries `<filter>` and two `<ies>` interfaces (OAM, mgmt_ztp) in one RPC. Use this pattern to fetch related nodes in one round-trip instead of N narrow queries.

Prefer mid or wide filters when you need to enumerate instances or when the exact key is uncertain. Precise filters only work when the key format is documented (e.g. `Slot-Lt-1_board`, `service-name`, `interface-name`).

### Endpoint by filename suffix

Templates encode the target endpoint in the filename:
- `*ihub*.tpl` or `getIhub*.tpl` → endpoint `ihub`
- `*lt*.tpl` or `getLt*.tpl` → endpoint `lt1` or `lt2` (pick by LT index)
- `*nt*.tpl` or `getNt*.tpl` → endpoint `nt`
- Pairs like `cvc_search_ihub.tpl` + `cvc_search_lt.tpl` → same query, different endpoints; pick the one matching the component, or issue both and correlate the replies.

### Template parameter syntax

Two delimiter styles coexist in the repository:
- `{{.X}}` — Go text/template standard (`getLtInfo.tpl`, `getihuboamacl.tpl`)
- `[[.X]]` — custom delimiter (`cvc_create.tpl`, `cvc_search_ihub.tpl`), chosen to avoid clashing with XML or YAML-like content

Both support conditionals: `[[if eq .MulticastCVC "true"]] ... [[else]] ... [[end]]` and `{{if .X}} ... {{end}}`.

Dynamic element names are allowed: `<{{.FilterNode}}>` renders to `<ingress>` or `<egress>` depending on the parameter value.

When calling a template via the recipe system, pass parameter values as a JSON object; the renderer fills both `{{.X}}` and `[[.X]]` forms. Do not substitute by hand — the recipe allowlist validates the schema. In a hand-written raw RPC, use literal values; the OLT rejects unrendered `{{.X}}` or `[[.X]]` with `malformed-message` or `unknown-element`.

### Hand-written RPC skeleton (use only when no template exists)

```xml
<?xml version="1.0" encoding="UTF-8"?>
<rpc xmlns="urn:ietf:params:xml:ns:netconf:base:1.0" message-id="diagnostic-{n}">
  <get-config>
    <source><running/></source>
    <filter type="subtree">
      <your-module xmlns="urn:ietf:params:xml:ns:yang:your-module">
        <target-node>
          <specific-key>specific-value</specific-key>
        </target-node>
      </your-module>
    </filter>
  </get-config>
</rpc>
```

- For operational state, replace `<get-config>...<source>...</source>...</get-config>` with `<get><filter type="subtree">...</filter></get>`.
- Always escape `&`, `<`, `>`, `"` in attribute and text values (`&amp;`, `&lt;`, `&gt;`, `&quot;`).
- Keep `message-id` unique within a single diagnostic run so replies can be correlated.

### JSON argument shape (reminder)

```json
{
  "endpoint": "lt1",
  "rpc": "<?xml version=\"1.0\" encoding=\"UTF-8\"?><rpc xmlns=\"urn:ietf:params:xml:ns:netconf:base:1.0\" message-id=\"diagnostic-1\"><get><filter type=\"subtree\">...</filter></get></rpc>",
  "timeoutSeconds": 30
}
```

`rpc` is a string. `endpoint` and `timeoutSeconds` are top-level fields, not inside the XML. Do not nest `timeoutSeconds` inside the XML.
