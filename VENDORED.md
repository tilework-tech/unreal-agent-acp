# Vendored upstream code

`unreal-agent` does not yet expose a public, long-lived session API. Until it
does, the runner wiring we need is copied out of its internal
`cmd/internal/agentrunner` package and modified here. Each copy is a
merge-resolution liability on every upstream bump.

Pinned upstream: `github.com/unreallabsai/unreal-agent` `v0.2.0`
(`third_party/unreal-agent/UPSTREAM_COMMIT`). `go.mod` must pin the same
commit.

| Our file | Upstream file @ pinned commit | What changed | Why |
|---|---|---|---|
| `internal/unrealrt/runtime.go` | `cmd/internal/agentrunner/run.go` (`Run`, `resolveSessionDirectory`, `resolveMaxAttempts`, `selectProvider`, `defaultSystemPrompt`, provider/key resolution) | One-shot `Run` split into `Runtime` + `Session` (`NewSession`, `ResumeSession`, `Prompt`, `Cancel`, `Replay`, `Close`). Request parsing, flags, JSONL output and `loadDotEnv` dropped. One store observer per runtime instead of one per run. | ACP needs many prompts per session and many sessions per process; `loadDotEnv` mutates process-wide env. |
| `internal/unrealrt/providers.go` | `cmd/internal/agentrunner/providers.go`, `run.go` (`Provider`, `Client`) | Copied verbatim except package name. | `DefaultProviders` is internal upstream. |

Pristine copies of every upstream file listed above live under
`third_party/unreal-agent/` at the pinned commit.

## Bumping upstream

1. `make vendor-diff TO=<commit-or-ref>` prints the upstream diff for every
   pristine file.
2. Apply that diff by hand to our modified copies listed above.
3. `scripts/vendor-diff.sh --update <commit>` refreshes the pristine copies
   and `UPSTREAM_COMMIT`.
4. `go get github.com/unreallabsai/unreal-agent@<commit>` and `make test`.

## Known upstream gaps

These need upstream changes; they are not worked around here.

- **Token streaming.** `harness/llm/responsesapi/stream.go` discards output
  deltas, so ACP clients receive one `agent_message_chunk` per completed
  model message.
- **Session metadata.** The session store has no title or cwd. We keep a
  sidecar `<id>.acp.json` next to each session file (`internal/unrealrt/metadata.go`, ours, not vendored).
- **Long-lived coordinator.** `coordinator.Run` is single-use, so each prompt
  rebuilds the coordinator from the session file.
- **Per-session environment.** `operation.ShellInput` has no environment, so
  every session shares the agent process environment.

## Delete this file when

Upstream exposes a public runtime package equivalent to `internal/unrealrt`.
Then delete `internal/unrealrt` vendored files, `third_party/unreal-agent`,
and `scripts/vendor-diff.sh`.
