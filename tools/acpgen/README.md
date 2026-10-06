# acpgen

Generates Go types for the Agent Client Protocol from the official
`schema.json` / `meta.json` release assets.

Forked from `github.com/coder/acp-go-sdk/cmd/generate` at v0.13.5
(Apache-2.0, see `third_party/acp-go-sdk/LICENSE`). Changes from upstream:

- Emits only types, method-name constants and helpers. Dispatch code
  (`agent_gen.go`, `client_gen.go`) was removed because it targets the SDK's
  connection runtime; our wire layer is `acp/jsonrpc`.
- `-package` flag; `-schema` and `-out` are required (no repo-root search).
- anyOf unions with shared base properties no longer get a struct
  `Validate()` that references fields the union wrapper does not have.
- Primitive union variants no longer emit unreachable code after `return`.

Known quirks kept from upstream:

- `NewSessionConfigOption*` helpers do not set the required `id`/`name`.
- Some union variant types are named after the variant title rather than the
  schema (e.g. `SessionToolCallUpdate`).

Run with `make generate`. It has its own `go.mod` so `jennifer` never becomes
a runtime dependency.
