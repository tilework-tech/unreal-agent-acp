# unreal-agent-acp

Go [Agent Client Protocol](https://agentclientprotocol.com) agent that embeds
[`unreal-agent`](https://github.com/unreallabsai/unreal-agent) as a library.
Architecture: [docs.md](docs.md). Vendoring rules: [VENDORED.md](VENDORED.md).

## Commands

| Command | What it does |
|---|---|
| `make build` | Build `bin/unreal-agent-acp` |
| `make test` | All tests with `-race` (needs cgo; use `go test ./...` without it), plus generator tests |
| `make vet` | `go vet` and gofmt check |
| `make generate` | Regenerate `acp/v1` and `acp/v2` from `schema/` |
| `make schemas` | Re-download pinned ACP schemas (bump versions in Makefile first) |
| `make tck` | Run the ACP conformance suite (`acp-tck`, pinned commit, needs `uv`) against the binary and the fake model |
| `make vendor-diff TO=<ref>` | Show upstream drift for vendored files |
| `make snapshot` | Build release archives into `dist/` with GoReleaser v2, without publishing |

Toolchain: Go 1.27 (`encoding/json/v2`, stdlib `uuid`).

## Rules

- Only `acp/`, `internal/acpagent` and `cmd/` may import ACP types. `internal/unrealrt`,
  `internal/translate`, and `internal/permission` must stay ACP-free.
- Never hand-edit `acp/*/*_gen.go`; change `tools/acpgen` and run `make generate`. CI fails if
  regenerated output differs.
- Files derived from upstream start with a `// VENDORED from ...` header and are
  listed in `VENDORED.md`. Keep edits to them minimal and note what changed.
- Our code is Apache-2.0. Any new third-party code or data needs its license
  text under `third_party/`, an entry in `NOTICE`, and (if modified) a header
  saying where it came from.
- Releases are cut by pushing a `vX.Y.Z` tag on `main`; `.goreleaser.yaml`
  builds them. Do not rename the archives or move the binary off the archive
  root: the Nori broker downloads `unreal-agent-acp-<os>-<arch>.tar.gz` and
  runs `./unreal-agent-acp`.
- stdout is the ACP transport. Log only to stderr.
- Tests go through public boundaries: the ACP wire, the `unrealrt` API, or
  `operation.Manager`. Use a real `localfile` store, real `sh`, and the fake
  Responses server in `internal/fakemodel`. No test hooks in production code.
  ACP-level tests validate every outbound frame with `internal/schemacheck`.
