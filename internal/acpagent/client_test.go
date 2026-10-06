package acpagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tilework-tech/unreal-agent-acp/acp/jsonrpc"
	acp "github.com/tilework-tech/unreal-agent-acp/acp/v1"
	"github.com/tilework-tech/unreal-agent-acp/internal/acpagent"
	"github.com/tilework-tech/unreal-agent-acp/internal/fakemodel"
	"github.com/tilework-tech/unreal-agent-acp/internal/schemacheck"
	"github.com/tilework-tech/unreal-agent-acp/internal/unrealrt"
)

// responseDefinitions maps agent methods to the schema definition of their result.
var responseDefinitions = map[string]string{
	acp.AgentMethodInitialize:             "InitializeResponse",
	acp.AgentMethodSessionNew:             "NewSessionResponse",
	acp.AgentMethodSessionLoad:            "LoadSessionResponse",
	acp.AgentMethodSessionResume:          "ResumeSessionResponse",
	acp.AgentMethodSessionList:            "ListSessionsResponse",
	acp.AgentMethodSessionPrompt:          "PromptResponse",
	acp.AgentMethodSessionClose:           "CloseSessionResponse",
	acp.AgentMethodSessionSetConfigOption: "SetSessionConfigOptionResponse",
	acp.AgentMethodSessionSetMode:         "SetSessionModeResponse",
}

// client is an ACP client speaking to an in-process agent over pipes.
type client struct {
	t     *testing.T
	conn  *jsonrpc.Conn
	raw   io.Writer // writes frames to the agent without going through conn
	model *fakemodel.Server
	store string

	mu         sync.Mutex
	updates    []acp.SessionNotification
	rawUpdates []json.RawMessage
	notify     chan struct{}

	// permission answers session/request_permission; nil selects allow_once.
	permission func(context.Context, acp.RequestPermissionRequest) acp.RequestPermissionResponse
	permMu     sync.Mutex
	asked      []acp.RequestPermissionRequest
	// cancelled is closed per session by cancel, as a spec-compliant client
	// answers its pending permission requests with "cancelled".
	cancelled map[acp.SessionId]chan struct{}
}

type clientOptions struct {
	env        func(map[string]string)
	store      string
	permission func(context.Context, acp.RequestPermissionRequest) acp.RequestPermissionResponse
}

func startAgent(t *testing.T, handler func(fakemodel.Request) fakemodel.Reply, options ...func(*clientOptions)) *client {
	t.Helper()
	model := fakemodel.Start(t, handler)
	return startAgentWithModel(t, model, options...)
}

func startAgentWithModel(t *testing.T, model *fakemodel.Server, options ...func(*clientOptions)) *client {
	t.Helper()
	settings := clientOptions{store: t.TempDir()}
	for _, option := range options {
		option(&settings)
	}
	env := model.Env()
	if settings.env != nil {
		settings.env(env)
	}

	agentIn, clientOut := io.Pipe()
	clientIn, agentOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- acpagent.Serve(ctx, agentIn, agentOut, acpagent.Config{
			Name:    "unreal-agent-acp",
			Version: "test",
			Runtime: unrealrt.Config{
				SessionDirectory: settings.store,
				Getenv:           func(name string) string { return env[name] },
			},
		})
		agentOut.Close()
	}()

	c := &client{
		t: t, model: model, store: settings.store, notify: make(chan struct{}, 1),
		permission: settings.permission, cancelled: map[acp.SessionId]chan struct{}{},
	}
	c.raw = clientOut
	c.conn = jsonrpc.NewConn(clientIn, clientOut, c)
	clientDone := make(chan struct{})
	go func() {
		c.conn.Serve(context.Background())
		close(clientDone)
	}()
	t.Cleanup(func() {
		clientOut.Close()
		cancel()
		select {
		case <-served:
		case <-time.After(15 * time.Second):
			t.Error("agent did not shut down")
		}
		<-clientDone
	})
	return c
}

func (c *client) HandleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case acp.ClientMethodSessionRequestPermission:
		if err := schemacheck.V1("RequestPermissionRequest", params); err != nil {
			c.t.Errorf("agent sent invalid permission request %s: %v", params, err)
		}
		var request acp.RequestPermissionRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, err
		}
		c.permMu.Lock()
		c.asked = append(c.asked, request)
		answer := c.permission
		cancelled := c.cancelSignal(request.SessionId)
		c.permMu.Unlock()
		if answer == nil {
			return selectKind(request, acp.PermissionOptionKindAllowOnce), nil
		}
		answered := make(chan acp.RequestPermissionResponse, 1)
		go func() { answered <- answer(ctx, request) }()
		select {
		case response := <-answered:
			return response, nil
		case <-cancelled:
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	default:
		return nil, jsonrpc.ErrMethodNotFound(method)
	}
}

