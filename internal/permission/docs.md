# Noridoc: permission

Path: @/internal/permission

### Overview

- Gates shell (Bash) operations behind user approval by wrapping an upstream `operation.Manager`. Other operation types, including view-image (reads any path) and skill-use (loads skills from `<cwd>/.harness/skills`), are not gated.
- A `Gate` holds one session's policy: mode (`ask` or `full-access`) and whether the user chose "allow always". `Gate.Wrap` produces a per-prompt manager that enforces it.
- ACP-free: the user is reached through an `Approver` interface that @/internal/acpagent implements with `session/request_permission`.

### How it fits into the larger codebase

- @/internal/unrealrt calls `Config.WrapOperations` once per prompt; @/internal/acpagent answers with the session's gate `Wrap`. If the session is unknown it wraps with a reject-everything approver.
- The ACP session mode and the `mode` config option both call `Gate.SetMode`.
- Decisions surface to the model and client as ordinary terminal shell operation states, which the upstream coordinator records and @/internal/translate renders like any other failed or canceled command.

### Core Implementation

- `Add`: non-shell operations, or any operation when the gate already allows (full access or allow-always), go straight to the inner manager. A shell operation in ask mode whose shell state cannot be decoded is refused (`Add` returns an error) rather than passed through unapproved. Otherwise it is held and an Approver is called in a goroutine, so `Add` never blocks the coordinator loop and several commands can await approval at once.
- Decision handling: allow-once/allow-always forward the held operation to the inner manager (allow-always also flips the gate for the rest of the session, covering all later shell commands); reject finishes it as failed with "The user rejected this command."; a withdrawn request finishes it as canceled; an Approver error finishes it as failed.
- `Cancel` of a held operation cancels the Approver's context and emits a canceled terminal state; otherwise it delegates to the inner manager.
- A forwarding goroutine merges inner updates with synthetic terminal states into the wrapper's `Updates` channel. When the inner manager shuts down (prompt ended), the wrapper closes its channel and withdraws every outstanding approval.

### Things to Know

- A mutex serializes decisions with `Cancel`, so an approved operation is handed to the inner manager before a later `Cancel` looks for it there; a decision arriving after cancel is discarded and never runs.
- Synthetic terminal states mimic what the shell actor would produce (status plus `TerminalError` in the shell state), so the coordinator needs no special casing.
- Policy is in memory only. A new gate is created whenever @/internal/acpagent tracks a session, so mode and allow-always reset to `ask` after load, resume, or restart.
- `Wrap` assumes the inner manager is owned by a single prompt.

Created and maintained by Nori.
