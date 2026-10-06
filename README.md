# unreal-agent-acp

An [Agent Client Protocol](https://agentclientprotocol.com) (v1) agent for
[`unreal-agent`](https://github.com/unreallabsai/unreal-agent). Point any ACP
client (Zed, Nori CLI, ...) at the `unreal-agent-acp` binary over stdio.

```sh
make build
UNREAL_HARNESS_LLM_PROVIDER=openai OPENAI_API_KEY=... ./bin/unreal-agent-acp
```

Environment is the same as `unreal-agent-runner`: `UNREAL_HARNESS_LLM_PROVIDER`,
`UNREAL_HARNESS_LLM_MODEL`, `UNREAL_HARNESS_LLM_BASE_URL`,
`UNREAL_HARNESS_LLM_API_KEY` (or the provider's key variable). Sessions are
stored under `$XDG_STATE_HOME/unreal-agent/sessions` unless
`UNREAL_AGENT_ACP_SESSION_DIRECTORY` is set.

See [docs.md](docs.md) for architecture and [VENDORED.md](VENDORED.md) for
the upstream code we carry.
