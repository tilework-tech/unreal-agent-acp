package translate_test

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"

	"github.com/tilework-tech/unreal-agent-acp/internal/translate"
)

func externalInput(text string) sessionstore.Item {
	payload, _ := json.Marshal(text)
	return sessionstore.Item{Kind: sessionstore.ItemInput, Data: inbox.Input{ID: "in-" + inbox.ID(text), Kind: inbox.InputExternal, Payload: payload}}
}

func controlInput(message inbox.ControlMessage) sessionstore.Item {
	payload, _ := json.Marshal(message)
	return sessionstore.Item{Kind: sessionstore.ItemInput, Data: inbox.Input{ID: "control", Kind: inbox.InputControl, Payload: payload}}
}

func modelResponse(id string, outputs ...llm.Item) sessionstore.Item {
	return sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{
		TurnID: "turn-1", Response: llm.Response{ID: id, Stop: llm.StopComplete, Output: outputs},
	}}
}

func message(providerID, text string) llm.Item {
	return llm.Item{ProviderID: providerID, Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}}
}

func call(callID, name, arguments string) llm.Item {
	return llm.Item{ProviderID: "fc-" + callID, Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: callID, Name: name, Arguments: arguments}}
}

func shellOperation(t *testing.T, id operation.ID, status operation.Status, state operation.ShellState) operation.Operation {
	t.Helper()
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{ID: id, Type: operation.TypeShell, Version: operation.VersionShell, Status: status, MaxOutputLength: 1000, State: encoded}
}

func toolStatus(callID string, status tool.CallStatus, operations ...operation.Operation) sessionstore.Item {
	return sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{
		TurnID: "turn-1", CallID: callID, Status: status, Operations: operations,
	}}
}

func translateAll(items ...sessionstore.Item) []translate.Update {
	translator := translate.NewSession()
	var updates []translate.Update
	for _, item := range items {
		updates = append(updates, translator.Translate(item)...)
	}
	return updates
}

func TestUserInputBecomesUserMessageAndControlInputsAreHidden(t *testing.T) {
	updates := translateAll(
		controlInput(inbox.ControlMessage{Mode: inbox.UpdateSettings, Parameters: inbox.Settings{ReasoningEffort: llm.ReasoningEffortHigh}}),
		externalInput("fix the build"),
		controlInput(inbox.ControlMessage{Mode: inbox.StopWhenIdle}),
		controlInput(inbox.ControlMessage{Mode: inbox.Heartbeat, Reason: "waited 600 seconds"}),
		sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn-1", Type: session.TurnRegular}},
	)
	if len(updates) != 1 || updates[0].Kind != translate.UserMessage || updates[0].Text != "fix the build" {
		t.Fatalf("updates = %+v", updates)
	}
}

func TestModelOutputBecomesThoughtsAndMessagesWithStableIDs(t *testing.T) {
	item := modelResponse("resp-1",
		llm.Item{ProviderID: "rs-1", Type: llm.ItemReasoning, Data: llm.Reasoning{Summary: []string{"Looking at files.", "Found it."}}},
		message("msg-1", "Here is the fix."),
	)
	updates := translateAll(item)
	if len(updates) != 2 {
		t.Fatalf("updates = %+v", updates)
	}
	if updates[0].Kind != translate.AgentThought || !strings.Contains(updates[0].Text, "Looking at files.") || !strings.Contains(updates[0].Text, "Found it.") {
		t.Fatalf("thought = %+v", updates[0])
	}
	if updates[1].Kind != translate.AgentMessage || updates[1].Text != "Here is the fix." || updates[1].MessageID == "" {
		t.Fatalf("message = %+v", updates[1])
	}
	if again := translateAll(item); again[1].MessageID != updates[1].MessageID {
		t.Fatalf("message id is not stable: %q vs %q", updates[1].MessageID, again[1].MessageID)
	}
}

func TestEmptyReasoningProducesNoThought(t *testing.T) {
	updates := translateAll(modelResponse("resp-1", llm.Item{Type: llm.ItemReasoning, Data: llm.Reasoning{}}))
	if len(updates) != 0 {
		t.Fatalf("updates = %+v", updates)
	}
}

