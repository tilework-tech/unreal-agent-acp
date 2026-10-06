// Package acpagent implements the ACP v1 agent side on top of unrealrt.
//
// It owns the session table and translates between ACP messages and the
// protocol-independent runtime: requests become unrealrt calls, and session
// items observed during a prompt become session/update notifications, written
// synchronously so every update precedes the prompt's response.
package acpagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"

	"github.com/tilework-tech/unreal-agent-acp/acp/jsonrpc"
	acp "github.com/tilework-tech/unreal-agent-acp/acp/v1"
	"github.com/tilework-tech/unreal-agent-acp/internal/permission"
	"github.com/tilework-tech/unreal-agent-acp/internal/translate"
	"github.com/tilework-tech/unreal-agent-acp/internal/unrealrt"
)

const (
	configModel           = "model"
	configReasoningEffort = "reasoning_effort"
	configMode            = "mode"

	optionAllowOnce   = "allow_once"
	optionAllowAlways = "allow_always"
	optionRejectOnce  = "reject_once"

	codeAuthRequired = -32000
)

var modes = []acp.SessionMode{
	{Id: acp.SessionModeId(permission.Ask), Name: "Ask", Description: ptr("Ask before running shell commands")},
	{Id: acp.SessionModeId(permission.FullAccess), Name: "Full access", Description: ptr("Run shell commands without asking")},
}

var reasoningEfforts = []llm.ReasoningEffort{
	llm.ReasoningEffortLow, llm.ReasoningEffortMedium, llm.ReasoningEffortHigh,
	llm.ReasoningEffortXHigh, llm.ReasoningEffortMax,
}

type Config struct {
	Name    string
	Version string
	// Runtime configures the embedded unreal-agent runtime. Observer and
	// WrapOperations are owned by the agent and must be left unset.
	Runtime unrealrt.Config
	// Models are offered in the model config option alongside the session's current model.
	Models []string
	// Logger defaults to a text logger on stderr. stdout is the ACP transport.
	Logger *slog.Logger
}

type agent struct {
	config  Config
	logger  *slog.Logger
	runtime *unrealrt.Runtime
	conn    *jsonrpc.Conn

	mu       sync.Mutex
	sessions map[session.ID]*agentSession
}

type agentSession struct {
	runtime *unrealrt.Session
	gate    *permission.Gate
	// live translates items as they are appended; only the prompting
	// goroutine touches it.
	live *translate.Session

	mu sync.Mutex
	// toolCalls maps operation IDs to the tool call that submitted them, so
	// permission requests can name the tool call the client already shows.
	toolCalls map[operation.ID]string
}

// Serve runs an ACP agent over r and w until r ends or ctx is done.
func Serve(ctx context.Context, r io.Reader, w io.Writer, config Config) error {
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	a := &agent{config: config, logger: logger, sessions: map[session.ID]*agentSession{}}
	runtimeConfig := config.Runtime
	runtimeConfig.Observer = a.observe
	runtimeConfig.WrapOperations = a.wrapOperations
	runtime, err := unrealrt.New(runtimeConfig)
	if err != nil {
		return err
	}
	a.runtime = runtime
	a.conn = jsonrpc.NewConn(r, w, a)
	serveErr := a.conn.Serve(ctx)
	return errors.Join(serveErr, runtime.Close())
}

func (a *agent) HandleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case acp.AgentMethodInitialize:
		return handle(ctx, params, a.initialize)
	case acp.AgentMethodSessionNew:
		return handle(ctx, params, a.newSession)
	case acp.AgentMethodSessionPrompt:
		return handle(ctx, params, a.prompt)
	case acp.AgentMethodSessionClose:
		return handle(ctx, params, a.closeSession)
	case acp.AgentMethodSessionSetConfigOption:
		return handle(ctx, params, a.setConfigOption)
	case acp.AgentMethodSessionSetMode:
		return handle(ctx, params, a.setMode)
	case acp.AgentMethodSessionLoad:
		return handle(ctx, params, a.loadSession)
	case acp.AgentMethodSessionResume:
		return handle(ctx, params, a.resumeSession)
	case acp.AgentMethodSessionList:
		return handle(ctx, params, a.listSessions)
	default:
		return nil, jsonrpc.ErrMethodNotFound(method)
	}
}

