// Package permission gates shell operations behind user approval.
//
// A Gate holds one session's policy (mode and "allow always"). Gate.Wrap
// wraps a prompt's operation.Manager: shell operations are held until an
// Approver decides, while the coordinator loop keeps running because Add
// never waits for the decision. Rejected and canceled operations are
// finished with terminal shell states the coordinator records like any other.
package permission

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

type Decision int

const (
	// Cancelled means the request was withdrawn before the user decided.
	Cancelled Decision = iota
	AllowOnce
	// AllowAlways allows this and every later shell command in the session.
	AllowAlways
	Reject
)

type Mode string

const (
	// Ask requests approval for every shell command until the user allows always.
	Ask Mode = "ask"
	// FullAccess runs shell commands without asking.
	FullAccess Mode = "full-access"
)

const (
	rejectedMessage  = "The user rejected this command."
	cancelledMessage = "The permission request was cancelled before the user decided."
	canceledMessage  = "shell operation canceled"
)

type Request struct {
	SessionID   session.ID
	OperationID operation.ID
	Command     string
	Directory   string
}

// Approver asks the user. Approve may block; its context is canceled when the
// operation is canceled or the prompt ends.
type Approver interface {
	Approve(context.Context, Request) (Decision, error)
}

type Gate struct {
	sessionID session.ID
	approver  Approver

	mu            sync.Mutex
	mode          Mode
	alwaysAllowed bool
}

func NewGate(sessionID session.ID, approver Approver) *Gate {
	return &Gate{sessionID: sessionID, approver: approver, mode: Ask}
}

func (g *Gate) Mode() Mode {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mode
}

func (g *Gate) SetMode(mode Mode) error {
	switch mode {
	case Ask, FullAccess:
	default:
		return fmt.Errorf("unknown permission mode %q", mode)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = mode
	return nil
}

func (g *Gate) allowed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mode == FullAccess || g.alwaysAllowed
}

func (g *Gate) allowAlways() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.alwaysAllowed = true
}

// Wrap gates inner, which must be owned by a single prompt.
func (g *Gate) Wrap(inner operation.Manager) operation.Manager {
	m := &manager{
		gate:      g,
		inner:     inner,
		updates:   make(chan operation.Operation),
		synthetic: make(chan operation.Operation),
		done:      make(chan struct{}),
		pending:   make(map[operation.ID]*pending),
	}
	go m.forward()
	return m
}

type manager struct {
	gate      *Gate
	inner     operation.Manager
	updates   chan operation.Operation
	synthetic chan operation.Operation
	done      chan struct{}

	// mu serializes decisions with Cancel so an approved operation is handed
	// to inner before a later Cancel looks for it there.
	mu      sync.Mutex
	pending map[operation.ID]*pending
}

type pending struct {
	value  operation.Operation
	cancel context.CancelFunc
}

func (m *manager) Updates() <-chan operation.Operation { return m.updates }

func (m *manager) Add(value operation.Operation) error {
	if value.Type != operation.TypeShell || m.gate.allowed() {
		return m.inner.Add(value)
	}
	state, err := operation.DecodeShellState(value)
	if err != nil {
		return m.inner.Add(value)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.pending[value.ID]; exists {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	held := &pending{value: value, cancel: cancel}
	m.pending[value.ID] = held
	go m.ask(ctx, held, Request{
		SessionID:   m.gate.sessionID,
		OperationID: value.ID,
		Command:     state.Input.Command,
		Directory:   state.Input.Directory,
	})
	return nil
}

func (m *manager) ask(ctx context.Context, held *pending, request Request) {
	decision, approveErr := m.gate.approver.Approve(ctx, request)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending[held.value.ID] != held {
		return // canceled while asking
	}
	delete(m.pending, held.value.ID)
	held.cancel()

	switch {
	case approveErr != nil:
		m.emit(finish(held.value, operation.StatusFailed, "permission request failed: "+approveErr.Error()))
	case decision == AllowAlways || decision == AllowOnce:
		if decision == AllowAlways {
			m.gate.allowAlways()
		}
		if err := m.inner.Add(held.value); err != nil {
			m.emit(finish(held.value, operation.StatusFailed, err.Error()))
		}
	case decision == Reject:
		m.emit(finish(held.value, operation.StatusFailed, rejectedMessage))
	default:
		m.emit(finish(held.value, operation.StatusCanceled, cancelledMessage))
	}
}

func (m *manager) Cancel(id operation.ID, reason string) error {
	m.mu.Lock()
	held, exists := m.pending[id]
	if exists {
		delete(m.pending, id)
	}
	m.mu.Unlock()
	if !exists {
		return m.inner.Cancel(id, reason)
	}
	held.cancel()
	m.emit(finish(held.value, operation.StatusCanceled, canceledMessage))
	return nil
}

func (m *manager) emit(value operation.Operation) {
	select {
	case m.synthetic <- value:
	case <-m.done:
	}
}

// forward merges inner updates with gate decisions until inner shuts down,
// then withdraws any outstanding approval requests.
func (m *manager) forward() {
	defer func() {
		m.mu.Lock()
		for id, held := range m.pending {
			held.cancel()
			delete(m.pending, id)
		}
		m.mu.Unlock()
	}()
	defer close(m.updates)
	defer close(m.done)

	innerUpdates := m.inner.Updates()
	var queue []operation.Operation
	for {
		var out chan operation.Operation
		var next operation.Operation
		if len(queue) != 0 {
			out, next = m.updates, queue[0]
		}
		select {
		case value, open := <-innerUpdates:
			if !open {
				return
			}
			queue = append(queue, value)
		case value := <-m.synthetic:
			queue = append(queue, value)
		case out <- next:
			queue = queue[1:]
		}
	}
}

// finish ends a held shell operation the way the shell actor would.
func finish(value operation.Operation, status operation.Status, message string) operation.Operation {
	value.Status = status
	state, err := operation.DecodeShellState(value)
	if err != nil {
		return value
	}
	state.Phase = ""
	state.TerminalError = message
	state.ErrorTruncated = false
	encoded, err := json.Marshal(state)
	if err != nil {
		return value
	}
	value.State = encoded
	return value
}
