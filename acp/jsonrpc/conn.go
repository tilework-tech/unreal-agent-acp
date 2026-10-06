// Package jsonrpc is a newline-delimited JSON-RPC 2.0 peer, as used by ACP over stdio.
//
// Incoming requests are handled concurrently, each in its own goroutine.
// Incoming notifications are handled synchronously and in order on the read
// loop, so handlers must not block. Outgoing frames are written atomically.
package jsonrpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

const (
	CodeParseError      = -32700
	CodeInvalidRequest  = -32600
	CodeMethodNotFound  = -32601
	CodeInvalidParams   = -32602
	CodeInternalError   = -32603
	CodeRequestCanceled = -32800

	cancelRequestMethod = "$/cancel_request"
)

var ErrClosed = errors.New("jsonrpc: connection closed")

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

func ErrMethodNotFound(method string) error {
	return &Error{Code: CodeMethodNotFound, Message: "method not found: " + method}
}

func ErrInvalidParams(err error) error {
	return &Error{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
}

type Handler interface {
	HandleRequest(ctx context.Context, method string, params json.RawMessage) (any, error)
	HandleNotification(ctx context.Context, method string, params json.RawMessage)
}

type Conn struct {
	reader  io.Reader
	handler Handler

	writeMu sync.Mutex
	writer  io.Writer

	mu       sync.Mutex
	nextID   int64
	pending  map[string]chan frame
	incoming map[string]context.CancelCauseFunc
	closed   bool
}

type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type successResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type errorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   *Error          `json:"error"`
}

type outgoing struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type cancelParams struct {
	RequestID json.RawMessage `json:"requestId"`
}

var errCanceledByPeer = errors.New("jsonrpc: request canceled by peer")

var nullID = json.RawMessage("null")

func NewConn(r io.Reader, w io.Writer, h Handler) *Conn {
	return &Conn{
		reader:   r,
		writer:   w,
		handler:  h,
		pending:  make(map[string]chan frame),
		incoming: make(map[string]context.CancelCauseFunc),
	}
}

// Serve reads frames until the input ends (returning nil) or ctx is done.
// In-flight incoming requests are canceled and pending calls fail with
// ErrClosed when Serve returns.
func (c *Conn) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.close()

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(c.reader)
		for {
			line, err := reader.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) != 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("jsonrpc: read: %w", err)
		case line := <-lines:
			if err := c.dispatch(ctx, line); err != nil {
				return err
			}
		}
	}
}

func (c *Conn) dispatch(ctx context.Context, line []byte) error {
	var message frame
	if err := json.Unmarshal(line, &message); err != nil {
		return c.writeError(nullID, &Error{Code: CodeParseError, Message: "parse error: " + err.Error()})
	}
	hasID := message.ID != nil
	switch {
	case message.Method != "" && hasID:
		c.startRequest(ctx, message)
		return nil
	case message.Method == cancelRequestMethod:
		var params cancelParams
		if json.Unmarshal(message.Params, &params) == nil {
			c.cancelIncoming(params.RequestID)
		}
		return nil
	case message.Method != "":
		c.handler.HandleNotification(ctx, message.Method, message.Params)
		return nil
	case hasID && (message.Result != nil || message.Error != nil):
		c.resolve(message)
		return nil
	default:
		id := message.ID
		if id == nil {
			id = nullID
		}
		return c.writeError(id, &Error{Code: CodeInvalidRequest, Message: "invalid request"})
	}
}

func (c *Conn) startRequest(ctx context.Context, message frame) {
	requestCtx, cancel := context.WithCancelCause(ctx)
	key := idKey(message.ID)
	c.mu.Lock()
	c.incoming[key] = cancel
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.incoming, key)
			c.mu.Unlock()
			cancel(nil)
		}()
		result, err := c.handler.HandleRequest(requestCtx, message.Method, message.Params)
		if err == nil {
			c.write(successResponse{JSONRPC: "2.0", ID: message.ID, Result: result})
			return
		}
		c.writeError(message.ID, toError(requestCtx, err))
	}()
}

func toError(ctx context.Context, err error) *Error {
	var rpcErr *Error
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	if errors.Is(context.Cause(ctx), errCanceledByPeer) {
		return &Error{Code: CodeRequestCanceled, Message: "request canceled"}
	}
	return &Error{Code: CodeInternalError, Message: err.Error()}
}

func (c *Conn) cancelIncoming(id json.RawMessage) {
	c.mu.Lock()
	cancel, exists := c.incoming[idKey(id)]
	c.mu.Unlock()
	if exists {
		cancel(errCanceledByPeer)
	}
}

func (c *Conn) resolve(message frame) {
	key := idKey(message.ID)
	c.mu.Lock()
	waiter, exists := c.pending[key]
	delete(c.pending, key)
	c.mu.Unlock()
	if exists {
		waiter <- message
	}
}

// Call sends a request and decodes the result into result (which may be nil).
// If ctx is done first, Call sends $/cancel_request and returns ctx's error.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	waiter := make(chan frame, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.nextID++
	id := c.nextID
	key := strconv.FormatInt(id, 10)
	c.pending[key] = waiter
	c.mu.Unlock()

	if err := c.write(outgoing{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		c.forget(key)
		return err
	}
	select {
	case response, open := <-waiter:
		if !open {
			return ErrClosed
		}
		if response.Error != nil {
			return response.Error
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(response.Result, result); err != nil {
			return fmt.Errorf("jsonrpc: decode %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		c.forget(key)
		cancelErr := c.write(outgoing{
			JSONRPC: "2.0",
			Method:  cancelRequestMethod,
			Params:  cancelParams{RequestID: json.RawMessage(key)},
		})
		return errors.Join(ctx.Err(), cancelErr)
	}
}

func (c *Conn) Notify(_ context.Context, method string, params any) error {
	return c.write(outgoing{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Conn) forget(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

func (c *Conn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for key, waiter := range c.pending {
		close(waiter)
		delete(c.pending, key)
	}
	for _, cancel := range c.incoming {
		cancel(ErrClosed)
	}
}

func (c *Conn) writeError(id json.RawMessage, rpcErr *Error) error {
	return c.write(errorResponse{JSONRPC: "2.0", ID: id, Error: rpcErr})
}

func (c *Conn) write(message any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("jsonrpc: encode: %w", err)
	}
	encoded = append(encoded, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.writer.Write(encoded); err != nil {
		return fmt.Errorf("jsonrpc: write: %w", err)
	}
	return nil
}

// idKey normalizes a JSON-RPC id so that the same id always maps to the same key.
func idKey(id json.RawMessage) string {
	return string(bytes.TrimSpace(id))
}