func (a *agent) HandleNotification(_ context.Context, method string, params json.RawMessage) {
	switch method {
	case acp.AgentMethodSessionCancel:
		var notification acp.CancelNotification
		if err := json.Unmarshal(params, &notification); err != nil {
			a.logger.Warn("ignoring malformed session/cancel", "error", err)
			return
		}
		if current, err := a.session(notification.SessionId); err == nil {
			current.runtime.Cancel()
		}
	default:
		a.logger.Debug("ignoring notification", "method", method)
	}
}

func handle[Request, Response any](ctx context.Context, params json.RawMessage, handler func(context.Context, Request) (Response, error)) (any, error) {
	var request Request
	if len(params) != 0 {
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, jsonrpc.ErrInvalidParams(err)
		}
	}
	return handler(ctx, request)
}

func (a *agent) initialize(_ context.Context, request acp.InitializeRequest) (acp.InitializeResponse, error) {
	version := acp.ProtocolVersion(acp.ProtocolVersionNumber)
	if request.ProtocolVersion < version && request.ProtocolVersion > 0 {
		version = request.ProtocolVersion
	}
	return acp.InitializeResponse{
		ProtocolVersion: version,
		AgentInfo:       &acp.Implementation{Name: a.config.Name, Version: a.config.Version},
		AuthMethods:     []acp.AuthMethod{},
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession:        true,
			PromptCapabilities: acp.PromptCapabilities{EmbeddedContext: true},
			SessionCapabilities: acp.SessionCapabilities{
				Close:  &acp.SessionCloseCapabilities{},
				List:   &acp.SessionListCapabilities{},
				Resume: &acp.SessionResumeCapabilities{},
			},
		},
	}, nil
}

func (a *agent) newSession(ctx context.Context, request acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	if len(request.McpServers) != 0 {
		a.logger.Warn("MCP servers are not supported yet; ignoring them", "count", len(request.McpServers))
	}
	created, err := a.runtime.NewSession(ctx, unrealrt.Options{Cwd: request.Cwd})
	if err != nil {
		return acp.NewSessionResponse{}, runtimeError(err)
	}
	current := a.track(created, translate.NewSession())
	return acp.NewSessionResponse{
		SessionId:     acp.SessionId(created.ID()),
		ConfigOptions: a.configOptions(current),
		Modes:         modeState(current),
	}, nil
}

// loadSession streams the session's history as session/update notifications
// before responding.
func (a *agent) loadSession(ctx context.Context, request acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	current, err := a.reopen(ctx, request.SessionId, request.Cwd, len(request.McpServers), true)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}
	return acp.LoadSessionResponse{ConfigOptions: a.configOptions(current), Modes: modeState(current)}, nil
}

func (a *agent) resumeSession(ctx context.Context, request acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	current, err := a.reopen(ctx, request.SessionId, request.Cwd, len(request.McpServers), false)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	return acp.ResumeSessionResponse{ConfigOptions: a.configOptions(current), Modes: modeState(current)}, nil
}

