package jsonrpc_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tilework-tech/unreal-agent-acp/acp/jsonrpc"
)

// peer is the remote side of a Conn: it writes raw lines and reads raw frames.
type peer struct {
	t      *testing.T
	writer *io.PipeWriter
	frames chan map[string]json.RawMessage
}

type handlerFuncs struct {
	request      func(context.Context, string, json.RawMessage) (any, error)
	notification func(context.Context, string, json.RawMessage)
}

func (h handlerFuncs) HandleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if h.request == nil {
		return nil, jsonrpc.ErrMethodNotFound(method)
	}
	return h.request(ctx, method, params)
}

func (h handlerFuncs) HandleNotification(ctx context.Context, method string, params json.RawMessage) {
	if h.notification != nil {
		h.notification(ctx, method, params)
	}
}

func start(t *testing.T, handler jsonrpc.Handler) (*jsonrpc.Conn, *peer, <-chan error) {
	t.Helper()
	toConn, fromPeer := io.Pipe()
	fromConn, toPeer := io.Pipe()
	conn := jsonrpc.NewConn(toConn, toPeer, handler)
	served := make(chan error, 1)
	go func() { served <- conn.Serve(t.Context()) }()
	p := &peer{t: t, writer: fromPeer, frames: make(chan map[string]json.RawMessage, 16)}
	done := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(fromConn)
		scanner.Buffer(make([]byte, 1<<20), 1<<24)
		for scanner.Scan() {
			var frame map[string]json.RawMessage
			if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
				frame = map[string]json.RawMessage{"invalid": json.RawMessage(strconv.Quote(scanner.Text()))}
			}
			select {
			case p.frames <- frame:
			case <-done:
				return
			}
		}
		close(p.frames)
	}()
	t.Cleanup(func() {
		close(done)
		fromPeer.Close()
		fromConn.Close()
	})
	return conn, p, served
}

func (p *peer) send(line string) {
	p.t.Helper()
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(p.writer, line+"\n")
		written <- err
	}()
	if err := recv(p.t, written); err != nil {
		p.t.Fatalf("write: %v", err)
	}
}

func (p *peer) next() map[string]json.RawMessage {
	p.t.Helper()
	select {
	case frame, ok := <-p.frames:
		if !ok {
			p.t.Fatal("connection closed while waiting for a frame")
		}
		if raw, invalid := frame["invalid"]; invalid {
			p.t.Fatalf("conn wrote invalid JSON %s", raw)
		}
		return frame
	case <-time.After(5 * time.Second):
		p.t.Fatal("timed out waiting for a frame")
		return nil
	}
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting on channel")
		var zero T
		return zero
	}
}

func errorCode(t *testing.T, frame map[string]json.RawMessage) int {
	t.Helper()
	var body struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(frame["error"], &body); err != nil {
		t.Fatalf("frame has no error object: %v", frame)
	}
	return body.Code
}

func TestRequestIsAnsweredWithHandlerResultAndSameID(t *testing.T) {
	_, p, _ := start(t, handlerFuncs{request: func(_ context.Context, method string, params json.RawMessage) (any, error) {
		var in struct{ N int }
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, err
		}
		return map[string]any{"method": method, "double": in.N * 2}, nil
	}})

	p.send(`{"jsonrpc":"2.0","id":"abc","method":"math/double","params":{"N":21}}`)
	frame := p.next()

	if string(frame["id"]) != `"abc"` || string(frame["jsonrpc"]) != `"2.0"` {
		t.Fatalf("frame = %v", frame)
	}
	var result struct {
		Method string
		Double int
	}
	if err := json.Unmarshal(frame["result"], &result); err != nil || result.Method != "math/double" || result.Double != 42 {
		t.Fatalf("result = %s", frame["result"])
	}
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	_, p, _ := start(t, handlerFuncs{})
	p.send(`{"jsonrpc":"2.0","id":7,"method":"nope"}`)
	frame := p.next()
	if string(frame["id"]) != "7" || errorCode(t, frame) != -32601 {
		t.Fatalf("frame = %v", frame)
	}
}

func TestMalformedJSONReturnsParseErrorAndKeepsServing(t *testing.T) {
	_, p, _ := start(t, handlerFuncs{request: func(context.Context, string, json.RawMessage) (any, error) {
		return "ok", nil
	}})
	p.send(`{"jsonrpc":"2.0","id":1,`)
	frame := p.next()
	if string(frame["id"]) != "null" || errorCode(t, frame) != -32700 {
		t.Fatalf("frame = %v", frame)
	}
	p.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if frame := p.next(); string(frame["result"]) != `"ok"` {
		t.Fatalf("frame after parse error = %v", frame)
	}
}

