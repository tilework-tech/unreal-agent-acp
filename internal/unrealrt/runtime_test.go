package unrealrt_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"

	"github.com/tilework-tech/unreal-agent-acp/internal/fakemodel"
	"github.com/tilework-tech/unreal-agent-acp/internal/unrealrt"
)

type recorder struct {
	mu    sync.Mutex
	items map[session.ID][]sessionstore.Item
}

func (r *recorder) observe(id session.ID, item sessionstore.Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[id] = append(r.items[id], item)
}

func (r *recorder) of(id session.ID) []sessionstore.Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sessionstore.Item(nil), r.items[id]...)
}

type fixture struct {
	runtime  *unrealrt.Runtime
	model    *fakemodel.Server
	recorder *recorder
	store    string
	cwd      string
}

func newFixture(t *testing.T, handler func(fakemodel.Request) fakemodel.Reply, overrides ...func(map[string]string)) *fixture {
	t.Helper()
	model := fakemodel.Start(t, handler)
	env := model.Env()
	for _, override := range overrides {
		override(env)
	}
	rec := &recorder{items: map[session.ID][]sessionstore.Item{}}
	store := t.TempDir()
	runtime, err := unrealrt.New(unrealrt.Config{
		SessionDirectory: store,
		Getenv:           func(name string) string { return env[name] },
		Observer:         rec.observe,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	return &fixture{runtime: runtime, model: model, recorder: rec, store: store, cwd: t.TempDir()}
}

func (f *fixture) newSession(t *testing.T) *unrealrt.Session {
	t.Helper()
	s, err := f.runtime.NewSession(t.Context(), unrealrt.Options{Cwd: f.cwd})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func prompt(t *testing.T, s *unrealrt.Session, text string) unrealrt.TurnResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	result, err := s.Prompt(ctx, text)
	if err != nil {
		t.Fatalf("Prompt(%q): %v", text, err)
	}
	return result
}

type outcome struct {
	result unrealrt.TurnResult
	err    error
}

func promptAsync(ctx context.Context, s *unrealrt.Session, text string) <-chan outcome {
	done := make(chan outcome, 1)
	go func() {
		result, err := s.Prompt(ctx, text)
		done <- outcome{result, err}
	}()
	return done
}

func await[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// startSleeper is a command that records its PID in cwd/pid and sleeps.
const startSleeper = "echo $$ > pid && exec sleep 30"

func sleeperPID(t *testing.T, cwd string) int {
	t.Helper()
	var pid int
	waitFor(t, "command start", func() bool {
		encoded, err := os.ReadFile(filepath.Join(cwd, "pid"))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(encoded)))
		return err == nil
	})
	return pid
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	waitFor(t, "process exit", func() bool {
		return syscall.Kill(pid, 0) != nil
	})
}

func assistantTexts(items []sessionstore.Item) []string {
	var texts []string
	for _, item := range items {
		response, ok := item.Data.(sessionstore.ModelResponse)
		if !ok {
			continue
		}
		for _, output := range response.Response.Output {
			if message, ok := output.Data.(llm.Message); ok {
				texts = append(texts, message.Text)
			}
		}
	}
	return texts
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPromptRunsUntilIdleAndStreamsItemsToObserver(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(fakemodel.Text("hello there")))
	s := f.newSession(t)

	result := prompt(t, s, "say hi")

	if result.Stop != unrealrt.StopEndTurn {
		t.Fatalf("stop = %q", result.Stop)
	}
	if got := assistantTexts(f.recorder.of(s.ID())); !slices.Equal(got, []string{"hello there"}) {
		t.Fatalf("observed assistant texts = %q", got)
	}
	requests := f.model.Requests()
	if len(requests) != 1 || !slices.Contains(requests[0].UserTexts(), "say hi") || requests[0].Model() != "fake-model" {
		t.Fatalf("model requests = %+v", requests)
	}
}

func TestBashToolRunsInSessionCwdAndResultReachesModel(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": "pwd && echo marker-output"}),
		fakemodel.Text("ran it"),
	))
	s := f.newSession(t)

	result := prompt(t, s, "run it")

	if result.Stop != unrealrt.StopEndTurn {
		t.Fatalf("stop = %q", result.Stop)
	}
	requests := f.model.Requests()
	if len(requests) != 2 {
		t.Fatalf("model saw %d requests, want 2", len(requests))
	}
	output := requests[1].ToolOutputs()["call-1"]
	resolvedCwd, _ := filepath.EvalSymlinks(f.cwd)
	if !strings.Contains(output, "marker-output") || !(strings.Contains(output, f.cwd) || strings.Contains(output, resolvedCwd)) {
		t.Fatalf("tool output = %q, want marker and cwd %s", output, f.cwd)
	}
}

