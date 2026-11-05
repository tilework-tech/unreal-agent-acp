# Vendored upstream code

`unreal-agent` does not yet expose a public, long-lived session API. Until it
does, the runner wiring we need is copied out of its internal
`cmd/internal/agentrunner` package and modified here. Each copy is a
merge-resolution liability on every upstream bump.

Pinned upstream: `github.com/unreallabsai/unreal-agent` `v0.3.1`, commit
`55747f5` (`third_party/unreal-agent/UPSTREAM_COMMIT`). `go.mod` must require
the version that resolves to that same commit.

| Our file | Upstream file @ pinned commit | What changed | Why |
|---|---|---|---|
| `internal/unrealrt/runtime.go` | `cmd/internal/agentrunner/run.go` (`Run`, `resolveSessionDirectory`, `resolveMaxAttempts`, `defaultSystemPrompt`, provider/key resolution, model settings and compaction threshold, operation drain) | One-shot `Run` split into `Runtime` + `Session` (`NewSession`, `ResumeSession`, `Prompt`, `Cancel`, `Replay`, `Close`). Request parsing, flags, JSONL output, `max_output_tokens` and `loadDotEnv` dropped. One store observer per runtime instead of one per run. `settings.json` is loaded per prompt for the session's current model. | ACP needs many prompts per session and many sessions per process; the model can change between prompts; `loadDotEnv` mutates process-wide env. |
| `internal/unrealrt/providers.go` | `cmd/internal/providers/providers.go` | Package name; `Default` renamed `DefaultProviders`, `Find` renamed `findProvider`. | The provider table is internal upstream. |
| `internal/unrealrt/xdgpath.go` | `cmd/internal/xdgpath/directory.go` | Package name; functions unexported (`configDirectory`, `sessionDirectory`, `xdgDirectory`). | Session and settings directories must match `unreal-agent-runner`'s. |

Pristine copies of every upstream file listed above live under
`third_party/unreal-agent/` at the pinned commit, plus
`cmd/internal/agentrunner/config.go` (the `Request`/`Tools` types `run.go` is
written against; nothing of it is vendored) for merge context. That directory
has its own `go.mod` so the copies are never built.

## Bumping upstream

1. `make vendor-diff TO=<commit-or-ref>` prints the upstream diff for every
   pristine file.
2. Apply that diff by hand to our modified copies listed above.
3. `scripts/vendor-diff.sh --update <commit>` refreshes the pristine copies
   and `UPSTREAM_COMMIT`. It deletes pristine files that no longer exist
   upstream; copy any file that replaces them (and any newly vendored file)
   into `third_party/unreal-agent/` by hand.
4. `go get github.com/unreallabsai/unreal-agent@<commit>` and `make test`.

## Known upstream gaps

These need upstream changes; they are not worked around here.

- **Token streaming.** `harness/llm/responsesapi/stream.go` discards output
  deltas, so ACP clients receive one `agent_message_chunk` per completed
  model message.
- **Session metadata.** The session store has no title or cwd. We keep a
  sidecar `<id>.acp.json` next to each session file (`internal/unrealrt/metadata.go`, ours, not vendored).
- **Long-lived coordinator.** `coordinator.Run` is single-use, so each prompt
  re-reads the session file and rebuilds the coordinator from it.
- **Compaction for unknown models.** A model missing from the settings catalog
  has compaction threshold 0, so upstream compacts whenever more than 20,000
  tokens are in context. Custom models need a `settings.json` entry.
- **No token refresh.** Codex access tokens are never refreshed; something
  outside the agent must keep `auth.json` current. We build an LLM client per
  prompt so the rewritten file is picked up.
- **Per-session environment.** `operation.ShellInput` has no environment, so
  every session shares the agent process environment.

## Known gaps in this agent

Not upstream limitations; recorded so they are not mistaken for bugs.

- **MCP.** `mcpServers` in `session/new`, `session/load` and `session/resume`
  are accepted and ignored with a stderr warning. MCP support is not
  implemented yet.
- **Permission policy is in memory.** Permission mode and "allow always" reset
  to `ask` on `session/load`, `session/resume`, or process restart.
- **No usage reporting.** ACP v1 `usage_update` requires a context-window size,
  which is known only for models in the upstream settings catalog, and v1
  `PromptResponse` has no usage field, so per-turn usage (summed in
  `unrealrt.TurnResult`) is not sent.
- **No compaction notice.** Compaction summaries are hidden from the client and
  nothing tells it that context was compacted.
- **Images and audio.** Prompt image/audio content blocks are rejected as
  invalid params; unreal-agent takes a single text input per prompt.

## Delete this file when

Upstream exposes a public runtime package equivalent to `internal/unrealrt`.
Then delete `internal/unrealrt` vendored files, `third_party/unreal-agent`,
and `scripts/vendor-diff.sh`.
