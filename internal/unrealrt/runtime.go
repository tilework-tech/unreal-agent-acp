// VENDORED from github.com/unreallabsai/unreal-agent@55747f5 (MIT; see third_party/unreal-agent/LICENSE)
//   cmd/internal/agentrunner/run.go (Run, resolveSessionDirectory,
//   resolveMaxAttempts, defaultSystemPrompt)
// Modified: the one-shot Run is split into a long-lived Runtime and per-session
// Session with Prompt/Cancel/Replay/Close. Request parsing, flags, JSONL output,
// log files, max_output_tokens and loadDotEnv are dropped. One store observer
// per Runtime instead of one per run. Model settings are loaded per prompt
// for the session's current model. Merge-resolve on every upstream bump; see
// VENDORED.md. Delete once upstream exposes a public runtime package.

// Package unrealrt runs long-lived unreal-agent sessions in-process.
//
// Each Prompt rebuilds a coordinator from the session store, submits the user
// input followed by a stop-when-idle control, and runs the event loop until the
// agent is idle. Cancel submits a hard stop. Nothing here knows about ACP.
package unrealrt

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/coordinator"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/settings"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/bash"
	"github.com/unreallabsai/unreal-agent/harness/tool/viewimage"
)

const (
	defaultProvider           = "openai"
	llmBaseURLEnvironment     = "UNREAL_HARNESS_LLM_BASE_URL"
	llmModelEnvironment       = "UNREAL_HARNESS_LLM_MODEL"
	llmProviderEnvironment    = "UNREAL_HARNESS_LLM_PROVIDER"
	llmMaxAttemptsEnvironment = "UNREAL_HARNESS_LLM_MAX_ATTEMPTS"

	defaultToolHeartbeatInterval = 10 * time.Minute
	maxTitleLength               = 80
)

const defaultSystemPrompt = `You are an AI agent running inside an isolated sandbox container.

## Guidelines
- Save output files to the workspace root.
- For large datasets, inspect a sample first before processing everything.
`

var (
	ErrAuthRequired     = errors.New("unrealrt: LLM credentials are not configured")
	ErrSessionNotFound  = errors.New("unrealrt: session not found")
	ErrPromptInProgress = errors.New("unrealrt: a prompt is already running for this session")
	ErrClosed           = errors.New("unrealrt: closed")
	ErrInvalidCwd       = errors.New("unrealrt: invalid cwd")
)

type Config struct {
	// SessionDirectory defaults to $XDG_STATE_HOME/unreal-agent/sessions.
	SessionDirectory string
	// Getenv defaults to os.Getenv.
	Getenv func(string) string
	// Providers defaults to DefaultProviders.
	Providers []Provider
	// Observer receives every item appended to any session, synchronously and
	// in order, from the goroutine running that session's Prompt.
	Observer func(session.ID, sessionstore.Item)
	// WrapOperations, when set, wraps each prompt's operation manager.
	WrapOperations func(session.ID, operation.Manager) operation.Manager
	// SystemPrompt defaults to the unreal-agent-runner system prompt.
	SystemPrompt string
	// ToolHeartbeatInterval defaults to 10 minutes; negative disables heartbeats.
	ToolHeartbeatInterval time.Duration
}

type Options struct {
	// Cwd must be an absolute path to an existing directory. On resume, empty
	// keeps the cwd the session was created with.
	Cwd string
	// Model defaults to UNREAL_HARNESS_LLM_MODEL or the provider default.
	Model string
	// ReasoningEffort defaults to high.
	ReasoningEffort llm.ReasoningEffort
}

type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopCancelled StopReason = "cancelled"
	StopMaxTokens StopReason = "max_tokens"
	StopRefused   StopReason = "refused"
)

type TurnResult struct {
	Stop StopReason
	// Usage is summed over every model response in the turn.
	Usage llm.Usage
}

type SessionInfo struct {
	ID        session.ID
	Cwd       string
	Title     string
	UpdatedAt time.Time
}

type Runtime struct {
	config       Config
	getenv       func(string) string
	store        *localfile.Store
	directory    string
	observerID   sessionstore.ObserverID
	provider     Provider
	baseURL      string
	defaultModel string
	maxAttempts  int
	heartbeat    time.Duration

	clientMu sync.Mutex
	client   Client

	mu       sync.Mutex
	sessions map[session.ID]*Session
	closed   bool
}

