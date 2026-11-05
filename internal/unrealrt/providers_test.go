package unrealrt_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tilework-tech/unreal-agent-acp/internal/fakemodel"
	"github.com/tilework-tech/unreal-agent-acp/internal/unrealrt"
)

// codexToken is an unsigned JWT shaped like a ChatGPT access token for account-1.
func codexToken(t *testing.T, expires time.Time) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{
		"exp":                         expires.Unix(),
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "account-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"none"}`)) + "." + encode(claims) + "." + encode([]byte("signature"))
}

// writeCodexAuth writes a Codex auth.json the way an external credential
// manager does: a full replacement with a redacted refresh token.
func writeCodexAuth(t *testing.T, path, accessToken string) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": accessToken, "account_id": "account-1", "refresh_token": "REDACTED",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
}

// codexEnv points the openai-codex provider at the fake model with credentials
// in a private CODEX_HOME, and returns the auth.json path.
func codexEnv(t *testing.T, accessToken string) (func(map[string]string), string) {
	t.Helper()
	home := t.TempDir()
	authFile := filepath.Join(home, "auth.json")
	writeCodexAuth(t, authFile, accessToken)
	return func(env map[string]string) {
		env["UNREAL_HARNESS_LLM_PROVIDER"] = "openai-codex"
		env["CODEX_HOME"] = home
		delete(env, "UNREAL_HARNESS_LLM_API_KEY")
		delete(env, "UNREAL_HARNESS_LLM_MODEL")
	}, authFile
}

func writeSettings(t *testing.T, env map[string]string, settings string) {
	t.Helper()
	directory := filepath.Join(env["XDG_CONFIG_HOME"], "unreal-agent")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAnthropicProviderRequiresItsAPIKey(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(), func(env map[string]string) {
		env["UNREAL_HARNESS_LLM_PROVIDER"] = "anthropic"
		delete(env, "UNREAL_HARNESS_LLM_API_KEY")
	})
	_, err := f.runtime.NewSession(t.Context(), unrealrt.Options{Cwd: f.cwd})
	if !errors.Is(err, unrealrt.ErrAuthRequired) || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("err = %v, want ErrAuthRequired naming ANTHROPIC_API_KEY", err)
	}
}

func TestAnthropicProviderDefaultsToClaude(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(), func(env map[string]string) {
		env["UNREAL_HARNESS_LLM_PROVIDER"] = "anthropic"
		env["ANTHROPIC_API_KEY"] = "test-key"
		delete(env, "UNREAL_HARNESS_LLM_API_KEY")
		delete(env, "UNREAL_HARNESS_LLM_MODEL")
	})
	if got := f.newSession(t).Model(); got != "claude-opus-5-5" {
		t.Fatalf("default model = %q", got)
	}
}

func TestOpenAICodexDefaultsToGPT6Astra(t *testing.T) {
	override, _ := codexEnv(t, codexToken(t, time.Now().Add(time.Hour)))
	f := newFixture(t, fakemodel.Sequence(fakemodel.Text("ok")), override)
	s := f.newSession(t)
	if got := s.Model(); got != "gpt-6-astra" {
		t.Fatalf("default model = %q", got)
	}
	prompt(t, s, "hi")
	if got := f.model.Requests()[0].Model(); got != "gpt-6-astra" {
		t.Fatalf("model requested = %q", got)
	}
}

const compactionInstruction = "You are performing context compaction"

func compactionRequests(requests []fakemodel.Request) int {
	count := 0
	for _, request := range requests {
		if slices.ContainsFunc(request.UserTexts(), func(text string) bool { return strings.Contains(text, compactionInstruction) }) {
			count++
		}
	}
	return count
}

func largeContextReply(text string) fakemodel.Reply {
	reply := fakemodel.Text(text)
	reply.InputTokens = 30_000
	return reply
}

func TestCompactionThresholdComesFromSettings(t *testing.T) {
	for _, test := range []struct {
		name       string
		threshold  int
		compaction bool
	}{
		{"above the context in use", 900_000, false},
		{"below the context in use", 25_000, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, fakemodel.Sequence(largeContextReply("first"), fakemodel.Text("summary"), fakemodel.Text("second")), func(env map[string]string) {
				writeSettings(t, env, `{"providers":{"openai":{"info":{"id":"openai","name":"OpenAI"},"models":[`+
					`{"id":"fake-model","name":"Fake","context_window":1000000,"compaction_threshold":`+strconv.Itoa(test.threshold)+`}]}}}`)
			})
			s := f.newSession(t)
			prompt(t, s, "first")
			prompt(t, s, "second")
			if got := compactionRequests(f.model.Requests()) != 0; got != test.compaction {
				t.Fatalf("compaction requested = %v, want %v", got, test.compaction)
			}
		})
	}
}

func TestInvalidSettingsFailThePrompt(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(fakemodel.Text("ok")), func(env map[string]string) {
		writeSettings(t, env, `{"providers":`)
	})
	if _, err := f.newSession(t).Prompt(t.Context(), "hi"); err == nil || !strings.Contains(err.Error(), "settings") {
		t.Fatalf("err = %v, want a settings error", err)
	}
}

func TestRewrittenCodexAuthFileIsUsedOnTheNextPrompt(t *testing.T) {
	first, second := codexToken(t, time.Now().Add(time.Hour)), codexToken(t, time.Now().Add(2*time.Hour))
	override, authFile := codexEnv(t, first)
	f := newFixture(t, fakemodel.Sequence(fakemodel.Text("one"), fakemodel.Text("two")), override)
	s := f.newSession(t)

	prompt(t, s, "first")
	writeCodexAuth(t, authFile, second)
	prompt(t, s, "second")

	requests := f.model.Requests()
	if len(requests) != 2 {
		t.Fatalf("model requests = %d", len(requests))
	}
	if got := requests[0].Header.Get("Authorization"); got != "Bearer "+first {
		t.Fatal("first prompt did not use the original access token")
	}
	if got := requests[1].Header.Get("Authorization"); got != "Bearer "+second {
		t.Fatal("second prompt did not use the rewritten access token")
	}
}

func TestChangedAPIKeyIsUsedOnTheNextPrompt(t *testing.T) {
	var env map[string]string
	f := newFixture(t, fakemodel.Sequence(fakemodel.Text("one"), fakemodel.Text("two")), func(e map[string]string) { env = e })
	s := f.newSession(t)

	prompt(t, s, "first")
	env["UNREAL_HARNESS_LLM_API_KEY"] = "rotated-key"
	prompt(t, s, "second")

	requests := f.model.Requests()
	if got := requests[0].Header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("first prompt Authorization = %q", got)
	}
	if got := requests[1].Header.Get("Authorization"); got != "Bearer rotated-key" {
		t.Fatalf("second prompt Authorization = %q", got)
	}
}

func TestExpiredCodexTokenFailsOnlyUntilTheFileIsRenewed(t *testing.T) {
	override, authFile := codexEnv(t, codexToken(t, time.Now().Add(time.Hour)))
	f := newFixture(t, fakemodel.Sequence(fakemodel.Text("one"), fakemodel.Text("two")), override)
	s := f.newSession(t)
	prompt(t, s, "first")

	writeCodexAuth(t, authFile, codexToken(t, time.Now().Add(-time.Minute)))
	if _, err := s.Prompt(t.Context(), "expired"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("prompt with an expired token err = %v", err)
	}
	writeCodexAuth(t, authFile, codexToken(t, time.Now().Add(time.Hour)))
	if result := prompt(t, s, "renewed"); result.Stop != unrealrt.StopEndTurn {
		t.Fatalf("stop after renewal = %q", result.Stop)
	}
}
