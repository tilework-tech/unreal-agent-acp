# Noridoc: unreal-agent-acp

Path: @/

### Overview

- An [Agent Client Protocol](https://agentclientprotocol.com) v1 agent over stdio that runs [`unreal-agent`](https://github.com/unreallabsai/unreal-agent) in-process as a Go library. Any ACP client (Zed, Nori CLI, the ACP TCK) can drive it by spawning the `unreal-agent-acp` binary.
- It embeds the upstream harness (coordinator, inbox, session store, bash/view-image tools, LLM clients) rather than wrapping the `unreal-agent-runner` CLI, because ACP needs many prompts per session, many sessions per process, mid-turn cancellation, and permission prompts, none of which a one-shot CLI can provide.
- Upstream has no public long-lived session API, so the runner wiring from its internal `cmd/internal/agentrunner` package is vendored and reshaped into a facade in @/internal/unrealrt. Vendoring rules, pristine copies, and known upstream gaps are tracked in @/VENDORED.md.

### How it fits into the larger codebase

- Package layering is strict: only @/acp, @/internal/acpagent and @/cmd know ACP. @/internal/unrealrt, @/internal/translate and @/internal/permission are protocol-independent so they could back a different protocol (or ACP v2) unchanged. The rule is stated in @/AGENTS.md.

```
  ACP client (stdin/stdout, NDJSON)
            |
  cmd/unreal-agent-acp      env -> acpagent.Config, logs to stderr
            |
  acp/jsonrpc  <---------->  internal/acpagent   (ACP handlers, session table, Approver)
                                 |      |      \
                                 |      |       internal/permission  (Gate wraps operation.Manager)
                                 |      internal/translate           (store items -> neutral Updates)
                                 |
                          internal/unrealrt  (VENDORED+MODIFIED Runtime / Session facade)
                                 |
              github.com/unreallabsai/unreal-agent (coordinator, inbox, localfile store, tools, LLM)
```

- @/acp/v1 and @/acp/v2 are generated from the pinned official schemas in @/schema by @/tools/acpgen (a fork of coder/acp-go-sdk's generator; no official Go SDK exists). Only v1 is wired; v2 compiles but no client speaks it yet. CI regenerates and fails on drift.
- @/schema embeds the pinned `schema.json` files; @/internal/schemacheck validates JSON against them and is used only by tests.
- @/internal/fakemodel is an `httptest` OpenAI Responses SSE server; @/internal/cmd/fake-model is a standalone binary of it for the TCK. It blocks forever on prompts containing `TCK-CANCEL` so cancellation always lands mid-turn.
- @/third_party holds license texts plus pristine upstream copies of the vendored runner files (with their own `go.mod` so they are never built), used by @/scripts/vendor-diff.sh for 3-way merges on upstream bumps.
- @/scripts/tck.sh runs the official `agentclientprotocol/acp-tck` (pinned commit, via `uv`) against the built binary with the fake model.

### Core Implementation

- **Startup.** @/cmd/unreal-agent-acp reads `UNREAL_AGENT_ACP_SESSION_DIRECTORY` and `UNREAL_AGENT_ACP_MODELS` and calls `acpagent.Serve` on stdin/stdout. `UNREAL_HARNESS_LLM_*` variables are read by @/internal/unrealrt. `Serve` creates one `unrealrt.Runtime`, injecting its own `Observer` and `WrapOperations` hooks, and runs a `jsonrpc.Conn` until stdin closes or SIGINT/SIGTERM.
- **A prompt turn is fully synchronous before the response.**

```
session/prompt (own goroutine in jsonrpc)
  -> acpagent.prompt: flatten content blocks to one string
  -> unrealrt.Session.Prompt
       rebuild coordinator from store; submit settings control, user text, StopWhenIdle
       coordinator.Run (same goroutine) -> LLM call / tool ops
         -> localfile store Append
           -> Runtime observer (synchronous)
             -> acpagent.observe -> translate.Session.Translate -> conn.Notify(session/update)
  <- TurnResult{Stop}
  -> PromptResponse{stopReason}
```

  Because every store append notifies on the prompting goroutine, all `session/update` frames for a turn are written before the `session/prompt` response.
- **Cancellation paths** all end in the same place, an upstream `StopHard` control that interrupts the model call and cancels running operations so they are recorded in the session file:

| Trigger | Path |
|---|---|
| `session/cancel` notification | `acpagent.HandleNotification` -> `Session.Cancel` -> prompt returns `cancelled` |
| `$/cancel_request` for the prompt request | jsonrpc cancels the request ctx -> `Prompt` turns ctx cancellation into `Cancel` -> error `-32800` |
| `session/close` | `Session.Close` cancels, waits for the prompt, untracks |
| stdin EOF / signal | `Serve` returns, `Runtime.Close` closes every session |

- **Permissions.** Each ACP session has a `permission.Gate` (modes `ask` and `full-access`). Per prompt, `WrapOperations` wraps the operation manager with the gate. Shell operations in ask mode are held while the agent's Approver issues `session/request_permission` to the client, naming the tool call id the client already displays (learned from `ToolCallStatus.WaitingFor` before the operation is dispatched). Allow-always is remembered for the rest of the in-memory session.
- **Load and resume.** `session/load` reopens via `ResumeSession`, replays every stored item through a fresh translator and streams the updates before responding. `session/resume` feeds history into the translator silently (so later tool updates refer to known calls) and sends nothing. Neither runs the model; `ResumeSession` deliberately never starts the loop because upstream would auto-answer any unanswered input.
- **Persistence.** Sessions live in the upstream `localfile` store (`<id>.session.jsonl`) under `$XDG_STATE_HOME/unreal-agent/sessions` (fallback `~/.local/state/...`) unless overridden. A sidecar `<id>.acp.json` holds what the store lacks: cwd, title (first prompt line), model and reasoning effort. Shell operation output goes under `operations/<id>/` in the same directory.

### Things to Know

- stdout is the ACP transport; anything else written to it corrupts the stream. All logs go to stderr via `slog`.
- Testing is black-box through public boundaries only: the ACP wire (an in-process client over pipes, plus a stdio round-trip against the real binary), the `unrealrt` API, or `operation.Manager`. Tests use a real `localfile` store, real `sh`, and the fake model over HTTP so the real upstream LLM client is exercised. The test client validates every outbound frame (notifications, permission requests, responses) against the pinned v1 schema. Conformance is checked by `make tck`.
- `make test` runs with `-race`, which needs cgo (a C compiler and `CGO_ENABLED=1`). Without cgo, run `go test ./...` locally; CI runs the race build.
- One prompt per session at a time; a concurrent prompt is rejected with `-32603`. Sessions are independent and may prompt concurrently.
- Each prompt re-reads the session file and rebuilds the coordinator because upstream `coordinator.Run` is single-use.
- Responses arrive as one `agent_message_chunk` per completed model message; upstream discards token deltas.
- Permission mode and allow-always live only in memory: they reset to `ask` after `session/load`, `session/resume`, or a process restart.
- Non-empty `mcpServers` are accepted and ignored with a stderr warning; MCP support is not implemented yet.
- `usage_update` is not sent and `PromptResponse` carries no usage: ACP v1 `usage_update` requires a context-window size the agent does not know, and v1 `PromptResponse` has no usage field. `unrealrt` still sums usage per turn.

Created and maintained by Nori.