func TestStopReasonReflectsIncompleteResponses(t *testing.T) {
	for incomplete, want := range map[string]unrealrt.StopReason{
		"max_output_tokens": unrealrt.StopMaxTokens,
		"content_filter":    unrealrt.StopRefused,
	} {
		t.Run(incomplete, func(t *testing.T) {
			reply := fakemodel.Text("partial")
			reply.Incomplete = incomplete
			f := newFixture(t, fakemodel.Sequence(reply))
			if result := prompt(t, f.newSession(t), "go"); result.Stop != want {
				t.Fatalf("stop = %q, want %q", result.Stop, want)
			}
		})
	}
}

func TestPromptReportsUsageSummedAcrossTheTurn(t *testing.T) {
	first := fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": "true"})
	first.InputTokens, first.OutputTokens = 100, 10
	second := fakemodel.Text("done")
	second.InputTokens, second.OutputTokens = 150, 20
	f := newFixture(t, fakemodel.Sequence(first, second))

	result := prompt(t, f.newSession(t), "go")

	if result.Usage.InputTokens != 250 || result.Usage.OutputTokens != 30 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestCancelDuringModelCallStopsPromptAsCancelled(t *testing.T) {
	requested := make(chan struct{}, 1)
	f := newFixture(t, func(request fakemodel.Request) fakemodel.Reply {
		requested <- struct{}{}
		<-request.Context().Done()
		return fakemodel.Reply{}
	})
	s := f.newSession(t)
	done := promptAsync(t.Context(), s, "think forever")

	await(t, "model call", requested)
	s.Cancel()

	got := await(t, "Prompt to return after Cancel", done)
	if got.err != nil || got.result.Stop != unrealrt.StopCancelled {
		t.Fatalf("Prompt = %+v, %v", got.result, got.err)
	}
}

func TestCancelDuringRunningCommandKillsItAndStopsPrompt(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": startSleeper}),
	))
	s := f.newSession(t)
	done := promptAsync(t.Context(), s, "sleep")
	pid := sleeperPID(t, f.cwd)

	s.Cancel()

	got := await(t, "Prompt to return after Cancel", done)
	if got.err != nil || got.result.Stop != unrealrt.StopCancelled {
		t.Fatalf("Prompt = %+v, %v", got.result, got.err)
	}
	assertProcessGone(t, pid)
}

func TestPromptAfterCancelledToolCallReportsTheCancellationToTheModel(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": startSleeper}),
		fakemodel.Text("understood"),
	))
	s := f.newSession(t)
	done := promptAsync(t.Context(), s, "sleep")
	sleeperPID(t, f.cwd)
	s.Cancel()
	await(t, "cancelled prompt", done)

	if result := prompt(t, s, "never mind"); result.Stop != unrealrt.StopEndTurn {
		t.Fatalf("stop = %q", result.Stop)
	}
	requests := f.model.Requests()
	last := requests[len(requests)-1]
	if output, ok := last.ToolOutputs()["call-1"]; !ok || !strings.Contains(output, "cancel") {
		t.Fatalf("model did not see the cancelled call's result: outputs=%v", last.ToolOutputs())
	}
	if !slices.Contains(last.UserTexts(), "never mind") {
		t.Fatalf("model did not see the new prompt: %q", last.UserTexts())
	}
}