func (c *client) HandleNotification(_ context.Context, method string, params json.RawMessage) {
	if method != acp.ClientMethodSessionUpdate {
		c.t.Errorf("unexpected notification %s", method)
		return
	}
	if err := schemacheck.V1("SessionNotification", params); err != nil {
		c.t.Errorf("agent sent invalid session/update %s: %v", params, err)
	}
	var notification acp.SessionNotification
	if err := json.Unmarshal(params, &notification); err != nil {
		c.t.Errorf("decode session/update: %v", err)
		return
	}
	c.mu.Lock()
	c.updates = append(c.updates, notification)
	c.rawUpdates = append(c.rawUpdates, append(json.RawMessage(nil), params...))
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *client) permissionRequests() []acp.RequestPermissionRequest {
	c.permMu.Lock()
	defer c.permMu.Unlock()
	return append([]acp.RequestPermissionRequest(nil), c.asked...)
}

// call sends a request, validates the result against the schema, and decodes it.
func (c *client) call(method string, params, result any) error {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.callContext(ctx, method, params, result)
}

func (c *client) callContext(ctx context.Context, method string, params, result any) error {
	var raw json.RawMessage
	if err := c.conn.Call(ctx, method, params, &raw); err != nil {
		return err
	}
	if definition, ok := responseDefinitions[method]; ok {
		if err := schemacheck.V1(definition, raw); err != nil {
			c.t.Errorf("%s returned invalid result %s: %v", method, raw, err)
		}
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(raw, result)
}

func (c *client) mustCall(method string, params, result any) {
	c.t.Helper()
	if err := c.call(method, params, result); err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
}

func (c *client) initialize() acp.InitializeResponse {
	c.t.Helper()
	var response acp.InitializeResponse
	c.mustCall(acp.AgentMethodInitialize, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}, &response)
	return response
}

func (c *client) newSession(cwd string) acp.NewSessionResponse {
	c.t.Helper()
	var response acp.NewSessionResponse
	c.mustCall(acp.AgentMethodSessionNew, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}}, &response)
	return response
}

func textPrompt(sessionID acp.SessionId, text string) acp.PromptRequest {
	return acp.PromptRequest{SessionId: sessionID, Prompt: []acp.ContentBlock{{Text: &acp.ContentBlockText{Text: text}}}}
}

func (c *client) prompt(sessionID acp.SessionId, text string) acp.PromptResponse {
	c.t.Helper()
	var response acp.PromptResponse
	c.mustCall(acp.AgentMethodSessionPrompt, textPrompt(sessionID, text), &response)
	return response
}

type promptOutcome struct {
	response acp.PromptResponse
	err      error
}

func (c *client) promptAsync(ctx context.Context, sessionID acp.SessionId, text string) <-chan promptOutcome {
	done := make(chan promptOutcome, 1)
	go func() {
		var response acp.PromptResponse
		err := c.callContext(ctx, acp.AgentMethodSessionPrompt, textPrompt(sessionID, text), &response)
		done <- promptOutcome{response, err}
	}()
	return done
}

// cancelSignal must be called with permMu held.
func (c *client) cancelSignal(sessionID acp.SessionId) chan struct{} {
	signal, ok := c.cancelled[sessionID]
	if !ok {
		signal = make(chan struct{})
		c.cancelled[sessionID] = signal
	}
	return signal
}

func (c *client) cancel(sessionID acp.SessionId) {
	c.t.Helper()
	c.permMu.Lock()
	close(c.cancelSignal(sessionID))
	c.cancelled[sessionID] = make(chan struct{})
	c.permMu.Unlock()
	if err := c.conn.Notify(context.Background(), acp.AgentMethodSessionCancel, acp.CancelNotification{SessionId: sessionID}); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) received() []acp.SessionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]acp.SessionNotification(nil), c.updates...)
}

func (c *client) receivedRaw() []json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]json.RawMessage(nil), c.rawUpdates...)
}

func (c *client) waitForUpdate(what string, match func(acp.SessionNotification) bool) acp.SessionNotification {
	c.t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		for _, update := range c.received() {
			if match(update) {
				return update
			}
		}
		select {
		case <-c.notify:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			c.t.Fatalf("timed out waiting for %s; received %d updates", what, len(c.received()))
		}
	}
}

func await[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func rpcCode(err error) int {
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) {
		return rpcErr.Code
	}
	return 0
}

func agentText(updates []acp.SessionNotification) string {
	var text strings.Builder
	for _, update := range updates {
		if chunk := update.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
			text.WriteString(chunk.Content.Text.Text)
		}
	}
	return text.String()
}

func configOption(options []acp.SessionConfigOption, id string) *acp.SessionConfigOptionSelect {
	for _, option := range options {
		if option.Select != nil && string(option.Select.Id) == id {
			return option.Select
		}
	}
	return nil
}

// selectKind answers a permission request with the first option of kind.
func selectKind(request acp.RequestPermissionRequest, kind acp.PermissionOptionKind) acp.RequestPermissionResponse {
	for _, option := range request.Options {
		if option.Kind == kind {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(option.OptionId)}
		}
	}
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}
}
