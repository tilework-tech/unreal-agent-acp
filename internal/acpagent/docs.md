# Noridoc: acpagent

Path: @/internal/acpagent

### Overview

- The ACP v1 agent: implements `jsonrpc.Handler`, owns the ACP session table, and translates between ACP messages and the protocol-independent packages.
- The only internal package that imports ACP types (@/acp/v1) together with the runtime, translator and permission gate.
- `Serve` is the whole agent lifecycle: build the runtime with this package's hooks, serve the connection, close the runtime when the connection ends.

### How it fits into the larger codebase

- Called by @/cmd/unreal-agent-acp with stdin/stdout and a `Config` (name, version, runtime config, offered models, logger).
- Sets `unrealrt.Config.Observer` and `WrapOperations` itself; callers must leave them unset.
- Uses @/acp/jsonrpc for transport, @/internal/unrealrt for sessions, @/internal/translate to turn store items into `session/update`, and @/internal/permission for shell approvals.

### Core Implementation

- `initialize` negotiates down to the client's version if lower, and advertises `loadSession`, `embeddedContext` prompts, and the `list`, `resume` and `close` session capabilities. `authMethods` is empty; missing LLM credentials surface as `-32000` from session creation.
- `session/new`, `session/load` and `session/resume` return the session's config options (permissions mode, model, reasoning effort) and mode state. The model list is `UNREAL_AGENT_ACP_MODELS` with the session's current model prepended if absent.
- `session/load` replays every stored item through a fresh translator and sends the updates before responding; `session/resume` feeds the same translator silently. Both reuse that translator for the live session.
- `session/list` filters by cwd when given, formats `updatedAt` as RFC3339 UTC, and sorts newest first.
- `session/prompt` flattens content blocks into the single string unreal-agent accepts: text verbatim, resource links as Markdown links, embedded text resources as `<resource uri=...>` blocks, blob resources as an omitted-content placeholder. Image and audio blocks are rejected as invalid params.
- Live updates: the runtime observer runs on the prompting goroutine, records `ToolCallStatus.WaitingFor` operation ids against their tool call id, translates the item, and writes each update (except user-message echoes) with `Notify` before the prompt returns.
- Permission Approver: issues `session/request_permission` naming the tool call id learned above (the status item is appended before operations are dispatched), with allow-once, allow-always and reject options. A cancelled outcome withdraws the request; an unknown option id is an error.
- `session/cancel` calls `Session.Cancel`; `session/close` forgets the session and closes it (canceling and waiting for any running prompt). `session/set_config_option` handles `mode`, `model` and `reasoning_effort`; `session/set_mode` sets the gate mode.

### Things to Know

- Error mapping:

| Condition | JSON-RPC code |
|---|---|
| LLM credentials missing | `-32000` (auth required) |
| invalid cwd, unknown/closed session, bad params or option | `-32602` |
| concurrent prompt in the same session | `-32603` |
| prompt request canceled via `$/cancel_request` | `-32800` (from @/acp/jsonrpc) |

- `session/cancel` is a notification handled on the read loop, so it must stay non-blocking; `Session.Cancel` only submits a control.
- Reopening an already-open session id replaces its entry with a new translator and a new gate, so permission mode resets to `ask`.
- Non-empty `mcpServers` on new/load/resume are accepted and ignored with a stderr warning; MCP is not implemented yet.
- No `usage_update` is sent and `PromptResponse` has no usage: v1 `usage_update` needs a context-window size the agent does not know.
- Tests drive the agent through a real `jsonrpc.Conn` client over pipes with the fake model from @/internal/fakemodel, and validate every outbound notification, permission request and response against the pinned schema via @/internal/schemacheck.

Created and maintained by Nori.
