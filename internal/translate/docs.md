# Noridoc: translate

Path: @/internal/translate

### Overview

- Turns unreal-agent `sessionstore.Item`s into neutral conversation `Update`s: user message, agent message, agent thought, tool call started, tool call updated.
- ACP-free; its kinds, tool kinds and statuses are chosen so @/internal/acpagent can map them to ACP `session/update` variants one-to-one.
- One stateful `Session` per conversation drives both live streaming and replay, so a client loading a session sees what it would have seen live.

### How it fits into the larger codebase

- @/internal/acpagent keeps one `translate.Session` per ACP session. During a prompt it is fed by the `unrealrt` observer; during `session/load`/`session/resume` it is fed by `unrealrt.Session.Replay`.
- Live streaming drops `UserMessage` updates (the client already shows what it sent); load replay sends them. Otherwise load output is byte-identical to the live stream.
- Tool output text is rendered by upstream's bash `ResultTranslator`, i.e. the same text the model sees for a finished call.

### Core Implementation

- External inbox inputs become `UserMessage` (control inputs are hidden). Model responses yield `AgentMessage` per non-empty message, `AgentThought` per non-empty reasoning summary, and `ToolCallStarted` per tool call. Message ids use the provider id, else `<responseID>-<index>`, so ids are stable across replay.
- Tool calls are remembered by call id so later `ToolCallStatus` items can be titled and their output rendered. Per-tool presentation:

| Tool | Kind | Title / extras |
|---|---|---|
| Bash | execute | the command (whitespace-collapsed, shortened) |
| ViewImage | read | `View <path>`, plus a location |
| SkillUse | read | `Load skill <name>` |
| anything else | other | the tool name |

- Status mapping from the call's operations: any operation still running -> `in_progress`; otherwise any `Ready` (not started, e.g. awaiting approval) -> `pending`; otherwise any failed or canceled -> `failed`; otherwise `completed`. A call-level error with no operations (e.g. invalid arguments) is `failed` with `Error: ...` as output. Output is attached only to terminal updates.
- A repeated identical non-terminal status for the same call is suppressed.

### Things to Know

- `Session` is not safe for concurrent use; @/internal/acpagent only touches it from the prompting goroutine (or during load before the session is tracked).
- A non-zero shell exit is `completed`, not `failed`; the exit code is in the output text. Only failed/canceled operations make a call `failed`.
- A status for a call the translator never saw (e.g. history fed partially) is still reported, just without tool-specific rendering.

Created and maintained by Nori.
