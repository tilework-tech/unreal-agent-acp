package schemacheck_test

import (
	"testing"

	"github.com/tilework-tech/unreal-agent-acp/internal/schemacheck"
)

func TestV1AcceptsValidAndRejectsInvalidPayloads(t *testing.T) {
	if err := schemacheck.V1("PromptResponse", []byte(`{"stopReason":"end_turn"}`)); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	for _, invalid := range []struct{ definition, payload string }{
		{"PromptResponse", `{"stopReason":"bogus"}`},
		{"PromptResponse", `{}`},
		{"SessionNotification", `{"sessionId":"s","update":{"sessionUpdate":"tool_call"}}`},
	} {
		if err := schemacheck.V1(invalid.definition, []byte(invalid.payload)); err == nil {
			t.Errorf("%s accepted invalid payload %s", invalid.definition, invalid.payload)
		}
	}
}

func TestV1RejectsUnknownDefinition(t *testing.T) {
	if err := schemacheck.V1("NoSuchDefinition", []byte(`{}`)); err == nil {
		t.Fatal("unknown definition accepted")
	}
}
