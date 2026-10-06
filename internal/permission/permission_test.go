package permission_test

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"

	"github.com/tilework-tech/unreal-agent-acp/internal/permission"
)

// approver answers requests from a channel so tests control timing.
type approver struct {
	mu        sync.Mutex
	requests  []permission.Request
	asked     chan permission.Request
	decisions chan permission.Decision
	canceled  chan permission.Request
}

func newApprover() *approver {
	return &approver{
		asked:     make(chan permission.Request, 8),
		decisions: make(chan permission.Decision, 8),
		canceled:  make(chan permission.Request, 8),
	}
}

func (a *approver) Approve(ctx context.Context, request permission.Request) (permission.Decision, error) {
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.mu.Unlock()
	a.asked <- request
	select {
	case decision := <-a.decisions:
		return decision, nil
	case <-ctx.Done():
		a.canceled <- request
		return permission.Cancelled, ctx.Err()
	}
}

func (a *approver) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests)
}

func await[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func shellOperation(t *testing.T, id operation.ID, command, dir string) operation.Operation {
	t.Helper()
	spec, err := operation.NewShellSpec(operation.ShellInput{Command: command, Shell: "/bin/sh", Directory: dir}, t.TempDir(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{ID: id, Type: spec.Type, Version: spec.Version, Status: operation.StatusReady, State: spec.State, MaxOutputLength: spec.MaxOutputLength}
}

// terminal waits for the operation's terminal update.
func terminal(t *testing.T, manager operation.Manager, id operation.ID) operation.Operation {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case update, open := <-manager.Updates():
			if !open {
				t.Fatal("updates closed before a terminal update")
			}
			if update.ID != id {
				continue
			}
			switch update.Status {
			case operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled:
				return update
			}
		case <-deadline:
			t.Fatalf("no terminal update for %s", id)
		}
	}
}

func newManager(t *testing.T, gate *permission.Gate) operation.Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return gate.Wrap(operation.NewLocalOperationManager(ctx))
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestAskModeRunsShellOnlyAfterApproval(t *testing.T) {
	dir := t.TempDir()
	approvals := newApprover()
	gate := permission.NewGate("session-1", approvals)
	manager := newManager(t, gate)

	added := make(chan error, 1)
	go func() { added <- manager.Add(shellOperation(t, "op-1", "touch ran", dir)) }()
	if err := await(t, "Add to return without waiting for approval", added); err != nil {
		t.Fatal(err)
	}
	request := await(t, "approval request", approvals.asked)
	if request.SessionID != "session-1" || request.OperationID != "op-1" || request.Command != "touch ran" || request.Directory != dir {
		t.Fatalf("request = %+v", request)
	}
	select {
	case update := <-manager.Updates():
		t.Fatalf("operation progressed before approval: %+v", update)
	case <-time.After(50 * time.Millisecond):
	}
	if exists(filepath.Join(dir, "ran")) {
		t.Fatal("command ran before approval")
	}

	approvals.decisions <- permission.AllowOnce
	if done := terminal(t, manager, "op-1"); done.Status != operation.StatusCompleted {
		t.Fatalf("status = %s", done.Status)
	}
	if !exists(filepath.Join(dir, "ran")) {
		t.Fatal("approved command did not run")
	}
}

func TestRejectedShellFailsWithoutRunning(t *testing.T) {
	dir := t.TempDir()
	approvals := newApprover()
	manager := newManager(t, permission.NewGate("s", approvals))

	manager.Add(shellOperation(t, "op-1", "touch ran", dir))
	await(t, "approval request", approvals.asked)
	approvals.decisions <- permission.Reject

	done := terminal(t, manager, "op-1")
	state, err := operation.DecodeShellState(done)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != operation.StatusFailed || !strings.Contains(strings.ToLower(state.TerminalError), "rejected") {
		t.Fatalf("status = %s, error = %q", done.Status, state.TerminalError)
	}
	if exists(filepath.Join(dir, "ran")) {
		t.Fatal("rejected command ran")
	}
}

func TestCancelWhileAwaitingApprovalCancelsTheRequest(t *testing.T) {
	dir := t.TempDir()
	approvals := newApprover()
	manager := newManager(t, permission.NewGate("s", approvals))

	manager.Add(shellOperation(t, "op-1", "touch ran", dir))
	await(t, "approval request", approvals.asked)
	if err := manager.Cancel("op-1", "stop"); err != nil {
		t.Fatal(err)
	}

	if done := terminal(t, manager, "op-1"); done.Status != operation.StatusCanceled {
		t.Fatalf("status = %s", done.Status)
	}
	await(t, "approval context cancellation", approvals.canceled)
	if exists(filepath.Join(dir, "ran")) {
		t.Fatal("canceled command ran")
	}
}

// stubbornApprover ignores cancellation and answers with decision once released.
type stubbornApprover struct {
	asked    chan struct{}
	release  chan struct{}
	decision permission.Decision
	err      error
}

func (a *stubbornApprover) Approve(context.Context, permission.Request) (permission.Decision, error) {
	a.asked <- struct{}{}
	<-a.release
	return a.decision, a.err
}

func TestDecisionArrivingAfterCancelDoesNotRun(t *testing.T) {
	dir := t.TempDir()
	approvals := &stubbornApprover{asked: make(chan struct{}, 1), release: make(chan struct{}), decision: permission.AllowOnce}
	manager := newManager(t, permission.NewGate("s", approvals))
	manager.Add(shellOperation(t, "op-1", "touch ran", dir))
	await(t, "approval request", approvals.asked)
	manager.Cancel("op-1", "stop")
	if done := terminal(t, manager, "op-1"); done.Status != operation.StatusCanceled {
		t.Fatalf("status = %s", done.Status)
	}
	close(approvals.release)
	time.Sleep(100 * time.Millisecond)
	if exists(filepath.Join(dir, "ran")) {
		t.Fatal("command ran after it was canceled")
	}
}

func TestApproverOutcomesWithoutApproval(t *testing.T) {
	for name, test := range map[string]struct {
		decision permission.Decision
		err      error
		want     operation.Status
	}{
		"client answered cancelled": {decision: permission.Cancelled, want: operation.StatusCanceled},
		"approver failed":           {decision: permission.Cancelled, err: errors.New("client went away"), want: operation.StatusFailed},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			approvals := &stubbornApprover{asked: make(chan struct{}, 1), release: make(chan struct{}), decision: test.decision, err: test.err}
			close(approvals.release)
			manager := newManager(t, permission.NewGate("s", approvals))
			manager.Add(shellOperation(t, "op-1", "touch ran", dir))
			if done := terminal(t, manager, "op-1"); done.Status != test.want {
				t.Fatalf("status = %s, want %s", done.Status, test.want)
			}
			if exists(filepath.Join(dir, "ran")) {
				t.Fatal("command ran without approval")
			}
		})
	}
}

