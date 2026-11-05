# unreal-agent-acp

An [Agent Client Protocol](https://agentclientprotocol.com) (ACP v1) agent
for [`unreal-agent`](https://github.com/unreallabsai/unreal-agent).

Any ACP client (Zed, Nori CLI, the ACP conformance suite, ...) can spawn the
`unreal-agent-acp` binary and talk to it over stdin/stdout. unreal-agent runs
in-process as a Go library, so the `unreal-agent-runner` CLI is not needed.

Supported: multiple sessions per process, streaming updates, mid-turn
cancellation, shell-command permission prompts (`ask` and `full-access`
modes), model and reasoning-effort selection, and `session/load`,
`session/resume`, `session/list` and `session/close` backed by unreal-agent's
on-disk session store. MCP servers, image and audio prompts are not supported
yet; see [VENDORED.md](VENDORED.md) for the full list of known gaps.

## Install

Prebuilt binaries for Linux (x86_64, aarch64) and macOS (Apple silicon) are
attached to each [GitHub release](https://github.com/tilework-tech/unreal-agent-acp/releases).
Each archive holds the `unreal-agent-acp` binary plus `LICENSE` and `NOTICE`:

```sh
curl -fsSL https://github.com/tilework-tech/unreal-agent-acp/releases/latest/download/unreal-agent-acp-linux-x86_64.tar.gz | tar xz
./unreal-agent-acp --version
```

Replace `linux-x86_64` with `linux-aarch64` or `darwin-aarch64` as needed.
`SHA256SUMS` on the release page lists the archive checksums.

Or build from source with Go 1.27+:

```sh
go install github.com/tilework-tech/unreal-agent-acp/cmd/unreal-agent-acp@latest
```

## Quick start

Point your ACP client at the binary and pass credentials in its
environment. For example, with an OpenAI API key:

```sh
UNREAL_HARNESS_LLM_PROVIDER=openai OPENAI_API_KEY=sk-... unreal-agent-acp
```

In Zed, add it as a custom agent server whose command is the path to
`unreal-agent-acp` and whose environment holds the variables below.

stdout carries only ACP frames; logs go to stderr.

## Providers

Select a provider with `UNREAL_HARNESS_LLM_PROVIDER` (default `openai`).

| Provider | API | Credentials | Default model |
|---|---|---|---|
| `openai` | OpenAI Responses | `OPENAI_API_KEY` | `gpt-6-astra` |
| `openai-codex` | ChatGPT subscription (Codex backend) | Codex `auth.json` from a ChatGPT login; see below | `gpt-6-astra` |
| `anthropic` | Anthropic Messages | `ANTHROPIC_API_KEY` | `claude-opus-5-5` |
| `openrouter` | OpenRouter Responses | `OPENROUTER_API_KEY` | none; set `UNREAL_HARNESS_LLM_MODEL` |
| `fireworks` | Fireworks Responses | `FIREWORKS_API_KEY` | none; set `UNREAL_HARNESS_LLM_MODEL` |
| `ollama` | Ollama at `http://localhost:11434/v1` | none | none; set `UNREAL_HARNESS_LLM_MODEL` |

`UNREAL_HARNESS_LLM_API_KEY` overrides the provider's own key variable.

### ChatGPT login (`openai-codex`)

The `openai-codex` provider uses the credentials written by a
[Codex CLI](https://github.com/openai/codex) ChatGPT login. It reads, in order:

1. `OPENAI_CODEX_ACCESS_TOKEN` (and optionally `OPENAI_CODEX_ACCOUNT_ID`), or
2. the file named by `OPENAI_CODEX_AUTH_FILE`, or
3. `$CODEX_HOME/auth.json` (default `~/.codex/auth.json`).

The auth file must be private (`chmod 600`). unreal-agent-acp never refreshes
tokens itself, but it re-reads the credentials at the start of every prompt,
so a process that keeps `auth.json` up to date (for example `codex login` or
a credential broker) renews them without restarting the agent. A prompt
started with an expired token fails with an error.

### Anthropic options

| Variable | Meaning |
|---|---|
| `ANTHROPIC_CACHE_TTL` | Prompt cache TTL, `5m` or `1h` (default: the API default) |
| `ANTHROPIC_FALLBACK` | `false` disables server-side model fallback (default enabled) |

### Custom endpoints

Any OpenAI Responses-compatible server works with the `openai` provider:

```sh
UNREAL_HARNESS_LLM_PROVIDER=openai \
UNREAL_HARNESS_LLM_BASE_URL=https://llm.example.com/v1 \
UNREAL_HARNESS_LLM_API_KEY=... \
UNREAL_HARNESS_LLM_MODEL=my-model \
unreal-agent-acp
```

`UNREAL_HARNESS_LLM_BASE_URL` overrides the base URL of any provider (for
`openai-codex`, only loopback addresses are accepted).

## Environment

| Variable | Meaning |
|---|---|
| `UNREAL_HARNESS_LLM_PROVIDER` | LLM provider (default `openai`) |
| `UNREAL_HARNESS_LLM_MODEL` | Default model for new sessions (else the provider default) |
| `UNREAL_HARNESS_LLM_BASE_URL` | Override the provider base URL |
| `UNREAL_HARNESS_LLM_API_KEY` | API key (else the provider's own key variable) |
| `UNREAL_HARNESS_LLM_MAX_ATTEMPTS` | Retry attempts per model call |
| `UNREAL_AGENT_ACP_SESSION_DIRECTORY` | Session storage (default `$XDG_STATE_HOME/unreal-agent/sessions`, or `~/.local/state/unreal-agent/sessions`) |
| `UNREAL_AGENT_ACP_MODELS` | Comma-separated models offered in the client's model picker |

Provider, model, base URL and retry settings are read at startup. Credentials
(API key variables, the Codex auth file) and `settings.json` are read again at
the start of every prompt. Unlike `unreal-agent-runner`, a `.env` file is not
loaded.

### Model settings

Context compaction thresholds come from unreal-agent's model catalog,
overridable in `$XDG_CONFIG_HOME/unreal-agent/settings.json` (default
`~/.config/unreal-agent/settings.json`), the same file `unreal-agent-runner`
and the unreal-agent TUI use:

```json
{
  "providers": {
    "openai": {
      "info": { "id": "openai", "name": "OpenAI" },
      "models": [
        { "id": "my-model", "name": "My model", "context_window": 200000, "compaction_threshold": 150000 }
      ]
    }
  }
}
```

A model missing from the catalog has no threshold, so its context is
compacted as soon as it exceeds unreal-agent's fixed 20,000-token retained
budget. Add custom models to `settings.json` to avoid this.

## Development

```sh
go build -o bin/unreal-agent-acp ./cmd/unreal-agent-acp
go test ./...                       # add -race when cgo is available
cd tools/acpgen && go test ./...
```

The `Makefile` wraps these and adds `make generate` (regenerate the ACP Go
types from `schema/`), `make tck` (run the official ACP conformance suite;
needs [`uv`](https://docs.astral.sh/uv/)) and `make vendor-diff`.

See [docs.md](docs.md) for the architecture, [AGENTS.md](AGENTS.md) for
contributor rules, and [VENDORED.md](VENDORED.md) for the upstream code we
carry.

## License

unreal-agent-acp is licensed under the [Apache License 2.0](LICENSE).

It includes code derived from unreal-agent (MIT), the coder/acp-go-sdk
generator (Apache-2.0) and the Agent Client Protocol schema (Apache-2.0).
See [NOTICE](NOTICE), the license texts under [third_party/](third_party),
and [VENDORED.md](VENDORED.md).
