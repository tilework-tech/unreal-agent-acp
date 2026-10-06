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
	"slices"
	"strings"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"

	"github.com/tilework-tech/unreal-agent-acp/acp/jsonrpc"
	acp "github.com/tilework-tech/unreal-agent-acp/acp/v1"
	"github.com/tilework-tech/unreal-agent-acp/internal/translate"
	"github.com/tilework-tech/unreal-agent-acp/internal/unrealrt"
)

const (
	configModel           = "model"
	configReasoningEffort = "reasoning_effort"

	codeAuthRequired = -32000
)

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
	// live translates items as they are appended; only the prompting
	// goroutine touches it.
	live *translate.Session
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
			PromptCapabilities: acp.PromptCapabilities{EmbeddedContext: true},
			SessionCapabilities: acp.SessionCapabilities{
				Close: &acp.SessionCloseCapabilities{},
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
	a.track(created, translate.NewSession())
	return acp.NewSessionResponse{
		SessionId:     acp.SessionId(created.ID()),
		ConfigOptions: a.configOptions(created),
	}, nil
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
	default:
		err = fmt.Errorf("unknown config option %q", request.ValueId.ConfigId)
	}
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, jsonrpc.ErrInvalidParams(err)
	}
	return acp.SetSessionConfigOptionResponse{ConfigOptions: a.configOptions(current.runtime)}, nil
}

func (a *agent) track(runtimeSession *unrealrt.Session, live *translate.Session) *agentSession {
	current := &agentSession{runtime: runtimeSession, live: live}
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

func (a *agent) configOptions(current *unrealrt.Session) []acp.SessionConfigOption {
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
	modelCategory := acp.SessionConfigOptionCategoryModel
	effortCategory := acp.SessionConfigOptionCategoryThoughtLevel
	return []acp.SessionConfigOption{
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