func New(config Config) (*Runtime, error) {
	getenv := config.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	providers := config.Providers
	if providers == nil {
		providers = DefaultProviders()
	}
	directory, err := resolveSessionDirectory(config.SessionDirectory, getenv)
	if err != nil {
		return nil, fmt.Errorf("resolve session directory: %w", err)
	}
	store, err := localfile.New(directory)
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	maxAttempts, err := resolveMaxAttempts(getenv)
	if err != nil {
		return nil, err
	}
	providerName := strings.TrimSpace(getenv(llmProviderEnvironment))
	if providerName == "" {
		providerName = defaultProvider
	}
	selected, err := findProvider(providers, providerName)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimSpace(getenv(llmBaseURLEnvironment))
	if baseURL == "" {
		baseURL = selected.BaseURL
	}
	model := strings.TrimSpace(getenv(llmModelEnvironment))
	if model == "" {
		model = selected.DefaultModel
	}
	heartbeat := config.ToolHeartbeatInterval
	switch {
	case heartbeat == 0:
		heartbeat = defaultToolHeartbeatInterval
	case heartbeat < 0:
		heartbeat = 0
	}
	runtime := &Runtime{
		config:       config,
		getenv:       getenv,
		store:        store,
		directory:    filepath.Clean(directory),
		provider:     selected,
		baseURL:      baseURL,
		defaultModel: model,
		maxAttempts:  maxAttempts,
		heartbeat:    heartbeat,
		sessions:     make(map[session.ID]*Session),
	}
	// The localfile store's observer list is not synchronized, so register
	// exactly once, before any session can append.
	runtime.observerID = store.AddObserver(runtime.observe)
	return runtime, nil
}

func (runtime *Runtime) observe(id session.ID, item sessionstore.Item) {
	runtime.mu.Lock()
	current := runtime.sessions[id]
	runtime.mu.Unlock()
	if current != nil {
		current.observe(item)
	}
	if runtime.config.Observer != nil {
		runtime.config.Observer(id, item)
	}
}

// Close stops every running prompt and releases the LLM client. It is idempotent.
func (runtime *Runtime) Close() error {
	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		return nil
	}
	runtime.closed = true
	sessions := make([]*Session, 0, len(runtime.sessions))
	for _, current := range runtime.sessions {
		sessions = append(sessions, current)
	}
	runtime.mu.Unlock()

	var closeErr error
	for _, current := range sessions {
		closeErr = errors.Join(closeErr, current.Close())
	}
	runtime.store.RemoveObserver(runtime.observerID)
	runtime.clientMu.Lock()
	defer runtime.clientMu.Unlock()
	if runtime.client != nil {
		closeErr = errors.Join(closeErr, runtime.client.Close())
		runtime.client = nil
	}
	return closeErr
}

func (runtime *Runtime) llmClient() (Client, error) {
	runtime.clientMu.Lock()
	defer runtime.clientMu.Unlock()
	if runtime.client != nil {
		return runtime.client, nil
	}
	apiKey, err := runtime.provider.APIKey(runtime.getenv)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAuthRequired, err)
	}
	client, err := runtime.provider.NewClient(apiKey, runtime.baseURL, runtime.maxAttempts, runtime.getenv)
	if err != nil {
		return nil, fmt.Errorf("create %s client: %w", runtime.provider.Name, err)
	}
	runtime.client = client
	return client, nil
}

func (runtime *Runtime) checkOpen() error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closed {
		return ErrClosed
	}
	return nil
}

