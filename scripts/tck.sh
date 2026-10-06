#!/usr/bin/env bash
# Runs the ACP conformance suite (agentclientprotocol/acp-tck) against
# bin/unreal-agent-acp, with a fake model server standing in for the provider.
#
#   scripts/tck.sh [extra acp-tck args]
#
# Needs uv. ACP_TCK_REF pins the acp-tck commit (default below).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
tck_ref=${ACP_TCK_REF:-fa00ccd}
tck="$root/.cache/acp-tck"
work=$(mktemp -d)
trap 'kill "${model_pid:-}" 2>/dev/null || true; rm -rf "$work"' EXIT

if [[ ! -d "$tck/.git" ]]; then
  git clone --quiet https://github.com/agentclientprotocol/acp-tck.git "$tck"
fi
git -C "$tck" fetch --quiet origin
git -C "$tck" checkout --quiet "$tck_ref"

go build -o "$work/fake-model" "$root/internal/cmd/fake-model"
test -x "$root/bin/unreal-agent-acp" || go build -o "$root/bin/unreal-agent-acp" "$root/cmd/unreal-agent-acp"

"$work/fake-model" > "$work/model-url" &
model_pid=$!
for _ in $(seq 50); do
  [[ -s "$work/model-url" ]] && break
  sleep 0.1
done
model_url=$(head -n1 "$work/model-url")
mkdir -p "$work/agent-cwd" "$work/sessions"

uv --project "$tck" run acp-tck \
  --agent-cwd "$work/agent-cwd" \
  --agent-env UNREAL_HARNESS_LLM_PROVIDER=openai \
  --agent-env UNREAL_HARNESS_LLM_BASE_URL="$model_url" \
  --agent-env UNREAL_HARNESS_LLM_API_KEY=tck \
  --agent-env UNREAL_HARNESS_LLM_MODEL=fake-model \
  --agent-env UNREAL_HARNESS_LLM_MAX_ATTEMPTS=1 \
  --agent-env UNREAL_AGENT_ACP_SESSION_DIRECTORY="$work/sessions" \
  --cancel-prompt "TCK-CANCEL keep working" \
  "$@" \
  -- "$root/bin/unreal-agent-acp"