func TestBashCallLifecycle(t *testing.T) {
	input := operation.ShellInput{Command: "ls -la", Shell: "/bin/sh", Directory: "/w"}
	updates := translateAll(
		modelResponse("resp-1", call("call-1", "Bash", `{"command":"ls -la"}`)),
		toolStatus("call-1", tool.CallStatus{WaitingFor: []operation.ID{"op-1"}},
			shellOperation(t, "op-1", operation.StatusReady, operation.ShellState{Input: input})),
		toolStatus("call-1", tool.CallStatus{WaitingFor: []operation.ID{"op-1"}},
			shellOperation(t, "op-1", operation.StatusCompleted, operation.ShellState{Input: input, Result: &operation.ShellResult{Out: "file-a\nfile-b\n"}})),
	)
	if len(updates) != 3 {
		t.Fatalf("updates = %+v", updates)
	}
	started := updates[0]
	if started.Kind != translate.ToolCallStarted || started.Tool.CallID != "call-1" || started.Tool.Name != "Bash" ||
		started.Tool.Kind != translate.KindExecute || started.Tool.Title != "ls -la" || started.Tool.Status != translate.StatusPending ||
		!reflect.DeepEqual(started.Tool.RawInput, map[string]any{"command": "ls -la"}) {
		t.Fatalf("started = %+v", started.Tool)
	}
	if running := updates[1]; running.Kind != translate.ToolCallUpdated || running.Tool.Status != translate.StatusInProgress {
		t.Fatalf("running = %+v", running.Tool)
	}
	finished := updates[2]
	if finished.Kind != translate.ToolCallUpdated || finished.Tool.Status != translate.StatusCompleted || !strings.Contains(finished.Tool.Output, "file-a\nfile-b") {
		t.Fatalf("finished = %+v", finished.Tool)
	}
}

func TestNonZeroExitIsCompletedWithExitCodeInOutput(t *testing.T) {
	updates := translateAll(
		modelResponse("resp-1", call("call-1", "Bash", `{"command":"false"}`)),
		toolStatus("call-1", tool.CallStatus{WaitingFor: []operation.ID{"op-1"}},
			shellOperation(t, "op-1", operation.StatusCompleted, operation.ShellState{Result: &operation.ShellResult{ExitCode: 1}})),
	)
	last := updates[len(updates)-1].Tool
	if last.Status != translate.StatusCompleted || !strings.Contains(last.Output, "Exit code: 1") {
		t.Fatalf("tool = %+v", last)
	}
}

func TestCanceledAndFailedOperationsAreFailed(t *testing.T) {
	for _, status := range []operation.Status{operation.StatusCanceled, operation.StatusFailed} {
		updates := translateAll(
			modelResponse("resp-1", call("call-1", "Bash", `{"command":"sleep 30"}`)),
			toolStatus("call-1", tool.CallStatus{WaitingFor: []operation.ID{"op-1"}},
				shellOperation(t, "op-1", status, operation.ShellState{TerminalError: "shell operation " + string(status)})),
		)
		last := updates[len(updates)-1].Tool
		if last.Status != translate.StatusFailed || !strings.Contains(last.Output, string(status)) {
			t.Fatalf("%s: tool = %+v", status, last)
		}
	}
}

func TestInvalidToolArgumentsAreFailed(t *testing.T) {
	updates := translateAll(
		modelResponse("resp-1", call("call-1", "Bash", `{}`)),
		toolStatus("call-1", tool.CallStatus{Error: `bash argument "command" must be set`}),
	)
	if len(updates) != 2 {
		t.Fatalf("updates = %+v", updates)
	}
	if started := updates[0].Tool; started.Title != "Bash" {
		t.Fatalf("title for call without command = %q", started.Title)
	}
	if last := updates[1].Tool; last.Status != translate.StatusFailed || !strings.Contains(last.Output, "must be set") {
		t.Fatalf("tool = %+v", last)
	}
}