func (runtime *Runtime) NewSession(ctx context.Context, options Options) (*Session, error) {
	if err := runtime.checkOpen(); err != nil {
		return nil, err
	}
	cwd, err := validateCwd(options.Cwd)
	if err != nil {
		return nil, err
	}
	if _, err := runtime.llmClient(); err != nil {
		return nil, err
	}
	effort := options.ReasoningEffort
	if effort == "" {
		effort = llm.ReasoningEffortHigh
	}
	if !effort.Valid() {
		return nil, fmt.Errorf("unsupported reasoning effort %q", effort)
	}
	model := strings.TrimSpace(options.Model)
	if model == "" {
		model = runtime.defaultModel
	}
	if model == "" {
		return nil, fmt.Errorf("model must be set in the session options or %s", llmModelEnvironment)
	}

	id := session.ID(uuid.New().String())
	snapshot, err := runtime.store.Create(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("create session %q: %w", id, err)
	}
	meta := metadata{Cwd: cwd, Model: model, ReasoningEffort: effort, CreatedAt: snapshot.Session.CreatedAt}
	if err := runtime.saveMetadata(id, meta); err != nil {
		return nil, err
	}
	return runtime.track(id, meta)
}

// ResumeSession reopens a persisted session without running it: the model is
// not called until the next Prompt, even if the last input was never answered.
func (runtime *Runtime) ResumeSession(ctx context.Context, id session.ID, options Options) (*Session, error) {
	if err := runtime.checkOpen(); err != nil {
		return nil, err
	}
	if _, err := runtime.llmClient(); err != nil {
		return nil, err
	}
	snapshot, err := runtime.store.Inspect(ctx, id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("open session %q: %w", id, err)
	}
	// Two coordinators must never append to the same session file.
	runtime.closeTracked(id)
	meta, err := runtime.loadMetadata(id)
	if errors.Is(err, fs.ErrNotExist) {
		meta = metadata{CreatedAt: snapshot.Session.CreatedAt}
	} else if err != nil {
		return nil, err
	}
	if options.Cwd != "" || meta.Cwd == "" {
		if meta.Cwd, err = validateCwd(options.Cwd); err != nil {
			return nil, err
		}
	}
	if model := strings.TrimSpace(options.Model); model != "" {
		meta.Model = model
	}
	if meta.Model == "" {
		meta.Model = runtime.defaultModel
	}
	if options.ReasoningEffort != "" {
		meta.ReasoningEffort = options.ReasoningEffort
	}
	if meta.ReasoningEffort == "" {
		meta.ReasoningEffort = llm.ReasoningEffortHigh
	}
	if err := runtime.saveMetadata(id, meta); err != nil {
		return nil, err
	}
	return runtime.track(id, meta)
}

// ListSessions returns sessions created through this package, most recent first
// as reported by the store. Sessions without metadata (for example, created by
// unreal-agent-runner) are skipped because they have no known cwd.
func (runtime *Runtime) ListSessions(ctx context.Context) ([]SessionInfo, error) {
	stored, err := runtime.store.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	sessions := make([]SessionInfo, 0, len(stored))
	for _, info := range stored {
		meta, err := runtime.loadMetadata(info.ID)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, SessionInfo{ID: info.ID, Cwd: meta.Cwd, Title: meta.Title, UpdatedAt: info.LastUpdatedAt})
	}
	return sessions, nil
}

// track registers a new Session for id, first closing (and waiting for) any
// Session already tracked for it.
func (runtime *Runtime) track(id session.ID, meta metadata) (*Session, error) {
	for {
		runtime.mu.Lock()
		if runtime.closed {
			runtime.mu.Unlock()
			return nil, ErrClosed
		}
		if runtime.sessions[id] == nil {
			current := &Session{runtime: runtime, id: id, meta: meta}
			runtime.sessions[id] = current
			runtime.mu.Unlock()
			return current, nil
		}
		runtime.mu.Unlock()
		runtime.closeTracked(id)
	}
}

func (runtime *Runtime) closeTracked(id session.ID) {
	runtime.mu.Lock()
	previous := runtime.sessions[id]
	runtime.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
}

func (runtime *Runtime) untrack(current *Session) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.sessions[current.id] == current {
		delete(runtime.sessions, current.id)
	}
}

type Session struct {
	runtime *Runtime
	id      session.ID

	// promptMu is held for the whole of Prompt; Close waits on it.
	promptMu sync.Mutex

	mu      sync.Mutex
	meta    metadata
	turn    *turn
	closed  bool
	cancels uint64
}

// turn is the state of the running Prompt.
type turn struct {
	inbox         *inbox.Inbox
	cancelled     bool
	stopSubmitted bool
	lastStop      llm.StopReason
	failure       *llm.Failure
	usage         llm.Usage
}

