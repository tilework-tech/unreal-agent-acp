package acpagent_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	acp "github.com/tilework-tech/unreal-agent-acp/acp/v1"
	"github.com/tilework-tech/unreal-agent-acp/internal/fakemodel"
)

func TestInitializeAdvertisesCapabilities(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence())
	response := c.initialize()

	if response.ProtocolVersion != acp.ProtocolVersionNumber || response.AgentInfo == nil || response.AgentInfo.Name != "unreal-agent-acp" {
		t.Fatalf("initialize = %+v", response)
	}
	capabilities := response.AgentCapabilities
	if capabilities.SessionCapabilities.Close == nil || !capabilities.PromptCapabilities.EmbeddedContext {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if capabilities.PromptCapabilities.Image || capabilities.PromptCapabilities.Audio || capabilities.McpCapabilities.Sse {
		t.Fatalf("advertised unsupported capabilities: %+v", capabilities)
	}
}

func TestInitializeNegotiatesDownToVersionOne(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence())
	var response acp.InitializeResponse
	c.mustCall(acp.AgentMethodInitialize, acp.InitializeRequest{ProtocolVersion: 2}, &response)
	if response.ProtocolVersion != 1 {
		t.Fatalf("protocolVersion = %d", response.ProtocolVersion)
	}
}

func TestNewSessionReturnsModelAndReasoningConfigOptions(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence())
	c.initialize()
	response := c.newSession(t.TempDir())

	if response.SessionId == "" {
		t.Fatal("empty session id")
	}
	model := configOption(response.ConfigOptions, "model")
	effort := configOption(response.ConfigOptions, "reasoning_effort")
	if model == nil || model.CurrentValue != "fake-model" {
		t.Fatalf("model option = %+v", model)
	}
	if effort == nil || effort.CurrentValue != "high" {
		t.Fatalf("reasoning option = %+v", effort)
	}
}

func TestNewSessionWithoutCredentialsIsAuthRequired(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence(), func(options *clientOptions) {
		options.env = func(env map[string]string) { delete(env, "UNREAL_HARNESS_LLM_API_KEY") }
	})
	c.initialize()
	err := c.call(acp.AgentMethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}}, nil)
	if rpcCode(err) != -32000 {
		t.Fatalf("err = %v, want auth_required (-32000)", err)
	}
}

func TestNewSessionRejectsRelativeCwd(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence())
	c.initialize()
	err := c.call(acp.AgentMethodSessionNew, acp.NewSessionRequest{Cwd: "relative", McpServers: []acp.McpServer{}}, nil)
	if rpcCode(err) != -32602 {
		t.Fatalf("err = %v, want invalid params", err)
	}
}

func TestPromptStreamsAgentMessageBeforeEndTurn(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence(fakemodel.Reasoning("Considering.").Then(fakemodel.Text("Hello from unreal"))))
	c.initialize()
	session := c.newSession(t.TempDir())

	response := c.prompt(session.SessionId, "hi")

	if response.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stopReason = %q", response.StopReason)
	}
	updates := c.received()
	if got := agentText(updates); got != "Hello from unreal" {
		t.Fatalf("agent text = %q", got)
	}
	var sawThought bool
	for _, update := range updates {
		if update.SessionId != session.SessionId {
			t.Fatalf("update for wrong session: %+v", update)
		}
		if update.Update.UserMessageChunk != nil {
			t.Fatalf("live prompt echoed the user's message: %+v", update)
		}
		if chunk := update.Update.AgentThoughtChunk; chunk != nil && chunk.Content.Text != nil && strings.Contains(chunk.Content.Text.Text, "Considering.") {
			sawThought = true
		}
	}
	if !sawThought {
		t.Fatal("reasoning summary was not sent as agent_thought_chunk")
	}
	time.Sleep(100 * time.Millisecond)
	if after := len(c.received()); after != len(updates) {
		t.Fatalf("%d session/update notifications arrived after the prompt response", after-len(updates))
	}
}

