# Noridoc: unrealrt

Path: @/internal/unrealrt

### Overview

- The protocol-independent facade over the unreal-agent harness: a long-lived `Runtime` (one per process) holding many `Session`s that can each be prompted repeatedly, canceled, replayed, and closed.
- VENDORED and MODIFIED from upstream `cmd/internal/agentrunner` (the one-shot runner behind `unreal-agent-runner`), because upstream exposes no public long-lived session API. Vendored files carry a `// VENDORED from ...` header; the provider table is copied verbatim. The sidecar metadata code is ours.
- Knows nothing about ACP. Its exported errors and `StopReason` values are mapped to ACP by @/internal/acpagent.

### How it fits into the larger codebase

- @/internal/acpagent is the only production caller. It sets the two hooks: `Config.Observer` (every appended store item, used to drive @/internal/translate) and `Config.WrapOperations` (wraps each prompt's `operation.Manager` with a @/internal/permission gate).
- Calls into upstream `github.com/unreallabsai/unreal-agent`: the `localfile` session store, `inbox`, `coordinator`, `contextbuilder`, the bash/view-image tools, skill discovery, and the LLM clients from the provider table.
- Configuration comes from env: `UNREAL_HARNESS_LLM_PROVIDER` (default `openai`), `UNREAL_HARNESS_LLM_MODEL`, `UNREAL_HARNESS_LLM_BASE_URL`, `UNREAL_HARNESS_LLM_API_KEY` (falling back to the provider's own key variable), and `UNREAL_HARNESS_LLM_MAX_ATTEMPTS`. Upstream's `loadDotEnv` is intentionally not ported because it mutates process-wide env.
- @/VENDORED.md lists what changed from upstream and how to merge upstream bumps using the pristine copies in @/third_party/unreal-agent.

### Core Implementation

- `New` resolves the session directory (`Config.SessionDirectory`, else `$XDG_STATE_HOME/unreal-agent/sessions`, else `~/.local/state/...`), opens the store, selects the provider, and registers exactly one store observer. The LLM client is created lazily on first need and shared by all sessions.
- `NewSession` validates cwd (absolute, existing directory), requires LLM credentials, creates the store session, and writes the sidecar. `ResumeSession` inspects an existing session, merges options into the sidecar (empty cwd keeps the stored one), and never runs the loop: running it would make upstream answer any unanswered input with a model call. If the id is already tracked, `ResumeSession` closes the previous `Session` synchronously (waiting for its running prompt) before tracking the new one, and `track` loops until no previous session remains: two coordinators must never append to the same `localfile` session file, which has no per-session lock.
- `Prompt` is `PromptFrom` with the current `Arrived()` value. `PromptFrom` holds a per-session prompt lock (a second concurrent prompt gets `ErrPromptInProgress`), sets the title from the first prompt line if unset (via `updateMetadata`, under the session lock), then rebuilds everything per turn: resumes from the store, builds the tool registry rooted at the session cwd (bash, view-image, plus skill-use when `.harness/skills` exists under cwd), wraps the operation manager, opens an inbox, submits an `UpdateSettings` control (reasoning effort), the user text, and a `StopWhenIdle` control, and runs `coordinator.Run` until idle. Rebuilding per prompt is required because `coordinator.Run` is single-use.
- Store items reach `Session.observe` (usage, last stop reason, failure) and then `Config.Observer`, synchronously on the prompting goroutine, so callers see every item before `Prompt` returns.
- `TurnResult.Stop` is `cancelled` if a cancel happened, otherwise derived from the last model response (`max_tokens`, `refused`, else `end_turn`). A model failure becomes an error for that prompt only. Usage is summed across every model response in the turn.
- `Replay` pages through stored items in order; it is the same item stream the observer saw live.
- The sidecar `<id>.acp.json` stores cwd, title, model, reasoning effort, and creation time, written atomically via temp file and rename. `SetModel` and `SetReasoningEffort` persist there and apply from the next prompt. `ListSessions` skips store sessions with no sidecar (for example ones created by `unreal-agent-runner`), since they have no known cwd.

### Things to Know

- The `localfile` store's observer list is unsynchronized, so the runtime registers its single observer in `New`, before any append, and fans out to sessions itself. Never add observers per session.
- The run context is independent of the caller's ctx. Caller cancellation is converted into `Cancel` (via `context.AfterFunc`), which submits a `StopHard` control; this way the model call is interrupted and running operations are canceled and recorded in the session file, rather than the loop being torn down mid-write. `Prompt` then returns the caller's ctx error.
- A `Cancel` that arrives before the inbox exists is remembered and the hard stop is submitted as soon as the inbox is ready. `Cancel` with no running prompt does nothing to any turn, but it always increments the session's cancel epoch.
- Cancel epoch: `Arrived()` returns an `Arrival` snapshot of the cancel count; `PromptFrom(ctx, arrival, text)` starts its turn already cancelled if any `Cancel` happened since that snapshot. Callers take the snapshot when the request is received (before scheduling), so a cancel sent after the prompt cannot be lost to goroutine ordering. A snapshot that is never used needs no cleanup.
- `Close` on a session cancels and then waits for the running prompt to release its lock. Since observer callbacks run on the prompting goroutine, a blocked observer (for example a stalled stdout write in @/internal/acpagent) also blocks `Close` and `ResumeSession` of that id.
- `Runtime.Close` closes every session the same way, removes the observer, and closes the LLM client.
- All sessions share the process environment and `$SHELL` (default `/bin/sh`); upstream's `ShellInput` has no per-session environment.

Created and maintained by Nori.