func (a *agent) reopen(ctx context.Context, id acp.SessionId, cwd string, mcpServers int, replay bool) (*agentSession, error) {
	if mcpServers != 0 {
		a.logger.Warn("MCP servers are not supported yet; ignoring them", "count", mcpServers)
	}
	reopened, err := a.runtime.ResumeSession(ctx, session.ID(id), unrealrt.Options{Cwd: cwd})
	if err != nil {
		return nil, runtimeError(err)
	}
	// The translator sees the whole history so later updates refer to known tool calls.
	live := translate.NewSession()
	if err := reopened.Replay(ctx, func(item sessionstore.Item) error {
		for _, update := range live.Translate(item) {
			if replay {
				a.notify(reopened.ID(), update)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return a.track(reopened, live), nil
}

func (a *agent) listSessions(ctx context.Context, request acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	stored, err := a.runtime.ListSessions(ctx)
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}
	slices.SortStableFunc(stored, func(left, right unrealrt.SessionInfo) int {
		return right.UpdatedAt.Compare(left.UpdatedAt)
	})
	sessions := make([]acp.SessionInfo, 0, len(stored))
	for _, info := range stored {
		if request.Cwd != nil && filepath.Clean(*request.Cwd) != info.Cwd {
			continue
		}
		entry := acp.SessionInfo{SessionId: acp.SessionId(info.ID), Cwd: info.Cwd}
		if info.Title != "" {
			entry.Title = ptr(info.Title)
		}
		if !info.UpdatedAt.IsZero() {
			entry.UpdatedAt = ptr(info.UpdatedAt.UTC().Format(time.RFC3339))
		}
		sessions = append(sessions, entry)
	}
	return acp.ListSessionsResponse{Sessions: sessions}, nil
}

func (a *agent) setMode(_ context.Context, request acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	current, err := a.session(request.SessionId)
	if err != nil {
		return acp.SetSessionModeResponse{}, err
	}
	if err := current.gate.SetMode(permission.Mode(request.ModeId)); err != nil {
		return acp.SetSessionModeResponse{}, jsonrpc.ErrInvalidParams(err)
	}
	return acp.SetSessionModeResponse{}, nil
}

func (a *agent) prompt(ctx context.Context, request acp.PromptRequest) (acp.PromptResponse, error) {
	current, err := a.session(request.SessionId)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	text, err := flattenPrompt(request.Prompt)
	if err != nil {
		return acp.PromptResponse{}, jsonrpc.ErrInvalidParams(err)
	}
	result, err := current.runtime.Prompt(ctx, text)
	if err != nil {
		return acp.PromptResponse{}, runtimeError(err)
	}
	return acp.PromptResponse{StopReason: stopReason(result.Stop)}, nil
}

func (a *agent) closeSession(_ context.Context, request acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	current, err := a.session(request.SessionId)
	if err != nil {
		return acp.CloseSessionResponse{}, err
	}
	a.mu.Lock()
	delete(a.sessions, current.runtime.ID())
	a.mu.Unlock()
	if err := current.runtime.Close(); err != nil {
		return acp.CloseSessionResponse{}, err
	}
	return acp.CloseSessionResponse{}, nil
}

func (a *agent) setConfigOption(_ context.Context, request acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	if request.ValueId == nil {
		return acp.SetSessionConfigOptionResponse{}, jsonrpc.ErrInvalidParams(errors.New("only select config options are supported"))
	}
	current, err := a.session(request.ValueId.SessionId)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	value := string(request.ValueId.Value)
	switch string(request.ValueId.ConfigId) {
	case configModel:
		err = current.runtime.SetModel(value)
	case configReasoningEffort:
		err = current.runtime.SetReasoningEffort(llm.ReasoningEffort(value))
	case configMode:
		err = current.gate.SetMode(permission.Mode(value))
	default:
		err = fmt.Errorf("unknown config option %q", request.ValueId.ConfigId)
	}
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, jsonrpc.ErrInvalidParams(err)
	}
	return acp.SetSessionConfigOptionResponse{ConfigOptions: a.configOptions(current)}, nil
}

func (a *agent) track(runtimeSession *unrealrt.Session, live *translate.Session) *agentSession {
	current := &agentSession{runtime: runtimeSession, live: live, toolCalls: map[operation.ID]string{}}
	current.gate = permission.NewGate(runtimeSession.ID(), approver{agent: a, session: current})
	a.mu.Lock()
	a.sessions[runtimeSession.ID()] = current
	a.mu.Unlock()
	return current
}

func (a *agent) session(id acp.SessionId) (*agentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	current, ok := a.sessions[session.ID(id)]
	if !ok {
		return nil, jsonrpc.ErrInvalidParams(fmt.Errorf("unknown session %q", id))
	}
	return current, nil
}

// observe runs on the goroutine executing the session's prompt.
func (a *agent) observe(id session.ID, item sessionstore.Item) {
	a.mu.Lock()
	current := a.sessions[id]
	a.mu.Unlock()
	if current == nil {
		return
	}
	if status, ok := item.Data.(sessionstore.ToolCallStatus); ok {
		current.mu.Lock()
		for _, id := range status.Status.WaitingFor {
			current.toolCalls[id] = status.CallID
		}
		current.mu.Unlock()
	}
	for _, update := range current.live.Translate(item) {
		if update.Kind == translate.UserMessage {
			continue // the client already shows what it sent
		}
		a.notify(id, update)
	}
}

func (a *agent) notify(id session.ID, update translate.Update) {
	notification := acp.SessionNotification{SessionId: acp.SessionId(id), Update: toSessionUpdate(update)}
	if err := a.conn.Notify(context.Background(), acp.ClientMethodSessionUpdate, notification); err != nil {
		a.logger.Warn("send session/update", "session", id, "error", err)
	}
}

// wrapOperations gates each prompt's operations with its session's policy.
func (a *agent) wrapOperations(id session.ID, inner operation.Manager) operation.Manager {
	a.mu.Lock()
	current := a.sessions[id]
	a.mu.Unlock()
	if current == nil {
		return permission.NewGate(id, denyAll{}).Wrap(inner)
	}
	return current.gate.Wrap(inner)
}

// approver asks the client through session/request_permission.
type approver struct {
	agent   *agent
	session *agentSession
}

func (p approver) Approve(ctx context.Context, request permission.Request) (permission.Decision, error) {
	p.session.mu.Lock()
	callID, known := p.session.toolCalls[request.OperationID]
	p.session.mu.Unlock()
	if !known {
		callID = string(request.OperationID)
	}
	kind := acp.ToolKindExecute
	status := acp.ToolCallStatusPending
	var response acp.RequestPermissionResponse
	if err := p.agent.conn.Call(ctx, acp.ClientMethodSessionRequestPermission, acp.RequestPermissionRequest{
		SessionId: acp.SessionId(request.SessionID),
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(callID),
			Title:      ptr(request.Command),
			Kind:       &kind,
			Status:     &status,
			RawInput:   map[string]any{"command": request.Command},
		},
		Options: []acp.PermissionOption{
			{OptionId: optionAllowOnce, Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: optionAllowAlways, Name: "Allow all commands in this session", Kind: acp.PermissionOptionKindAllowAlways},
			{OptionId: optionRejectOnce, Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
	}, &response); err != nil {
		return permission.Cancelled, err
	}
	if response.Outcome.Selected == nil {
		return permission.Cancelled, nil
	}
	switch response.Outcome.Selected.OptionId {
	case optionAllowOnce:
		return permission.AllowOnce, nil
	case optionAllowAlways:
		return permission.AllowAlways, nil
	case optionRejectOnce:
		return permission.Reject, nil
	default:
		return permission.Cancelled, fmt.Errorf("client selected unknown permission option %q", response.Outcome.Selected.OptionId)
	}
}

type denyAll struct{}

func (denyAll) Approve(context.Context, permission.Request) (permission.Decision, error) {
	return permission.Reject, nil
}

func modeState(current *agentSession) *acp.SessionModeState {
	return &acp.SessionModeState{CurrentModeId: acp.SessionModeId(current.gate.Mode()), AvailableModes: modes}
}

func ptr[T any](value T) *T { return &value }

func (a *agent) configOptions(session *agentSession) []acp.SessionConfigOption {
	current := session.runtime
	model := current.Model()
	models := slices.Clone(a.config.Models)
	if !slices.Contains(models, model) {
		models = append([]string{model}, models...)
	}
	modelOptions := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(models))
	for _, name := range models {
		modelOptions = append(modelOptions, acp.SessionConfigSelectOption{Value: acp.SessionConfigValueId(name), Name: name})
	}
	effortOptions := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(reasoningEfforts))
	for _, effort := range reasoningEfforts {
		effortOptions = append(effortOptions, acp.SessionConfigSelectOption{Value: acp.SessionConfigValueId(effort), Name: string(effort)})
	}
	modeOptions := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(modes))
	for _, mode := range modes {
		modeOptions = append(modeOptions, acp.SessionConfigSelectOption{Value: acp.SessionConfigValueId(mode.Id), Name: mode.Name, Description: mode.Description})
	}
	modeCategory := acp.SessionConfigOptionCategoryMode
	modelCategory := acp.SessionConfigOptionCategoryModel
	effortCategory := acp.SessionConfigOptionCategoryThoughtLevel
	return []acp.SessionConfigOption{
		{Select: &acp.SessionConfigOptionSelect{
			Id: configMode, Name: "Permissions", Category: &modeCategory,
			CurrentValue: acp.SessionConfigValueId(session.gate.Mode()),
			Options:      acp.SessionConfigSelectOptions{Ungrouped: &modeOptions},
		}},
		{Select: &acp.SessionConfigOptionSelect{
			Id: configModel, Name: "Model", Category: &modelCategory,
			CurrentValue: acp.SessionConfigValueId(model),
			Options:      acp.SessionConfigSelectOptions{Ungrouped: &modelOptions},
		}},
		{Select: &acp.SessionConfigOptionSelect{
			Id: configReasoningEffort, Name: "Reasoning effort", Category: &effortCategory,
			CurrentValue: acp.SessionConfigValueId(current.ReasoningEffort()),
			Options:      acp.SessionConfigSelectOptions{Ungrouped: &effortOptions},
		}},
	}
}

