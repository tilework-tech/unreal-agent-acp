# Noridoc: jsonrpc

Path: @/acp/jsonrpc

### Overview

- A newline-delimited JSON-RPC 2.0 peer: the wire layer ACP uses over stdio. One `Conn` both serves incoming requests/notifications and makes outgoing calls on the same stream.
- Protocol-agnostic apart from ACP's `$/cancel_request` convention; it knows nothing about ACP methods or types.
- Exists because the generated ACP types (@/acp/v1) deliberately exclude the coder SDK's dispatch runtime (see @/tools/acpgen/README.md).

### How it fits into the larger codebase

- @/internal/acpagent implements `Handler` (and `Preparer`, to stamp `session/prompt` with its arrival) and owns the only production `Conn`; it uses `Notify` for `session/update` and `Call` for `session/request_permission`.
- Tests in @/internal/acpagent build a second `Conn` as the fake ACP client, so both directions of the agent are exercised through this package.
- The cmd entrypoint (@/cmd/unreal-agent-acp) passes `os.Stdin`/`os.Stdout`, so every frame written here is the agent's entire stdout.

### Core Implementation

- `Serve` reads lines on a goroutine and dispatches them on the read loop. It returns nil on EOF, the ctx cause when ctx is done, or a read error. On return it marks the conn closed, fails every pending outgoing `Call` with `ErrClosed`, and cancels every in-flight incoming request.
- Incoming requests each run in their own goroutine with a per-request context. If the handler also implements the optional `Preparer` interface, `PrepareRequest` runs synchronously on the read loop first (before any later message is dispatched) and the context it returns becomes the request's parent context. This is how a handler observes request arrival order relative to notifications, which the goroutine hand-off otherwise loses. A `$/cancel_request` naming that id cancels the context with a peer-cancel cause; if the handler then returns a non-JSON-RPC error, it is mapped to `-32800` (request canceled). Other plain errors become `-32603`; handlers can return `*Error` for a specific code.
- Incoming notifications are handled synchronously and in order on the read loop. Handlers must not block, or the whole connection stalls (including responses to the agent's own outgoing calls).
- Outgoing `Call` assigns an integer id, waits for the matching response, and decodes the result. If its ctx ends first it forgets the id and sends `$/cancel_request` to the peer.
- Writes are serialized under a mutex and each frame is marshaled to a single line before writing, so concurrent goroutines never interleave frames.

### Things to Know

- Ids are matched by their trimmed raw JSON text, so `1` and `"1"` are different ids.
- An unparseable line gets a `-32700` response with a null id but does not end `Serve`; a structurally invalid frame gets `-32600`.
- `PrepareRequest` runs on the read loop, so like notification handlers it must not block.
- The response to a canceled incoming request is still sent (as an error) once the handler returns; handlers are expected to honor ctx promptly.
- Responses to unknown ids (for example after a `Call` was abandoned) are dropped silently.

Created and maintained by Nori.
