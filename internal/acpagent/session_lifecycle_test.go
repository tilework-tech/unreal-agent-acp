package acpagent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	acp "github.com/tilework-tech/unreal-agent-acp/acp/v1"
	"github.com/tilework-tech/unreal-agent-acp/internal/fakemodel"
)

func withPermission(answer func(context.Context, acp.RequestPermissionRequest) acp.RequestPermissionResponse) func(*clientOptions) {
	return func(options *clientOptions) { options.permission = answer }
}

func withStore(dir string) func(*clientOptions) {
	return func(options *clientOptions) { options.store = dir }
}

func choose(kind acp.PermissionOptionKind) func(context.Context, acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	return func(_ context.Context, request acp.RequestPermissionRequest) acp.RequestPermissionResponse {
		return selectKind(request, kind)
	}
}

func toolStatuses(updates []acp.SessionNotification, callID acp.ToolCallId) []acp.ToolCallStatus {
	var statuses []acp.ToolCallStatus
	for _, update := range updates {
		if call := update.Update.ToolCallUpdate; call != nil && call.ToolCallId == callID && call.Status != nil {
			statuses = append(statuses, *call.Status)
		}
	}
	return statuses
}

func TestInitializeAdvertisesSessionLifecycle(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence())
	capabilities := c.initialize().AgentCapabilities
	if !capabilities.LoadSession || capabilities.SessionCapabilities.List == nil || capabilities.SessionCapabilities.Resume == nil {
		t.Fatalf("capabilities = %+v", capabilities)
	}
}

func TestBashAsksPermissionBeforeRunning(t *testing.T) {
	var c *client
	statusWhenAsked := make(chan []acp.ToolCallStatus, 1)
	c = startAgent(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": "touch approved-file"}),
		fakemodel.Text("done"),
	), withPermission(func(ctx context.Context, request acp.RequestPermissionRequest) acp.RequestPermissionResponse {
		var seen []acp.ToolCallStatus
		for _, update := range c.received() {
			if call := update.Update.ToolCall; call != nil && call.ToolCallId == request.ToolCall.ToolCallId {
				seen = append(seen, call.Status)
			}
		}
		seen = append(seen, toolStatuses(c.received(), request.ToolCall.ToolCallId)...)
		statusWhenAsked <- seen
		return selectKind(request, acp.PermissionOptionKindAllowOnce)
	}))
	c.initialize()
	cwd := t.TempDir()
	session := c.newSession(cwd)

	c.prompt(session.SessionId, "make a file")

	asked := c.permissionRequests()
	if len(asked) != 1 {
		t.Fatalf("permission requests = %d", len(asked))
	}
	request := asked[0]
	if request.SessionId != session.SessionId || request.ToolCall.ToolCallId != "call-1" ||
		request.ToolCall.Title == nil || *request.ToolCall.Title != "touch approved-file" {
		t.Fatalf("permission request = %+v", request)
	}
	var kinds []acp.PermissionOptionKind
	for _, option := range request.Options {
		kinds = append(kinds, option.Kind)
	}
	if !slices.Equal(kinds, []acp.PermissionOptionKind{acp.PermissionOptionKindAllowOnce, acp.PermissionOptionKindAllowAlways, acp.PermissionOptionKindRejectOnce}) {
		t.Fatalf("options = %v", kinds)
	}
	if seen := await(t, "statuses at permission time", statusWhenAsked); !slices.Equal(seen, []acp.ToolCallStatus{acp.ToolCallStatusPending}) {
		t.Fatalf("tool call statuses when permission was requested = %v, want only pending", seen)
	}
	waitForFile(t, filepath.Join(cwd, "approved-file"))
	if statuses := toolStatuses(c.received(), "call-1"); !slices.Contains(statuses, acp.ToolCallStatusCompleted) {
		t.Fatalf("tool statuses = %v", statuses)
	}
}

func TestRejectedBashFailsAndTheModelIsTold(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": "touch rejected-file"}),
		fakemodel.Text("ok, I won't"),
	), withPermission(choose(acp.PermissionOptionKindRejectOnce)))
	c.initialize()
	cwd := t.TempDir()
	session := c.newSession(cwd)

	if response := c.prompt(session.SessionId, "make a file"); response.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stopReason = %q", response.StopReason)
	}
	if statuses := toolStatuses(c.received(), "call-1"); !slices.Contains(statuses, acp.ToolCallStatusFailed) {
		t.Fatalf("tool statuses = %v", statuses)
	}
	requests := c.model.Requests()
	if output := requests[len(requests)-1].ToolOutputs()["call-1"]; !strings.Contains(strings.ToLower(output), "rejected") {
		t.Fatalf("model saw tool output %q", output)
	}
	if _, err := os.Stat(filepath.Join(cwd, "rejected-file")); err == nil {
		t.Fatal("rejected command ran")
	}
}

