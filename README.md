# unreal-agent-acp

An [Agent Client Protocol](https://agentclientprotocol.com) (v1) agent for
[`unreal-agent`](https://github.com/unreallabsai/unreal-agent). Point any ACP
client (Zed, Nori CLI, ...) at the `unreal-agent-acp` binary over stdio.
unreal-agent runs in-process as a library; the `unreal-agent-runner` CLI is
not needed.

```sh
make build
UNREAL_HARNESS_LLM_PROVIDER=openai OPENAI_API_KEY=... ./bin/unreal-agent-acp
```

## Environment

| Variable | Meaning |
|---|---|
| `UNREAL_HARNESS_LLM_PROVIDER` | LLM provider (default `openai`) |
| `UNREAL_HARNESS_LLM_MODEL` | Default model (else the provider default) |
| `UNREAL_HARNESS_LLM_BASE_URL` | Override the provider base URL |
| `UNREAL_HARNESS_LLM_API_KEY` | API key (else the provider's own key variable, e.g. `OPENAI_API_KEY`) |
| `UNREAL_HARNESS_LLM_MAX_ATTEMPTS` | Retry attempts per model call |
| `UNREAL_AGENT_ACP_SESSION_DIRECTORY` | Session storage (default `$XDG_STATE_HOME/unreal-agent/sessions`, or `~/.local/state/unreal-agent/sessions`) |
| `UNREAL_AGENT_ACP_MODELS` | Comma-separated models offered in the client's model picker |

Unlike `unreal-agent-runner`, a `.env` file is not loaded.

## Development

`make test` runs with `-race`, which needs cgo (a C compiler and
`CGO_ENABLED=1`); without one, use `go test ./...`. `make tck` runs the
official ACP conformance suite and needs [`uv`](https://docs.astral.sh/uv/).

See [docs.md](docs.md) for architecture and [VENDORED.md](VENDORED.md) for
the upstream code we carry.