func TestHandlerErrorsMapToJSONRPCErrors(t *testing.T) {
	_, p, _ := start(t, handlerFuncs{request: func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "typed" {
			return nil, fmt.Errorf("wrapped: %w", &jsonrpc.Error{Code: -32000, Message: "auth required"})
		}
		return nil, errors.New("boom")
	}})
	p.send(`{"jsonrpc":"2.0","id":1,"method":"typed"}`)
	if frame := p.next(); errorCode(t, frame) != -32000 {
		t.Fatalf("typed error frame = %v", frame)
	}
	p.send(`{"jsonrpc":"2.0","id":2,"method":"untyped"}`)
	if frame := p.next(); errorCode(t, frame) != -32603 {
		t.Fatalf("untyped error frame = %v", frame)
	}
}

func TestCancelRequestCancelsInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	_, p, _ := start(t, handlerFuncs{request: func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	p.send(`{"jsonrpc":"2.0","id":"slow","method":"wait"}`)
	recv(t, started)
	p.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"slow"}}`)
	frame := p.next()
	if string(frame["id"]) != `"slow"` || errorCode(t, frame) != -32800 {
		t.Fatalf("frame = %v", frame)
	}
}

func TestRequestsAreHandledConcurrently(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	_, p, _ := start(t, handlerFuncs{request: func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "block" {
			<-release
		}
		return method, nil
	}})
	p.send(`{"jsonrpc":"2.0","id":1,"method":"block"}`)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"fast"}`)
	if frame := p.next(); string(frame["id"]) != "2" {
		t.Fatalf("first frame = %v, want the fast request", frame)
	}
	close(release)
	if frame := p.next(); string(frame["id"]) != "1" {
		t.Fatalf("second frame = %v", frame)
	}
}

func TestNotificationsAreDeliveredInOrderWithoutResponse(t *testing.T) {
	got := make(chan string, 3)
	_, p, _ := start(t, handlerFuncs{
		notification: func(_ context.Context, method string, params json.RawMessage) {
			got <- method + " " + string(params)
		},
		request: func(context.Context, string, json.RawMessage) (any, error) { return "pong", nil },
	})
	p.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`)
	p.send(`{"jsonrpc":"2.0","method":"_vendor/ping"}`)
	p.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"t"}}`)
	for _, want := range []string{`session/cancel {"sessionId":"s"}`, "_vendor/ping", `session/cancel {"sessionId":"t"}`} {
		if got := recv(t, got); !strings.HasPrefix(got, want) {
			t.Fatalf("notification = %q, want %q", got, want)
		}
	}
	p.send(`{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if frame := p.next(); string(frame["id"]) != "9" {
		t.Fatalf("first frame after notifications = %v, want the ping response", frame)
	}
}

func TestUnknownCancelRequestIsIgnored(t *testing.T) {
	notified := make(chan string, 1)
	_, p, _ := start(t, handlerFuncs{
		notification: func(_ context.Context, method string, _ json.RawMessage) { notified <- method },
		request:      func(context.Context, string, json.RawMessage) (any, error) { return "pong", nil },
	})
	p.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"missing"}}`)
	p.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if frame := p.next(); string(frame["id"]) != "1" {
		t.Fatalf("frame = %v", frame)
	}
	select {
	case method := <-notified:
		t.Fatalf("protocol notification leaked to handler: %s", method)
	default:
	}
}

func TestMessageWithoutMethodOrResultIsInvalidRequest(t *testing.T) {
	_, p, _ := start(t, handlerFuncs{})
	p.send(`{"jsonrpc":"2.0","id":3}`)
	if frame := p.next(); string(frame["id"]) != "3" || errorCode(t, frame) != -32600 {
		t.Fatalf("frame = %v", frame)
	}
}

func TestLargeMessagesRoundTrip(t *testing.T) {
	_, p, _ := start(t, handlerFuncs{request: func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		var in struct{ Text string }
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, err
		}
		return len(in.Text), nil
	}})
	big := strings.Repeat("x", 4<<20)
	p.send(`{"jsonrpc":"2.0","id":1,"method":"size","params":{"Text":"` + big + `"}}`)
	if frame := p.next(); string(frame["result"]) != strconv.Itoa(len(big)) {
		t.Fatalf("result = %s", frame["result"])
	}
}

func TestServeReturnsWhenContextIsCanceled(t *testing.T) {
	toConn, _ := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- jsonrpc.NewConn(toConn, io.Discard, handlerFuncs{}).Serve(ctx) }()
	cancel()
	if err := recv(t, served); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve = %v", err)
	}
}

func TestConcurrentCallsReceiveTheirOwnResults(t *testing.T) {
	conn, p, _ := start(t, handlerFuncs{})
	results := make(chan string, 2)
	for _, name := range []string{"a", "b"} {
		go func() {
			var out string
			if err := conn.Call(t.Context(), name, nil, &out); err != nil {
				out = "error: " + err.Error()
			}
			results <- name + "=" + out
		}()
	}
	first, second := p.next(), p.next()
	for _, request := range []map[string]json.RawMessage{second, first} {
		var method string
		json.Unmarshal(request["method"], &method)
		p.send(`{"jsonrpc":"2.0","id":` + string(request["id"]) + `,"result":"` + method + `-result"}`)
	}
	got := map[string]bool{recv(t, results): true, recv(t, results): true}
	if !got["a=a-result"] || !got["b=b-result"] {
		t.Fatalf("results = %v", got)
	}
}