func (current *Session) ID() session.ID { return current.id }

func (current *Session) Cwd() string {
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.meta.Cwd
}

func (current *Session) Title() string {
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.meta.Title
}

func (current *Session) Model() string {
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.meta.Model
}

func (current *Session) ReasoningEffort() llm.ReasoningEffort {
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.meta.ReasoningEffort
}

// SetModel takes effect on the next Prompt.
func (current *Session) SetModel(model string) error {
	return current.updateMetadata(func(meta *metadata) error {
		model = strings.TrimSpace(model)
		if model == "" {
			return errors.New("model must not be empty")
		}
		meta.Model = model
		return nil
	})
}

// SetReasoningEffort takes effect on the next Prompt.
func (current *Session) SetReasoningEffort(effort llm.ReasoningEffort) error {
	return current.updateMetadata(func(meta *metadata) error {
		if !effort.Valid() {
			return fmt.Errorf("unsupported reasoning effort %q", effort)
		}
		meta.ReasoningEffort = effort
		return nil
	})
}

func (current *Session) metadata() metadata {
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.meta
}

func (current *Session) updateMetadata(update func(*metadata) error) error {
	current.mu.Lock()
	defer current.mu.Unlock()
	next := current.meta
	if err := update(&next); err != nil {
		return err
	}
	if err := current.runtime.saveMetadata(current.id, next); err != nil {
		return err
	}
	current.meta = next
	return nil
}

