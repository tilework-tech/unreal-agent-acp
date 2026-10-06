package acp_test

import (
	"encoding/json"
	"testing"

	acp "github.com/tilework-tech/unreal-agent-acp/acp/v1"
	"github.com/tilework-tech/unreal-agent-acp/internal/schemacheck"
)

func ptr[T any](value T) *T { return &value }

func text(value string) acp.ContentBlock {
	return acp.ContentBlock{Text: &acp.ContentBlockText{Text: value}}
}

func TestOutboundMessagesValidateAgainstSchema(t *testing.T) {
	status := acp.ToolCallStatusCompleted
	kind := acp.ToolKindExecute
	cases := map[string]struct {
		definition string
		value      any
	}{
		"initialize response": {"InitializeResponse", acp.InitializeResponse{
			ProtocolVersion: acp.ProtocolVersionNumber,
			AgentInfo:       &acp.Implementation{Name: "unreal-agent-acp", Version: "dev"},
			AuthMethods:     []acp.AuthMethod{},
			AgentCapabilities: acp.AgentCapabilities{
				LoadSession:         true,
				McpCapabilities:     acp.McpCapabilities{Http: true},
				PromptCapabilities:  acp.PromptCapabilities{EmbeddedContext: true},
				SessionCapabilities: acp.SessionCapabilities{List: &acp.SessionListCapabilities{}, Resume: &acp.SessionResumeCapabilities{}, Close: &acp.SessionCloseCapabilities{}},
			},
		}},
		"agent message chunk": {"SessionNotification", acp.SessionNotification{
			SessionId: "s",
			Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: text("hello")}},
		}},
		"user message chunk": {"SessionNotification", acp.SessionNotification{
			SessionId: "s",
			Update:    acp.SessionUpdate{UserMessageChunk: &acp.SessionUpdateUserMessageChunk{Content: text("hi")}},
		}},
		"thought chunk": {"SessionNotification", acp.SessionNotification{
			SessionId: "s",
			Update:    acp.SessionUpdate{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: text("thinking")}},
		}},
		"tool call": {"SessionNotification", acp.SessionNotification{
			SessionId: "s",
			Update: acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "call-1", Title: "ls", Kind: acp.ToolKindExecute, Status: acp.ToolCallStatusPending,
				RawInput: map[string]any{"command": "ls"},
			}},
		}},
		"tool call update": {"SessionNotification", acp.SessionNotification{
			SessionId: "s",
			Update: acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
				ToolCallId: "call-1", Status: &status,
				Content: []acp.ToolCallContent{{Content: &acp.ToolCallContentContent{Content: text("out")}}},
			}},
		}},
		"current mode update": {"SessionNotification", acp.SessionNotification{
			SessionId: "s",
			Update:    acp.SessionUpdate{CurrentModeUpdate: &acp.SessionCurrentModeUpdate{CurrentModeId: "ask"}},
		}},
		"config option update": {"SessionNotification", acp.SessionNotification{
			SessionId: "s",
			Update: acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: []acp.SessionConfigOption{
				selectOption("mode", "ask"),
			}}},
		}},
		"new session response": {"NewSessionResponse", acp.NewSessionResponse{
			SessionId:     "s",
			ConfigOptions: []acp.SessionConfigOption{selectOption("model", "gpt"), selectOption("mode", "ask")},
			Modes: &acp.SessionModeState{CurrentModeId: "ask", AvailableModes: []acp.SessionMode{
				{Id: "ask", Name: "Ask"}, {Id: "full-access", Name: "Full access"},
			}},
		}},
		"prompt response": {"PromptResponse", acp.PromptResponse{StopReason: acp.StopReasonCancelled}},
		"permission request": {"RequestPermissionRequest", acp.RequestPermissionRequest{
			SessionId: "s",
			ToolCall:  acp.ToolCallUpdate{ToolCallId: "call-1", Title: ptr("rm -rf build"), Kind: &kind},
			Options: []acp.PermissionOption{
				{OptionId: "allow_once", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
				{OptionId: "allow_always", Name: "Always allow", Kind: acp.PermissionOptionKindAllowAlways},
				{OptionId: "reject_once", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
			},
		}},
		"list sessions response": {"ListSessionsResponse", acp.ListSessionsResponse{Sessions: []acp.SessionInfo{
			{SessionId: "s", Cwd: "/work", Title: ptr("fix the build"), UpdatedAt: ptr("2026-10-06T03:00:00Z")},
		}}},
		"empty list sessions response": {"ListSessionsResponse", acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}},
		"load session response":        {"LoadSessionResponse", acp.LoadSessionResponse{ConfigOptions: []acp.SessionConfigOption{selectOption("mode", "ask")}}},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			if err := schemacheck.V1Value(test.definition, test.value); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func selectOption(id, current string) acp.SessionConfigOption {
	return acp.SessionConfigOption{Select: &acp.SessionConfigOptionSelect{
		Id:           acp.SessionConfigId(id),
		Name:         id,
		CurrentValue: acp.SessionConfigValueId(current),
		Options: acp.SessionConfigSelectOptions{Ungrouped: &acp.SessionConfigSelectOptionsUngrouped{
			{Value: acp.SessionConfigValueId(current), Name: current},
			{Value: "other", Name: "Other"},
		}},
	}}
}

func TestInboundClientMessagesDecode(t *testing.T) {
	var prompt acp.PromptRequest
	if err := json.Unmarshal([]byte(`{"sessionId":"s","prompt":[
		{"type":"text","text":"look at"},
		{"type":"resource_link","name":"main.go","uri":"file:///w/main.go"},
		{"type":"resource","resource":{"uri":"file:///w/a.txt","text":"contents"}}
	]}`), &prompt); err != nil {
		t.Fatal(err)
	}
	if prompt.SessionId != "s" || len(prompt.Prompt) != 3 ||
		prompt.Prompt[0].Text == nil || prompt.Prompt[0].Text.Text != "look at" ||
		prompt.Prompt[1].ResourceLink == nil || prompt.Prompt[1].ResourceLink.Uri != "file:///w/main.go" ||
		prompt.Prompt[2].Resource == nil || prompt.Prompt[2].Resource.Resource.TextResourceContents == nil ||
		prompt.Prompt[2].Resource.Resource.TextResourceContents.Text != "contents" {
		t.Fatalf("prompt = %+v", prompt)
	}

	var session acp.NewSessionRequest
	if err := json.Unmarshal([]byte(`{"cwd":"/w","mcpServers":[
		{"name":"fs","command":"/bin/mcp","args":["-v"],"env":[{"name":"A","value":"1"}]},
		{"type":"http","name":"web","url":"http://localhost:9","headers":[]}
	]}`), &session); err != nil {
		t.Fatal(err)
	}
	if len(session.McpServers) != 2 || session.McpServers[0].Stdio == nil || session.McpServers[0].Stdio.Command != "/bin/mcp" ||
		session.McpServers[1].Http == nil || session.McpServers[1].Http.Url != "http://localhost:9" {
		t.Fatalf("session = %+v", session)
	}

	var permission acp.RequestPermissionResponse
	if err := json.Unmarshal([]byte(`{"outcome":{"outcome":"selected","optionId":"allow_once"}}`), &permission); err != nil {
		t.Fatal(err)
	}
	if permission.Outcome.Selected == nil || permission.Outcome.Selected.OptionId != "allow_once" {
		t.Fatalf("permission = %+v", permission)
	}
	if err := json.Unmarshal([]byte(`{"outcome":{"outcome":"cancelled"}}`), &permission); err != nil {
		t.Fatal(err)
	}
	if permission.Outcome.Cancelled == nil {
		t.Fatalf("cancelled permission = %+v", permission)
	}

	var setOption acp.SetSessionConfigOptionRequest
	if err := json.Unmarshal([]byte(`{"sessionId":"s","configId":"model","value":"gpt-6"}`), &setOption); err != nil {
		t.Fatal(err)
	}
	if setOption.ValueId == nil || setOption.ValueId.ConfigId != "model" || setOption.ValueId.Value != "gpt-6" {
		t.Fatalf("set option = %+v", setOption)
	}
}