func TestCancelingPromptContextStopsTheTurn(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": startSleeper}),
		fakemodel.Text("after"),
	))
	s := f.newSession(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := promptAsync(ctx, s, "sleep")
	pid := sleeperPID(t, f.cwd)

	cancel()

	if got := await(t, "Prompt to return after context cancel", done); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("Prompt = %+v, %v; want context.Canceled", got.result, got.err)
	}
	assertProcessGone(t, pid)
	if result := prompt(t, s, "again"); result.Stop != unrealrt.StopEndTurn {
		t.Fatalf("stop after context cancel = %q", result.Stop)
	}
}

func TestCancelWithoutActivePromptIsHarmless(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(fakemodel.Text("still works")))
	s := f.newSession(t)
	s.Cancel()
	s.Cancel()
	if result := prompt(t, s, "hello"); result.Stop != unrealrt.StopEndTurn {
		t.Fatalf("stop after idle cancel = %q", result.Stop)
	}
}

func TestSecondPromptCarriesConversationHistory(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(fakemodel.Text("first answer"), fakemodel.Text("second answer")))
	s := f.newSession(t)
	prompt(t, s, "first question")
	prompt(t, s, "second question")

	requests := f.model.Requests()
	last := requests[len(requests)-1].UserTexts()
	if !slices.Contains(last, "first question") || !slices.Contains(last, "second question") {
		t.Fatalf("second request user texts = %q", last)
	}
}

func TestConcurrentPromptIsRejected(t *testing.T) {
	release := make(chan struct{})
	requested := make(chan struct{}, 4)
	f := newFixture(t, func(request fakemodel.Request) fakemodel.Reply {
		requested <- struct{}{}
		select {
		case <-release:
		case <-request.Context().Done():
		}
		return fakemodel.Text("ok")
	})
	s := f.newSession(t)
	first := promptAsync(t.Context(), s, "first")
	await(t, "first model call", requested)

	if _, err := s.Prompt(t.Context(), "second"); !errors.Is(err, unrealrt.ErrPromptInProgress) {
		t.Fatalf("concurrent Prompt err = %v", err)
	}
	close(release)
	if got := await(t, "first prompt", first); got.err != nil {
		t.Fatal(got.err)
	}
	if result := prompt(t, s, "third"); result.Stop != unrealrt.StopEndTurn {
		t.Fatalf("prompt after the first finished = %q", result.Stop)
	}
}

func TestResumeSessionDoesNotCallModelAndContinuesHistory(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(fakemodel.Text("one"), fakemodel.Text("two")))
	original := f.newSession(t)
	prompt(t, original, "remember the word banana")
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	before := len(f.model.Requests())

	resumed, err := f.runtime.ResumeSession(t.Context(), original.ID(), unrealrt.Options{Cwd: f.cwd})
	if err != nil {
		t.Fatal(err)
	}
	if after := len(f.model.Requests()); after != before {
		t.Fatalf("resume called the model (%d -> %d requests)", before, after)
	}
	prompt(t, resumed, "what was the word?")
	requests := f.model.Requests()
	if texts := requests[len(requests)-1].UserTexts(); !slices.Contains(texts, "remember the word banana") {
		t.Fatalf("resumed prompt lost history: %q", texts)
	}
}

func TestResumeUnknownSessionFails(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence())
	if _, err := f.runtime.ResumeSession(t.Context(), "does-not-exist", unrealrt.Options{Cwd: f.cwd}); !errors.Is(err, unrealrt.ErrSessionNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestReplayYieldsTheSameItemsTheObserverSawLive(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": "echo hi"}),
		fakemodel.Text("done"),
	))
	s := f.newSession(t)
	prompt(t, s, "go")

	var replayed []sessionstore.Item
	if err := s.Replay(t.Context(), func(item sessionstore.Item) error {
		replayed = append(replayed, item)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	live := f.recorder.of(s.ID())
	if len(replayed) != len(live) || len(live) == 0 {
		t.Fatalf("replayed %d items, observed %d live", len(replayed), len(live))
	}
	for index := range live {
		if replayed[index].Sequence != live[index].Sequence || replayed[index].Kind != live[index].Kind {
			t.Fatalf("item %d: replayed %v/%s, live %v/%s", index,
				replayed[index].Sequence, replayed[index].Kind, live[index].Sequence, live[index].Kind)
		}
	}
}

func TestListSessionsReportsCwdTitleAndActivity(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(fakemodel.Text("ok"), fakemodel.Text("ok")))
	s := f.newSession(t)
	prompt(t, s, "Fix the flaky build\nand more detail on a second line")
	prompt(t, s, "a later prompt does not retitle the session")
	otherCwd := t.TempDir()
	other, err := f.runtime.NewSession(t.Context(), unrealrt.Options{Cwd: otherCwd})
	if err != nil {
		t.Fatal(err)
	}

	sessions, err := f.runtime.ListSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[session.ID]unrealrt.SessionInfo{}
	for _, info := range sessions {
		byID[info.ID] = info
	}
	// Cwd is reported exactly as the client supplied it, without resolving symlinks.
	got, ok := byID[s.ID()]
	if !ok || got.Cwd != f.cwd || got.Title != "Fix the flaky build" || got.UpdatedAt.IsZero() {
		t.Fatalf("listed session = %+v (ok=%v)", got, ok)
	}
	if byID[other.ID()].Cwd != otherCwd {
		t.Fatalf("other session = %+v", byID[other.ID()])
	}
}