func (current *Session) observe(item sessionstore.Item) {
	response, ok := item.Data.(sessionstore.ModelResponse)
	if !ok {
		return
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.turn == nil {
		return
	}
	usage := response.Response.Usage
	current.turn.usage.InputTokens += usage.InputTokens
	current.turn.usage.CachedInputTokens += usage.CachedInputTokens
	current.turn.usage.CacheWriteInputTokens += usage.CacheWriteInputTokens
	current.turn.usage.OutputTokens += usage.OutputTokens
	current.turn.usage.ReasoningTokens += usage.ReasoningTokens
	current.turn.lastStop = response.Response.Stop
	current.turn.failure = response.Response.Failure
}

// Arrival marks when a prompt request reached the host, relative to Cancel calls.
type Arrival uint64

// Arrived returns the current Arrival. A prompt started with PromptFrom using
// it begins cancelled if Cancel is called before the prompt starts, so a
// cancel the client sent after its prompt is never lost to scheduling.
func (current *Session) Arrived() Arrival {
	current.mu.Lock()
	defer current.mu.Unlock()
	return Arrival(current.cancels)
}

// Cancel hard-stops the running Prompt: the model call is interrupted and
// running operations are canceled, then Prompt returns StopCancelled. It is a
// no-op when no Prompt is running.
func (current *Session) Cancel() {
	current.mu.Lock()
	current.cancels++
	active := current.turn
	if active == nil {
		current.mu.Unlock()
		return
	}
	active.cancelled = true
	inputs := active.inbox
	submit := inputs != nil && !active.stopSubmitted
	if submit {
		active.stopSubmitted = true
	}
	current.mu.Unlock()
	if submit {
		// An error means the turn already finished.
		_ = submitControl(context.Background(), inputs, inbox.ControlMessage{Mode: inbox.StopHard, Reason: "canceled by client"})
	}
}

// Close cancels any running Prompt, waits for it to finish, and rejects later prompts.
func (current *Session) Close() error {
	current.mu.Lock()
	current.closed = true
	current.mu.Unlock()
	current.Cancel()
	current.promptMu.Lock()
	defer current.promptMu.Unlock()
	current.runtime.untrack(current)
	return nil
}

// Replay calls visit for every persisted item of the session, in order.
func (current *Session) Replay(ctx context.Context, visit func(sessionstore.Item) error) error {
	after := sessionstore.BeforeFirst
	for {
		page, err := current.runtime.store.Items(ctx, current.id, after, 256)
		if err != nil {
			return fmt.Errorf("replay session %q: %w", current.id, err)
		}
		for _, item := range page.Items {
			if err := visit(item); err != nil {
				return err
			}
		}
		if !page.More {
			return nil
		}
		after = page.NextAfter
	}
}

// Prompt submits text and runs the agent until it is idle or canceled.
//
// Canceling ctx stops the turn the same way Cancel does, then returns ctx's error.
func (current *Session) Prompt(ctx context.Context, text string) (TurnResult, error) {
	return current.PromptFrom(ctx, current.Arrived(), text)
}

// PromptFrom is Prompt for a request that arrived at arrival; see Arrived.
func (current *Session) PromptFrom(ctx context.Context, arrival Arrival, text string) (TurnResult, error) {
	if !current.promptMu.TryLock() {
		return TurnResult{}, ErrPromptInProgress
	}
	defer current.promptMu.Unlock()

	current.mu.Lock()
	if current.closed {
		current.mu.Unlock()
		return TurnResult{}, ErrClosed
	}
	active := &turn{cancelled: current.cancels != uint64(arrival)}
	current.turn = active
	current.mu.Unlock()
	defer func() {
		current.mu.Lock()
		current.turn = nil
		current.mu.Unlock()
	}()
	if err := current.updateMetadata(func(meta *metadata) error {
		if meta.Title == "" {
			meta.Title = titleFrom(text)
		}
		return nil
	}); err != nil {
		return TurnResult{}, err
	}
	meta := current.metadata()
	stopWatching := context.AfterFunc(ctx, current.Cancel)
	defer stopWatching()

	runErr := current.run(active, meta, text)
	if err := ctx.Err(); err != nil {
		return TurnResult{}, err
	}
	if runErr != nil {
		return TurnResult{}, runErr
	}

	current.mu.Lock()
	defer current.mu.Unlock()
	result := TurnResult{Stop: StopEndTurn, Usage: active.usage}
	switch {
	case active.cancelled:
		result.Stop = StopCancelled
	case active.failure != nil:
		return TurnResult{}, fmt.Errorf("model response failed: %s %s", active.failure.Code, active.failure.Message)
	case active.lastStop == llm.StopMaxOutputTokens:
		result.Stop = StopMaxTokens
	case active.lastStop == llm.StopRefused:
		result.Stop = StopRefused
	}
	return result, nil
}

func (current *Session) run(active *turn, meta metadata, text string) error {
	runtime := current.runtime
	client, err := runtime.llmClient()
	if err != nil {
		return err
	}
	// The run context is independent of the caller's: cancellation goes through
	// a hard-stop control so canceled operations are recorded in the session.
	runContext, cancel := context.WithCancel(context.Background())
	defer cancel()

	restored, err := runtime.store.Resume(runContext, current.id)
	if err != nil {
		return fmt.Errorf("resume session %q: %w", current.id, err)
	}
	operationDirectory := filepath.Join(runtime.directory, "operations", string(current.id))
	if err := os.MkdirAll(operationDirectory, 0o700); err != nil {
		return fmt.Errorf("create operation directory: %w", err)
	}
	shell := strings.TrimSpace(runtime.getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	skills, _ := tool.DiscoverSkills(filepath.Join(meta.Cwd, ".harness", "skills"))
	names := []string{tool.BashName, tool.ViewImageName}
	if len(skills) != 0 {
		names = append(names, tool.SkillUseName)
	}
	registry := tool.NewRegistry(tool.StaticTranslators{
		Bash: bash.New(bash.Config{
			Shell:         shell,
			Directory:     meta.Cwd,
			BaseDirectory: operationDirectory,
		}),
		ViewImage: viewimage.New(viewimage.Config{Directory: meta.Cwd}),
	}, names...)
	for _, skill := range skills {
		if _, err := registry.RegisterSkill(skill); err != nil {
			return fmt.Errorf("register skill %q: %w", skill.Path, err)
		}
	}

	selectedModel := llm.Model{ID: meta.Model, ReasoningEffort: meta.ReasoningEffort}
	settingsDirectory, err := configDirectory(runtime.getenv)
	if err != nil {
		return fmt.Errorf("resolve settings directory: %w", err)
	}
	modelSettings, err := settings.Load(filepath.Join(settingsDirectory, "settings.json"))
	if err != nil {
		return fmt.Errorf("load model settings: %w", err)
	}
	selectedModel.CompactionThreshold = modelSettings.Model(runtime.provider.Name, selectedModel.ID).CompactionThreshold

	var operations operation.Manager = operation.NewLocalOperationManager(runContext)
	if runtime.config.WrapOperations != nil {
		operations = runtime.config.WrapOperations(current.id, operations)
	}
	// Wait for every operation to wind down before the prompt returns.
	defer func() {
		cancel()
		for range operations.Updates() {
		}
	}()
	inputs, err := inbox.New(runContext, restored.InputIDs)
	if err != nil {
		return fmt.Errorf("open inbox: %w", err)
	}
	if err := submitControl(runContext, inputs, inbox.ControlMessage{
		Mode: inbox.UpdateSettings,
		Parameters: inbox.Settings{
			Model:               selectedModel.ID,
			CompactionThreshold: new(selectedModel.CompactionThreshold),
			ReasoningEffort:     selectedModel.ReasoningEffort,
		},
	}); err != nil {
		return err
	}
	payload, err := json.Marshal(text)
	if err != nil {
		return fmt.Errorf("encode prompt: %w", err)
	}
	if err := inputs.Submit(runContext, inbox.Input{
		ID: inbox.ID(uuid.New().String()), Kind: inbox.InputExternal, Payload: payload,
	}); err != nil {
		return fmt.Errorf("submit prompt: %w", err)
	}
	if err := submitControl(runContext, inputs, inbox.ControlMessage{Mode: inbox.StopWhenIdle}); err != nil {
		return err
	}

	current.mu.Lock()
	active.inbox = inputs
	stopNow := active.cancelled && !active.stopSubmitted
	if stopNow {
		active.stopSubmitted = true
	}
	current.mu.Unlock()
	if stopNow {
		if err := submitControl(runContext, inputs, inbox.ControlMessage{Mode: inbox.StopHard, Reason: "canceled by client"}); err != nil {
			return err
		}
	}

	builder := contextbuilder.NewBuilder(registry.Skills()...)
	builder.SetModel(selectedModel)
	systemPrompt := runtime.config.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = defaultSystemPrompt
	}
	builder.SetSystemPrompt(systemPrompt)
	for _, definition := range registry.StaticDefinitions() {
		builder.AddTool(definition.Tool)
	}

	if err := coordinator.New(coordinator.Dependencies{
		ToolHeartbeatInterval: runtime.heartbeat,
		SessionID:             current.id,
		Inbox:                 inputs,
		Restored:              restored,
		Sessions:              runtime.store,
		ContextBuilder:        builder,
		LLM:                   client,
		Tools:                 registry,
		Operations:            operations,
	}).Run(runContext); err != nil {
		return fmt.Errorf("run coordinator: %w", err)
	}
	return nil
}

func submitControl(ctx context.Context, inputs *inbox.Inbox, message inbox.ControlMessage) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode %s control: %w", message.Mode, err)
	}
	if err := inputs.Submit(ctx, inbox.Input{
		ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload,
	}); err != nil {
		return fmt.Errorf("submit %s control: %w", message.Mode, err)
	}
	return nil
}

func validateCwd(cwd string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("%w: %q must be an absolute path", ErrInvalidCwd, cwd)
	}
	cwd = filepath.Clean(cwd)
	info, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidCwd, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %q is not a directory", ErrInvalidCwd, cwd)
	}
	return cwd, nil
}

func titleFrom(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	if runes := []rune(line); len(runes) > maxTitleLength {
		line = string(runes[:maxTitleLength-1]) + "…"
	}
	return line
}

func resolveMaxAttempts(getenv func(string) string) (int, error) {
	maxAttempts := responsesapi.DefaultMaxAttempts
	if value := strings.TrimSpace(getenv(llmMaxAttemptsEnvironment)); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("parse %s: %w", llmMaxAttemptsEnvironment, err)
		}
		maxAttempts = parsed
	}
	if maxAttempts <= 0 {
		return 0, errors.New("max attempts must be positive")
	}
	return maxAttempts, nil
}

func resolveSessionDirectory(configured string, getenv func(string) string) (string, error) {
	if configured = strings.TrimSpace(configured); configured != "" {
		return filepath.Abs(configured)
	}
	directory, err := sessionDirectory(getenv)
	if err != nil {
		return "", fmt.Errorf("%w; set a session directory to override", err)
	}
	return directory, nil
}