func TestPromptReportsBashToolCallLifecycle(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": "echo tool-output-marker"}),
		fakemodel.Text("done"),
	))
	c.initialize()
	session := c.newSession(t.TempDir())

	response := c.prompt(session.SessionId, "run it")

	if response.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stopReason = %q", response.StopReason)
	}
	var started *acp.SessionUpdateToolCall
	var completed *acp.SessionToolCallUpdate
	for _, update := range c.received() {
		if call := update.Update.ToolCall; call != nil && call.ToolCallId == "call-1" {
			started = call
		}
		if call := update.Update.ToolCallUpdate; call != nil && call.ToolCallId == "call-1" {
			if started == nil {
				t.Fatal("tool_call_update arrived before tool_call")
			}
			if call.Status != nil && *call.Status == acp.ToolCallStatusCompleted {
				completed = call
			}
		}
	}
	if started == nil || started.Kind != acp.ToolKindExecute || started.Title != "echo tool-output-marker" {
		t.Fatalf("tool_call = %+v", started)
	}
	if completed == nil || len(completed.Content) == 0 || completed.Content[0].Content == nil ||
		!strings.Contains(completed.Content[0].Content.Content.Text.Text, "tool-output-marker") {
		t.Fatalf("completed tool_call_update = %+v", completed)
	}
}

func TestPromptFlattensResourceLinksAndEmbeddedResources(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence(fakemodel.Text("ok")))
	c.initialize()
	session := c.newSession(t.TempDir())
	mime := "text/plain"
	var response acp.PromptResponse
	c.mustCall(acp.AgentMethodSessionPrompt, acp.PromptRequest{SessionId: session.SessionId, Prompt: []acp.ContentBlock{
		{Text: &acp.ContentBlockText{Text: "Compare these:"}},
		{ResourceLink: &acp.ContentBlockResourceLink{Name: "main.go", Uri: "file:///w/main.go"}},
		{Resource: &acp.ContentBlockResource{Resource: acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{Uri: "file:///w/notes.txt", Text: "embedded-contents", MimeType: &mime},
		}}},
	}}, &response)

	texts := c.model.Requests()[0].UserTexts()
	prompt := texts[len(texts)-1]
	for _, want := range []string{"Compare these:", "file:///w/main.go", "file:///w/notes.txt", "embedded-contents"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("model prompt %q is missing %q", prompt, want)
		}
	}
}

func TestPromptRejectsImageContent(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence())
	c.initialize()
	session := c.newSession(t.TempDir())
	err := c.call(acp.AgentMethodSessionPrompt, acp.PromptRequest{SessionId: session.SessionId, Prompt: []acp.ContentBlock{
		{Image: &acp.ContentBlockImage{Data: "aGVsbG8=", MimeType: "image/png"}},
	}}, nil)
	if rpcCode(err) != -32602 {
		t.Fatalf("err = %v, want invalid params", err)
	}
}

func TestPromptForUnknownSessionFails(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence())
	c.initialize()
	if err := c.call(acp.AgentMethodSessionPrompt, textPrompt("nope", "hi"), nil); rpcCode(err) != -32602 {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionCancelDuringModelCallEndsPromptCancelled(t *testing.T) {
	requested := make(chan struct{}, 1)
	c := startAgent(t, func(request fakemodel.Request) fakemodel.Reply {
		select {
		case requested <- struct{}{}:
		default:
		}
		<-request.Context().Done()
		return fakemodel.Reply{}
	})
	c.initialize()
	session := c.newSession(t.TempDir())
	done := c.promptAsync(context.Background(), session.SessionId, "think")
	await(t, "model call", requested)

	c.cancel(session.SessionId)

	got := await(t, "prompt response", done)
	if got.err != nil || got.response.StopReason != acp.StopReasonCancelled {
		t.Fatalf("prompt = %+v, %v", got.response, got.err)
	}
}

func TestSessionCancelDuringCommandMarksToolFailedBeforeResponding(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": "touch started && sleep 30"}),
	))
	c.initialize()
	cwd := t.TempDir()
	session := c.newSession(cwd)
	done := c.promptAsync(context.Background(), session.SessionId, "sleep")
	waitForFile(t, filepath.Join(cwd, "started"))

	c.cancel(session.SessionId)

	got := await(t, "prompt response", done)
	if got.err != nil || got.response.StopReason != acp.StopReasonCancelled {
		t.Fatalf("prompt = %+v, %v", got.response, got.err)
	}
	var failed bool
	for _, update := range c.received() {
		if call := update.Update.ToolCallUpdate; call != nil && call.ToolCallId == "call-1" && call.Status != nil && *call.Status == acp.ToolCallStatusFailed {
			failed = true
		}
	}
	if !failed {
		t.Fatal("cancelled tool call was not reported as failed before the prompt response")
	}
}