func TestSeveralCommandsCanAwaitApprovalAtOnce(t *testing.T) {
	dir := t.TempDir()
	approvals := newApprover()
	manager := newManager(t, permission.NewGate("s", approvals))
	manager.Add(shellOperation(t, "op-1", "touch one", dir))
	manager.Add(shellOperation(t, "op-2", "touch two", dir))
	await(t, "first approval request", approvals.asked)
	await(t, "second approval request", approvals.asked)
	approvals.decisions <- permission.AllowOnce
	approvals.decisions <- permission.AllowOnce
	statuses := map[operation.ID]operation.Status{}
	deadline := time.After(10 * time.Second)
	for len(statuses) < 2 {
		select {
		case update := <-manager.Updates():
			if update.Status == operation.StatusCompleted {
				statuses[update.ID] = update.Status
			}
		case <-deadline:
			t.Fatalf("completed = %v", statuses)
		}
	}
}

func TestAllowAlwaysSkipsLaterApprovalsInTheSession(t *testing.T) {
	dir := t.TempDir()
	approvals := newApprover()
	gate := permission.NewGate("s", approvals)
	first := newManager(t, gate)
	first.Add(shellOperation(t, "op-1", "touch one", dir))
	await(t, "first approval", approvals.asked)
	approvals.decisions <- permission.AllowAlways
	terminal(t, first, "op-1")

	second := newManager(t, gate) // a later prompt
	second.Add(shellOperation(t, "op-2", "touch two", dir))
	if done := terminal(t, second, "op-2"); done.Status != operation.StatusCompleted {
		t.Fatalf("status = %s", done.Status)
	}
	if approvals.count() != 1 || !exists(filepath.Join(dir, "two")) {
		t.Fatalf("approvals = %d, ran = %v", approvals.count(), exists(filepath.Join(dir, "two")))
	}
}

func TestFullAccessModeNeverAsks(t *testing.T) {
	dir := t.TempDir()
	approvals := newApprover()
	gate := permission.NewGate("s", approvals)
	if err := gate.SetMode(permission.FullAccess); err != nil {
		t.Fatal(err)
	}
	manager := newManager(t, gate)
	manager.Add(shellOperation(t, "op-1", "touch ran", dir))
	if done := terminal(t, manager, "op-1"); done.Status != operation.StatusCompleted || approvals.count() != 0 {
		t.Fatalf("status = %s, approvals = %d", done.Status, approvals.count())
	}
	if gate.Mode() != permission.FullAccess {
		t.Fatalf("mode = %q", gate.Mode())
	}
}

func TestSetModeRejectsUnknownModes(t *testing.T) {
	gate := permission.NewGate("s", newApprover())
	if err := gate.SetMode("yolo"); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if gate.Mode() != permission.Ask {
		t.Fatalf("default mode = %q", gate.Mode())
	}
}

func TestNonShellOperationsAreNotGated(t *testing.T) {
	approvals := newApprover()
	manager := newManager(t, permission.NewGate("s", approvals))
	value, err := operation.NewValueSpec(jsontext.Value(`"hello"`))
	if err != nil {
		t.Fatal(err)
	}
	manager.Add(operation.Operation{ID: "op-1", Type: value.Type, Version: value.Version, Status: operation.StatusReady, State: value.State})
	if done := terminal(t, manager, "op-1"); done.Status != operation.StatusCompleted || approvals.count() != 0 {
		t.Fatalf("status = %s, approvals = %d", done.Status, approvals.count())
	}
}