func runtimeError(err error) error {
	switch {
	case errors.Is(err, unrealrt.ErrAuthRequired):
		return &jsonrpc.Error{Code: codeAuthRequired, Message: "Authentication required: " + err.Error()}
	case errors.Is(err, unrealrt.ErrInvalidCwd), errors.Is(err, unrealrt.ErrSessionNotFound), errors.Is(err, unrealrt.ErrClosed):
		return jsonrpc.ErrInvalidParams(err)
	case errors.Is(err, unrealrt.ErrPromptInProgress):
		return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: err.Error()}
	default:
		return err
	}
}

func stopReason(stop unrealrt.StopReason) acp.StopReason {
	switch stop {
	case unrealrt.StopCancelled:
		return acp.StopReasonCancelled
	case unrealrt.StopMaxTokens:
		return acp.StopReasonMaxTokens
	case unrealrt.StopRefused:
		return acp.StopReasonRefusal
	default:
		return acp.StopReasonEndTurn
	}
}

// flattenPrompt renders ACP content blocks as the single text input unreal-agent accepts.
func flattenPrompt(blocks []acp.ContentBlock) (string, error) {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			parts = append(parts, block.Text.Text)
		case block.ResourceLink != nil:
			parts = append(parts, fmt.Sprintf("[%s](%s)", block.ResourceLink.Name, block.ResourceLink.Uri))
		case block.Resource != nil && block.Resource.Resource.TextResourceContents != nil:
			resource := block.Resource.Resource.TextResourceContents
			parts = append(parts, fmt.Sprintf("<resource uri=%q>\n%s\n</resource>", resource.Uri, resource.Text))
		case block.Resource != nil && block.Resource.Resource.BlobResourceContents != nil:
			parts = append(parts, fmt.Sprintf("<resource uri=%q>(binary content omitted)</resource>", block.Resource.Resource.BlobResourceContents.Uri))
		case block.Image != nil:
			return "", errors.New("image content is not supported")
		case block.Audio != nil:
			return "", errors.New("audio content is not supported")
		default:
			return "", errors.New("unsupported content block")
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

func toSessionUpdate(update translate.Update) acp.SessionUpdate {
	var messageID *acp.MessageId
	if update.MessageID != "" {
		id := acp.MessageId(update.MessageID)
		messageID = &id
	}
	content := acp.ContentBlock{Text: &acp.ContentBlockText{Text: update.Text}}
	switch update.Kind {
	case translate.UserMessage:
		return acp.SessionUpdate{UserMessageChunk: &acp.SessionUpdateUserMessageChunk{Content: content, MessageId: messageID}}
	case translate.AgentMessage:
		return acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: content, MessageId: messageID}}
	case translate.AgentThought:
		return acp.SessionUpdate{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: content, MessageId: messageID}}
	case translate.ToolCallStarted:
		call := update.Tool
		locations := make([]acp.ToolCallLocation, 0, len(call.Locations))
		for _, path := range call.Locations {
			locations = append(locations, acp.ToolCallLocation{Path: path})
		}
		name := call.Name
		return acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
			ToolCallId: acp.ToolCallId(call.CallID),
			Title:      call.Title,
			Name:       &name,
			Kind:       acp.ToolKind(call.Kind),
			Status:     acp.ToolCallStatus(call.Status),
			RawInput:   call.RawInput,
			Locations:  locations,
		}}
	default:
		call := update.Tool
		status := acp.ToolCallStatus(call.Status)
		var output []acp.ToolCallContent
		if call.Output != "" {
			output = []acp.ToolCallContent{{Content: &acp.ToolCallContentContent{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: call.Output}},
			}}}
		}
		return acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
			ToolCallId: acp.ToolCallId(call.CallID),
			Status:     &status,
			Content:    output,
		}}
	}
}
