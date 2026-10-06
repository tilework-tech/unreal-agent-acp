# Noridoc: acpgen

Path: @/tools/acpgen

### Overview

- Generates the ACP Go types, method-name constants and helpers in @/acp/v1 and @/acp/v2 from the pinned official `schema.json`/`meta.json` in @/schema.
- A fork of `coder/acp-go-sdk`'s `cmd/generate`; no official Go SDK exists. Divergences and kept quirks are listed in @/tools/acpgen/README.md.

### How it fits into the larger codebase

- Run via `make generate`, which writes `*_gen.go` files and gofmts them. Generated files are never hand-edited; CI regenerates and fails on any diff under @/acp.
- Has its own `go.mod` so its code-generation dependency (`dave/jennifer`) never becomes a runtime dependency of the agent; `make test` runs its tests separately.
- Emits types only. The coder SDK's dispatch code was dropped because the wire layer here is @/acp/jsonrpc.

### Core Implementation

- Pipeline: load and merge the schema and meta files into an intermediate representation, then emit types, constants and helpers with `jennifer`.
- `-schema`, `-out` and `-package` are required flags; there is no repo-root discovery.

### Things to Know

- Some quirks are inherited intentionally (for example union variant naming and config-option helpers that omit required fields); see the README before "fixing" generated output.
- Bumping schemas: change versions in @/Makefile, run `make schemas`, then `make generate`.

Created and maintained by Nori.