func TestSessionSurvivesRuntimeRestart(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(fakemodel.Text("ok"), fakemodel.Text("still here")))
	s := f.newSession(t)
	s.SetModel("other-model")
	s.SetReasoningEffort(llm.ReasoningEffortMedium)
	prompt(t, s, "titled prompt")
	var before int
	s.Replay(t.Context(), func(sessionstore.Item) error { before++; return nil })
	if err := f.runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.runtime.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	env := f.model.Env()
	restarted, err := unrealrt.New(unrealrt.Config{SessionDirectory: f.store, Getenv: func(name string) string { return env[name] }})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	resumed, err := restarted.ResumeSession(t.Context(), s.ID(), unrealrt.Options{Cwd: f.cwd})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Model() != "other-model" || resumed.ReasoningEffort() != llm.ReasoningEffortMedium ||
		resumed.Title() != "titled prompt" || resumed.Cwd() != f.cwd {
		t.Fatalf("resumed model=%q effort=%q title=%q cwd=%q", resumed.Model(), resumed.ReasoningEffort(), resumed.Title(), resumed.Cwd())
	}
	var replayed int
	resumed.Replay(t.Context(), func(sessionstore.Item) error { replayed++; return nil })
	if replayed != before {
		t.Fatalf("replay after restart yielded %d items, want %d", replayed, before)
	}
	prompt(t, resumed, "continue")
	requests := f.model.Requests()
	if texts := requests[len(requests)-1].UserTexts(); !slices.Contains(texts, "titled prompt") {
		t.Fatalf("history lost across restart: %q", texts)
	}
}

func TestRuntimeCloseStopsPromptsInEverySession(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": startSleeper}),
		fakemodel.ToolCall("call-2", "Bash", map[string]any{"command": startSleeper}),
	))
	firstCwd, secondCwd := t.TempDir(), t.TempDir()
	first, err := f.runtime.NewSession(t.Context(), unrealrt.Options{Cwd: firstCwd})
	if err != nil {
		t.Fatal(err)
	}
	firstDone := promptAsync(t.Context(), first, "sleep")
	firstPID := sleeperPID(t, firstCwd)
	second, err := f.runtime.NewSession(t.Context(), unrealrt.Options{Cwd: secondCwd})
	if err != nil {
		t.Fatal(err)
	}
	secondDone := promptAsync(t.Context(), second, "sleep")
	secondPID := sleeperPID(t, secondCwd)

	if err := f.runtime.Close(); err != nil {
		t.Fatal(err)
	}
	await(t, "first prompt", firstDone)
	await(t, "second prompt", secondDone)
	assertProcessGone(t, firstPID)
	assertProcessGone(t, secondPID)
}