func TestAllowAlwaysCoversLaterCommandsInTheSameSessionOnly(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": "touch one"}),
		fakemodel.Text("one"),
		fakemodel.ToolCall("call-2", "Bash", map[string]any{"command": "touch two"}),
		fakemodel.Text("two"),
		fakemodel.ToolCall("call-3", "Bash", map[string]any{"command": "touch three"}),
		fakemodel.Text("three"),
	), withPermission(choose(acp.PermissionOptionKindAllowAlways)))
	c.initialize()
	cwd := t.TempDir()
	session := c.newSession(cwd)
	c.prompt(session.SessionId, "first")
	c.prompt(session.SessionId, "second")
	if asked := len(c.permissionRequests()); asked != 1 {
		t.Fatalf("permission requests = %d, want 1", asked)
	}
	waitForFile(t, filepath.Join(cwd, "two"))

	other := c.newSession(t.TempDir())
	c.prompt(other.SessionId, "third")
	if asked := len(c.permissionRequests()); asked != 2 {
		t.Fatalf("another session was not asked: %d requests", asked)
	}
}

func TestCancelWhilePermissionIsPendingEndsPromptCancelled(t *testing.T) {
	pending := make(chan struct{}, 1)
	c := startAgent(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": "touch never"}),
	), withPermission(func(ctx context.Context, request acp.RequestPermissionRequest) acp.RequestPermissionResponse {
		pending <- struct{}{}
		<-ctx.Done() // the user never answers; the client answers "cancelled" on session/cancel
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}
	}))
	c.initialize()
	cwd := t.TempDir()
	session := c.newSession(cwd)
	done := c.promptAsync(context.Background(), session.SessionId, "do it")
	await(t, "permission request", pending)

	c.cancel(session.SessionId)

	got := await(t, "prompt response", done)
	if got.err != nil || got.response.StopReason != acp.StopReasonCancelled {
		t.Fatalf("prompt = %+v, %v", got.response, got.err)
	}
	if statuses := toolStatuses(c.received(), "call-1"); !slices.Contains(statuses, acp.ToolCallStatusFailed) {
		t.Fatalf("tool statuses = %v", statuses)
	}
	if _, err := os.Stat(filepath.Join(cwd, "never")); err == nil {
		t.Fatal("command ran after the prompt was cancelled")
	}
}

func TestFullAccessModeSkipsPermission(t *testing.T) {
	for _, via := range []string{"set_mode", "set_config_option"} {
		t.Run(via, func(t *testing.T) {
			c := startAgent(t, fakemodel.Sequence(
				fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": "true"}),
				fakemodel.Text("done"),
			))
			c.initialize()
			session := c.newSession(t.TempDir())
			if session.Modes == nil || session.Modes.CurrentModeId != "ask" || configOption(session.ConfigOptions, "mode") == nil {
				t.Fatalf("new session modes = %+v", session.Modes)
			}
			if via == "set_mode" {
				c.mustCall(acp.AgentMethodSessionSetMode, acp.SetSessionModeRequest{SessionId: session.SessionId, ModeId: "full-access"}, nil)
			} else {
				var response acp.SetSessionConfigOptionResponse
				c.mustCall(acp.AgentMethodSessionSetConfigOption, map[string]any{"sessionId": session.SessionId, "configId": "mode", "value": "full-access"}, &response)
				if option := configOption(response.ConfigOptions, "mode"); option == nil || option.CurrentValue != "full-access" {
					t.Fatalf("mode option = %+v", option)
				}
			}
			c.prompt(session.SessionId, "go")
			if asked := len(c.permissionRequests()); asked != 0 {
				t.Fatalf("permission requests = %d", asked)
			}
			var options acp.SetSessionConfigOptionResponse
			c.mustCall(acp.AgentMethodSessionSetConfigOption, map[string]any{"sessionId": session.SessionId, "configId": "reasoning_effort", "value": "high"}, &options)
			if option := configOption(options.ConfigOptions, "mode"); option == nil || option.CurrentValue != "full-access" {
				t.Fatalf("mode option out of sync with set_mode: %+v", option)
			}
		})
	}
}

func TestSetModeRejectsUnknownMode(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence())
	c.initialize()
	session := c.newSession(t.TempDir())
	if err := c.call(acp.AgentMethodSessionSetMode, acp.SetSessionModeRequest{SessionId: session.SessionId, ModeId: "yolo"}, nil); rpcCode(err) != -32602 {
		t.Fatalf("set_mode err = %v", err)
	}
	if err := c.call(acp.AgentMethodSessionSetConfigOption, map[string]any{"sessionId": session.SessionId, "configId": "mode", "value": "yolo"}, nil); rpcCode(err) != -32602 {
		t.Fatalf("set_config_option err = %v", err)
	}
}

// liveConversation runs a prompt with a tool call and returns the session id
// and every non-user update the client received live.
func liveConversation(t *testing.T, c *client, cwd string) (acp.SessionId, []json.RawMessage) {
	t.Helper()
	c.initialize()
	session := c.newSession(cwd)
	c.prompt(session.SessionId, "list files")
	return session.SessionId, c.receivedRaw()
}