func TestViewImageIsAReadWithLocation(t *testing.T) {
	updates := translateAll(modelResponse("resp-1", call("call-1", "ViewImage", `{"path":"/w/chart.png"}`)))
	started := updates[0].Tool
	if started.Kind != translate.KindRead || !strings.Contains(started.Title, "/w/chart.png") ||
		!reflect.DeepEqual(started.Locations, []string{"/w/chart.png"}) {
		t.Fatalf("tool = %+v", started)
	}
}

func TestSkillUseIsARead(t *testing.T) {
	updates := translateAll(modelResponse("resp-1", call("call-1", "SkillUse", `{"name":"deploy"}`)))
	if started := updates[0].Tool; started.Kind != translate.KindRead || !strings.Contains(started.Title, "deploy") {
		t.Fatalf("tool = %+v", started)
	}
}

func TestUnknownToolIsOtherTitledByName(t *testing.T) {
	updates := translateAll(
		modelResponse("resp-1", call("call-1", "McpCall", `{"server":"x"}`)),
		toolStatus("call-1", tool.CallStatus{Error: `tool "McpCall" is not available`}),
	)
	started := updates[0].Tool
	if started.Kind != translate.KindOther || started.Title != "McpCall" {
		t.Fatalf("tool = %+v", started)
	}
	if last := updates[1].Tool; last.Status != translate.StatusFailed || !strings.Contains(last.Output, "not available") {
		t.Fatalf("result = %+v", last)
	}
}

func TestLongCommandTitlesAreShortened(t *testing.T) {
	command := strings.Repeat("echo hello && ", 40)
	encoded, _ := json.Marshal(map[string]string{"command": command})
	updates := translateAll(modelResponse("resp-1", call("call-1", "Bash", string(encoded))))
	if title := updates[0].Tool.Title; len([]rune(title)) > 120 || !strings.HasPrefix(command, strings.TrimSuffix(title, "…")) {
		t.Fatalf("title = %q", title)
	}
}

func TestMessageIDsAreUniqueAcrossResponses(t *testing.T) {
	updates := translateAll(
		modelResponse("resp-1", llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "a"}}),
		modelResponse("resp-2", llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "b"}}),
	)
	if len(updates) != 2 || updates[0].MessageID == "" || updates[0].MessageID == updates[1].MessageID {
		t.Fatalf("updates = %+v", updates)
	}
}

func TestSeveralToolCallsInOneResponse(t *testing.T) {
	updates := translateAll(
		modelResponse("resp-1", call("call-1", "Bash", `{"command":"one"}`), call("call-2", "Bash", `{"command":"two"}`)),
		toolStatus("call-2", tool.CallStatus{Error: "boom"}),
		toolStatus("call-1", tool.CallStatus{WaitingFor: []operation.ID{"op-1"}},
			shellOperation(t, "op-1", operation.StatusCompleted, operation.ShellState{Result: &operation.ShellResult{Out: "first"}})),
	)
	if len(updates) != 4 || updates[0].Tool.Title != "one" || updates[1].Tool.Title != "two" {
		t.Fatalf("updates = %+v", updates)
	}
	if updates[2].Tool.CallID != "call-2" || updates[2].Tool.Status != translate.StatusFailed ||
		updates[3].Tool.CallID != "call-1" || updates[3].Tool.Status != translate.StatusCompleted {
		t.Fatalf("results = %+v %+v", updates[2].Tool, updates[3].Tool)
	}
}

func TestStatusForUnseenCallIsStillReported(t *testing.T) {
	updates := translateAll(toolStatus("call-9", tool.CallStatus{Error: "interrupted"}))
	if len(updates) != 1 || updates[0].Kind != translate.ToolCallUpdated || updates[0].Tool.CallID != "call-9" || updates[0].Tool.Status != translate.StatusFailed {
		t.Fatalf("updates = %+v", updates)
	}
}

func TestRepeatedIdenticalStatusIsNotReportedTwice(t *testing.T) {
	running := toolStatus("call-1", tool.CallStatus{WaitingFor: []operation.ID{"op-1"}},
		shellOperation(t, "op-1", operation.StatusReady, operation.ShellState{}))
	updates := translateAll(modelResponse("resp-1", call("call-1", "Bash", `{"command":"x"}`)), running, running)
	if len(updates) != 2 {
		t.Fatalf("updates = %+v", updates)
	}
}