func TestCancelSentRightAfterPromptIsNotLost(t *testing.T) {
	c := startAgent(t, func(request fakemodel.Request) fakemodel.Reply {
		if texts := request.UserTexts(); texts[len(texts)-1] == "first" {
			<-request.Context().Done() // only a delivered cancel ends this turn
			return fakemodel.Reply{}
		}
		return fakemodel.Text("second")
	})
	c.initialize()
	session := c.newSession(t.TempDir())

	// Both frames in one write, so the cancel is read immediately after the prompt.
	frames := fmt.Sprintf(`{"jsonrpc":"2.0","id":"raw-prompt","method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":"first"}]}}`+"\n"+
		`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":%q}}`+"\n", session.SessionId, session.SessionId)
	if _, err := io.WriteString(c.raw, frames); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		err := c.call(acp.AgentMethodSessionPrompt, textPrompt(session.SessionId, "second"), nil)
		if err == nil {
			return
		}
		if rpcCode(err) != -32603 || time.Now().After(deadline) {
			t.Fatalf("the first prompt was never cancelled: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAbandonedPromptRequestStopsTheTurn(t *testing.T) {
	requested := make(chan struct{}, 1)
	abandoned := make(chan struct{}, 1)
	c := startAgent(t, func(request fakemodel.Request) fakemodel.Reply {
		select {
		case requested <- struct{}{}:
		default:
		}
		<-request.Context().Done()
		select {
		case abandoned <- struct{}{}:
		default:
		}
		return fakemodel.Reply{}
	})
	c.initialize()
	session := c.newSession(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := c.promptAsync(ctx, session.SessionId, "think")
	await(t, "model call", requested)

	cancel() // sends $/cancel_request for the prompt

	if got := await(t, "prompt to stop", done); got.err == nil {
		t.Fatalf("abandoned prompt returned %+v", got.response)
	}
	await(t, "agent to abandon the model call", abandoned)
	if err := c.call(acp.AgentMethodSessionClose, acp.CloseSessionRequest{SessionId: session.SessionId}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentPromptInSameSessionIsRejected(t *testing.T) {
	release := make(chan struct{})
	requested := make(chan struct{}, 2)
	c := startAgent(t, func(request fakemodel.Request) fakemodel.Reply {
		select {
		case requested <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-request.Context().Done():
		}
		return fakemodel.Text("ok")
	})
	c.initialize()
	session := c.newSession(t.TempDir())
	first := c.promptAsync(context.Background(), session.SessionId, "first")
	await(t, "model call", requested)

	if err := c.call(acp.AgentMethodSessionPrompt, textPrompt(session.SessionId, "second"), nil); rpcCode(err) != -32603 {
		t.Fatalf("err = %v, want an internal error", err)
	}
	close(release)
	if got := await(t, "first prompt", first); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestSetConfigOptionChangesModelAndReasoningForNextPrompt(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence(fakemodel.Text("a")))
	c.initialize()
	session := c.newSession(t.TempDir())

	var modelResponse acp.SetSessionConfigOptionResponse
	c.mustCall(acp.AgentMethodSessionSetConfigOption, map[string]any{
		"sessionId": session.SessionId, "configId": "model", "value": "custom-model",
	}, &modelResponse)
	var effortResponse acp.SetSessionConfigOptionResponse
	c.mustCall(acp.AgentMethodSessionSetConfigOption, map[string]any{
		"sessionId": session.SessionId, "configId": "reasoning_effort", "value": "low",
	}, &effortResponse)

	if option := configOption(modelResponse.ConfigOptions, "model"); option == nil || option.CurrentValue != "custom-model" {
		t.Fatalf("model option after set = %+v", option)
	}
	if option := configOption(effortResponse.ConfigOptions, "reasoning_effort"); option == nil || option.CurrentValue != "low" {
		t.Fatalf("effort option after set = %+v", option)
	}
	c.prompt(session.SessionId, "go")
	request := c.model.Requests()[0]
	if request.Model() != "custom-model" || request.ReasoningEffort() != "low" {
		t.Fatalf("model request used model %q effort %q", request.Model(), request.ReasoningEffort())
	}
}

func TestSetConfigOptionRejectsUnknownOptionsAndValues(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence())
	c.initialize()
	session := c.newSession(t.TempDir())
	for _, params := range []map[string]any{
		{"sessionId": session.SessionId, "configId": "nope", "value": "x"},
		{"sessionId": session.SessionId, "configId": "reasoning_effort", "value": "ludicrous"},
	} {
		if err := c.call(acp.AgentMethodSessionSetConfigOption, params, nil); rpcCode(err) != -32602 {
			t.Errorf("set %v: err = %v, want invalid params", params, err)
		}
	}
}

func TestCloseStopsRunningPromptAndForgetsSession(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": "touch started && sleep 30"}),
	))
	c.initialize()
	cwd := t.TempDir()
	session := c.newSession(cwd)
	done := c.promptAsync(context.Background(), session.SessionId, "sleep")
	waitForFile(t, filepath.Join(cwd, "started"))

	c.mustCall(acp.AgentMethodSessionClose, acp.CloseSessionRequest{SessionId: session.SessionId}, nil)

	if got := await(t, "prompt after close", done); got.err != nil || got.response.StopReason != acp.StopReasonCancelled {
		t.Fatalf("prompt = %+v, %v", got.response, got.err)
	}
	var failed bool
	for _, update := range c.received() {
		if call := update.Update.ToolCallUpdate; call != nil && call.ToolCallId == "call-1" && call.Status != nil && *call.Status == acp.ToolCallStatusFailed {
			failed = true
		}
	}
	if !failed {
		t.Fatal("the closed session's running tool call was never reported as failed")
	}
	if err := c.call(acp.AgentMethodSessionPrompt, textPrompt(session.SessionId, "again"), nil); rpcCode(err) != -32602 {
		t.Fatalf("prompt after close err = %v", err)
	}
}

func TestSessionsAreIndependent(t *testing.T) {
	c := startAgent(t, func(request fakemodel.Request) fakemodel.Reply {
		texts := request.UserTexts()
		return fakemodel.Text("echo: " + texts[len(texts)-1])
	})
	c.initialize()
	first, second := c.newSession(t.TempDir()), c.newSession(t.TempDir())
	c.prompt(first.SessionId, "alpha")
	c.prompt(second.SessionId, "beta")

	var firstUpdates, secondUpdates []acp.SessionNotification
	for _, update := range c.received() {
		switch update.SessionId {
		case first.SessionId:
			firstUpdates = append(firstUpdates, update)
		case second.SessionId:
			secondUpdates = append(secondUpdates, update)
		}
	}
	if agentText(firstUpdates) != "echo: alpha" || agentText(secondUpdates) != "echo: beta" {
		t.Fatalf("first=%q second=%q", agentText(firstUpdates), agentText(secondUpdates))
	}
	if requests := c.model.Requests(); slices.Contains(requests[1].UserTexts(), "alpha") {
		t.Fatal("second session saw the first session's history")
	}
}

func TestUnknownMethodsAndSessionsAreRejected(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence())
	c.initialize()
	if err := c.call("session/teleport", map[string]any{}, nil); rpcCode(err) != -32601 {
		t.Errorf("unknown method err = %v", err)
	}
	if err := c.call(acp.AgentMethodSessionClose, acp.CloseSessionRequest{SessionId: "nope"}, nil); rpcCode(err) != -32602 {
		t.Errorf("close unknown session err = %v", err)
	}
	if err := c.call(acp.AgentMethodSessionSetConfigOption, map[string]any{"sessionId": "nope", "configId": "model", "value": "x"}, nil); rpcCode(err) != -32602 {
		t.Errorf("set option on unknown session err = %v", err)
	}
}

func TestNewSessionAcceptsMCPServersItDoesNotRunYet(t *testing.T) {
	c := startAgent(t, fakemodel.Sequence(fakemodel.Text("ok")))
	c.initialize()
	var response acp.NewSessionResponse
	c.mustCall(acp.AgentMethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "fs", Command: "/bin/true", Args: []string{}, Env: []acp.EnvVariable{}}},
	}}, &response)
	if c.prompt(response.SessionId, "hi").StopReason != acp.StopReasonEndTurn {
		t.Fatal("session with MCP servers could not prompt")
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