func TestWrapOperationsSeesEveryOperationOfItsSession(t *testing.T) {
	model := fakemodel.Start(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": "true"}),
		fakemodel.Text("done"),
	))
	env := model.Env()
	var mu sync.Mutex
	seen := map[session.ID][]operation.Type{}
	runtime, err := unrealrt.New(unrealrt.Config{
		SessionDirectory: t.TempDir(),
		Getenv:           func(name string) string { return env[name] },
		WrapOperations: func(id session.ID, inner operation.Manager) operation.Manager {
			return recordingManager{Manager: inner, record: func(value operation.Operation) {
				mu.Lock()
				defer mu.Unlock()
				seen[id] = append(seen[id], value.Type)
			}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	s, err := runtime.NewSession(t.Context(), unrealrt.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	prompt(t, s, "go")

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen[s.ID()], []operation.Type{operation.TypeShell}) {
		t.Fatalf("wrapped manager saw %v", seen)
	}
}

type recordingManager struct {
	operation.Manager
	record func(operation.Operation)
}

func (m recordingManager) Add(value operation.Operation) error {
	m.record(value)
	return m.Manager.Add(value)
}

func TestModelIsOfferedBashAndViewImage(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(fakemodel.Text("ok")))
	prompt(t, f.newSession(t), "hi")
	names := f.model.Requests()[0].ToolNames()
	slices.Sort(names)
	if !slices.Equal(names, []string{"Bash", "ViewImage"}) {
		t.Fatalf("tools offered = %q", names)
	}
}

func TestModelAndReasoningEffortApplyToNextPrompt(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(fakemodel.Text("a"), fakemodel.Text("b")))
	s := f.newSession(t)
	prompt(t, s, "first")
	s.SetModel("bigger-model")
	s.SetReasoningEffort(llm.ReasoningEffortLow)
	prompt(t, s, "second")

	requests := f.model.Requests()
	last := requests[len(requests)-1]
	if requests[0].Model() != "fake-model" || last.Model() != "bigger-model" || last.ReasoningEffort() != "low" {
		t.Fatalf("models %q -> %q, effort %q", requests[0].Model(), last.Model(), last.ReasoningEffort())
	}
}

func TestNewSessionRequiresAbsoluteExistingCwd(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence())
	for _, cwd := range []string{"relative/path", filepath.Join(t.TempDir(), "missing")} {
		if _, err := f.runtime.NewSession(t.Context(), unrealrt.Options{Cwd: cwd}); !errors.Is(err, unrealrt.ErrInvalidCwd) {
			t.Errorf("NewSession(cwd %q) err = %v, want ErrInvalidCwd", cwd, err)
		}
	}
}

func TestMissingAPIKeyIsAuthRequired(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(), func(env map[string]string) {
		delete(env, "UNREAL_HARNESS_LLM_API_KEY")
	})
	if _, err := f.runtime.NewSession(t.Context(), unrealrt.Options{Cwd: f.cwd}); !errors.Is(err, unrealrt.ErrAuthRequired) {
		t.Fatalf("err = %v", err)
	}
}

func TestModelFailureFailsOnlyThatPrompt(t *testing.T) {
	calls := 0
	var mu sync.Mutex
	f := newFixture(t, func(request fakemodel.Request) fakemodel.Reply {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return fakemodel.Reply{Items: []map[string]any{{"type": "unsupported_item_type"}}}
		}
		return fakemodel.Text("recovered")
	})
	s := f.newSession(t)
	if got := await(t, "failing prompt", promptAsync(t.Context(), s, "first")); got.err == nil {
		t.Fatal("Prompt succeeded despite an undecodable model response")
	}
	if result := prompt(t, s, "second"); result.Stop != unrealrt.StopEndTurn {
		t.Fatalf("stop after failure = %q", result.Stop)
	}
}

func TestCloseStopsRunningPromptAndRejectsNewOnes(t *testing.T) {
	f := newFixture(t, fakemodel.Sequence(
		fakemodel.ToolCall("call-1", "Bash", map[string]any{"command": startSleeper}),
	))
	s := f.newSession(t)
	done := promptAsync(t.Context(), s, "sleep")
	pid := sleeperPID(t, f.cwd)

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	await(t, "Close to stop the running prompt", done)
	assertProcessGone(t, pid)
	if _, err := s.Prompt(t.Context(), "again"); !errors.Is(err, unrealrt.ErrClosed) {
		t.Fatalf("Prompt after Close err = %v", err)
	}
}