func TestCallCorrelatesResponse(t *testing.T) {
	conn, p, _ := start(t, handlerFuncs{})
	type answer struct {
		Outcome string `json:"outcome"`
	}
	result := make(chan answer, 1)
	failed := make(chan error, 1)
	go func() {
		var out answer
		if err := conn.Call(t.Context(), "session/request_permission", map[string]string{"sessionId": "s"}, &out); err != nil {
			failed <- err
			return
		}
		result <- out
	}()

	request := p.next()
	if string(request["method"]) != `"session/request_permission"` || string(request["params"]) != `{"sessionId":"s"}` {
		t.Fatalf("request = %v", request)
	}
	p.send(`{"jsonrpc":"2.0","id":` + string(request["id"]) + `,"result":{"outcome":"allow"}}`)
	select {
	case out := <-result:
		if out.Outcome != "allow" {
			t.Fatalf("outcome = %q", out.Outcome)
		}
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("Call did not return")
	}
}

func TestCallReturnsRemoteError(t *testing.T) {
	conn, p, _ := start(t, handlerFuncs{})
	failed := make(chan error, 1)
	go func() { failed <- conn.Call(t.Context(), "x", nil, nil) }()
	request := p.next()
	p.send(`{"jsonrpc":"2.0","id":` + string(request["id"]) + `,"error":{"code":-32602,"message":"bad"}}`)
	var rpcErr *jsonrpc.Error
	if err := recv(t, failed); !errors.As(err, &rpcErr) || rpcErr.Code != -32602 || rpcErr.Message != "bad" {
		t.Fatalf("err = %v", err)
	}
}

func TestCanceledCallSendsCancelRequest(t *testing.T) {
	conn, p, _ := start(t, handlerFuncs{})
	ctx, cancel := context.WithCancel(t.Context())
	failed := make(chan error, 1)
	go func() { failed <- conn.Call(ctx, "slow", nil, nil) }()
	request := p.next()
	cancel()
	if err := recv(t, failed); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	notice := p.next()
	if string(notice["method"]) != `"$/cancel_request"` || string(notice["params"]) != `{"requestId":`+string(request["id"])+`}` {
		t.Fatalf("cancel notice = %v", notice)
	}
}

func TestServeReturnsOnEOFAndFailsPendingCalls(t *testing.T) {
	conn, p, served := start(t, handlerFuncs{})
	failed := make(chan error, 1)
	go func() { failed <- conn.Call(t.Context(), "never", nil, nil) }()
	p.next()
	p.writer.Close()
	if err := recv(t, served); err != nil {
		t.Fatalf("Serve = %v, want nil on EOF", err)
	}
	if err := recv(t, failed); !errors.Is(err, jsonrpc.ErrClosed) {
		t.Fatalf("pending call err = %v", err)
	}
}

func TestNotifyWritesNotification(t *testing.T) {
	conn, p, _ := start(t, handlerFuncs{})
	if err := conn.Notify(t.Context(), "session/update", map[string]string{"sessionId": "s"}); err != nil {
		t.Fatal(err)
	}
	frame := p.next()
	if _, hasID := frame["id"]; hasID || string(frame["method"]) != `"session/update"` ||
		string(frame["jsonrpc"]) != `"2.0"` || string(frame["params"]) != `{"sessionId":"s"}` {
		t.Fatalf("frame = %v", frame)
	}
}

type preparingHandler struct {
	handlerFuncs
	events chan string
}

func (h preparingHandler) PrepareRequest(ctx context.Context, method string, _ json.RawMessage) context.Context {
	h.events <- "prepare " + method
	return ctx
}

func TestPrepareRequestRunsBeforeLaterMessagesAreHandled(t *testing.T) {
	events := make(chan string, 8)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	handler := preparingHandler{events: events, handlerFuncs: handlerFuncs{
		request: func(context.Context, string, json.RawMessage) (any, error) {
			<-release
			return nil, nil
		},
		notification: func(_ context.Context, method string, _ json.RawMessage) { events <- "notify " + method },
	}}
	_, p, _ := start(t, handler)
	p.send(`{"jsonrpc":"2.0","id":1,"method":"session/prompt"}` + "\n" + `{"jsonrpc":"2.0","method":"session/cancel"}`)
	if first, second := recv(t, events), recv(t, events); first != "prepare session/prompt" || second != "notify session/cancel" {
		t.Fatalf("events = %q, %q", first, second)
	}
}