func conversationReplies() func(fakemodel.Request) fakemodel.Reply {
	return fakemodel.Sequence(
		fakemodel.Text("Let me look.").Then(fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": "echo listing"})),
		fakemodel.Text("Here they are."),
		fakemodel.Text("Continuing."),
	)
}

func TestLoadReplaysTheConversationBeforeResponding(t *testing.T) {
	store := t.TempDir()
	cwd := t.TempDir()
	model := fakemodel.Start(t, conversationReplies())
	sessionID, live := liveConversation(t, startAgentWithModel(t, model, withStore(store)), cwd)

	// A fresh agent process over the same store.
	loader := startAgentWithModel(t, model, withStore(store))
	loader.initialize()
	var response acp.LoadSessionResponse
	loader.mustCall(acp.AgentMethodSessionLoad, acp.LoadSessionRequest{SessionId: sessionID, Cwd: cwd, McpServers: []acp.McpServer{}}, &response)

	replayed := loader.received()
	if len(replayed) == 0 || replayed[0].Update.UserMessageChunk == nil ||
		replayed[0].Update.UserMessageChunk.Content.Text.Text != "list files" {
		t.Fatalf("replay should start with the user's message, got %+v", replayed)
	}
	var replayedWithoutUser []json.RawMessage
	for index, update := range loader.receivedRaw() {
		if replayed[index].Update.UserMessageChunk == nil {
			replayedWithoutUser = append(replayedWithoutUser, update)
		}
	}
	if len(replayedWithoutUser) != len(live) {
		t.Fatalf("replayed %d updates, live had %d", len(replayedWithoutUser), len(live))
	}
	for index := range live {
		if string(replayedWithoutUser[index]) != string(live[index]) {
			t.Fatalf("update %d differs:\nlive:   %s\nreplay: %s", index, live[index], replayedWithoutUser[index])
		}
	}
	if configOption(response.ConfigOptions, "model") == nil {
		t.Fatalf("load response = %+v", response)
	}

	loader.prompt(sessionID, "continue")
	requests := model.Requests()
	if texts := requests[len(requests)-1].UserTexts(); !slices.Contains(texts, "list files") {
		t.Fatalf("loaded session lost history: %q", texts)
	}
}

func TestResumeRestoresWithoutReplayOrModelCalls(t *testing.T) {
	store := t.TempDir()
	cwd := t.TempDir()
	model := fakemodel.Start(t, conversationReplies())
	sessionID, _ := liveConversation(t, startAgentWithModel(t, model, withStore(store)), cwd)
	before := len(model.Requests())

	resumer := startAgentWithModel(t, model, withStore(store))
	resumer.initialize()
	resumer.mustCall(acp.AgentMethodSessionResume, acp.ResumeSessionRequest{SessionId: sessionID, Cwd: cwd}, nil)

	if updates := resumer.received(); len(updates) != 0 {
		t.Fatalf("resume sent %d updates", len(updates))
	}
	if after := len(model.Requests()); after != before {
		t.Fatalf("resume called the model")
	}
	resumer.prompt(sessionID, "continue")
	requests := model.Requests()
	if texts := requests[len(requests)-1].UserTexts(); !slices.Contains(texts, "list files") {
		t.Fatalf("resumed session lost history: %q", texts)
	}
}

func TestLoadUnknownSessionFails(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence())
	c.initialize()
	err := c.call(acp.AgentMethodSessionLoad, acp.LoadSessionRequest{SessionId: "missing", Cwd: t.TempDir(), McpServers: []acp.McpServer{}}, nil)
	if rpcCode(err) != -32602 {
		t.Fatalf("load err = %v", err)
	}
	if err := c.call(acp.AgentMethodSessionResume, acp.ResumeSessionRequest{SessionId: "missing", Cwd: t.TempDir()}, nil); rpcCode(err) != -32602 {
		t.Fatalf("resume err = %v", err)
	}
}

func TestListSessionsFiltersByCwd(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence(fakemodel.Text("a"), fakemodel.Text("b")))
	c.initialize()
	firstCwd, secondCwd := t.TempDir(), t.TempDir()
	first := c.newSession(firstCwd)
	c.prompt(first.SessionId, "Refactor the parser")
	second := c.newSession(secondCwd)
	c.prompt(second.SessionId, "Write docs")

	var all acp.ListSessionsResponse
	c.mustCall(acp.AgentMethodSessionList, acp.ListSessionsRequest{}, &all)
	if len(all.Sessions) != 2 {
		t.Fatalf("sessions = %+v", all.Sessions)
	}
	var filtered acp.ListSessionsResponse
	c.mustCall(acp.AgentMethodSessionList, acp.ListSessionsRequest{Cwd: &firstCwd}, &filtered)
	if len(filtered.Sessions) != 1 {
		t.Fatalf("filtered sessions = %+v", filtered.Sessions)
	}
	info := filtered.Sessions[0]
	if info.SessionId != first.SessionId || info.Cwd != firstCwd || info.Title == nil || *info.Title != "Refactor the parser" || info.UpdatedAt == nil {
		t.Fatalf("session info = %+v", info)
	}
	if _, err := time.Parse(time.RFC3339, *info.UpdatedAt); err != nil {
		t.Fatalf("updatedAt %q is not RFC 3339: %v", *info.UpdatedAt, err)
	}
}
