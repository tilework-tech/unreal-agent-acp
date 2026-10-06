// Package fakemodel is an OpenAI Responses API server for tests. It speaks the
// streaming wire format unreal-agent's responsesapi adapter consumes, so tests
// exercise the real LLM client over HTTP instead of a fake llm.Adapter.
package fakemodel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
)

// Request is one decoded /responses call.
type Request struct {
	Body map[string]any
	ctx  context.Context
}

// Context is canceled when the client abandons the request.
func (r Request) Context() context.Context { return r.ctx }

func (r Request) Model() string {
	model, _ := r.Body["model"].(string)
	return model
}

func (r Request) ReasoningEffort() string {
	reasoning, _ := r.Body["reasoning"].(map[string]any)
	effort, _ := reasoning["effort"].(string)
	return effort
}

func (r Request) input() []map[string]any {
	raw, _ := r.Body["input"].([]any)
	items := make([]map[string]any, 0, len(raw))
	for _, value := range raw {
		if item, ok := value.(map[string]any); ok {
			items = append(items, item)
		}
	}
	return items
}

// UserTexts returns the text of every user message in the request input, in order.
func (r Request) UserTexts() []string {
	var texts []string
	for _, item := range r.input() {
		if item["role"] != "user" {
			continue
		}
		texts = append(texts, contentText(item["content"]))
	}
	return texts
}

// ToolOutputs maps call_id to the text output of each function_call_output in the input.
func (r Request) ToolOutputs() map[string]string {
	outputs := map[string]string{}
	for _, item := range r.input() {
		if item["type"] != "function_call_output" {
			continue
		}
		callID, _ := item["call_id"].(string)
		outputs[callID] = contentText(item["output"])
	}
	return outputs
}

// ToolNames lists the function tools offered to the model.
func (r Request) ToolNames() []string {
	raw, _ := r.Body["tools"].([]any)
	var names []string
	for _, value := range raw {
		if tool, ok := value.(map[string]any); ok {
			if name, ok := tool["name"].(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

func contentText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var text string
		for _, part := range value {
			if part, ok := part.(map[string]any); ok {
				if t, ok := part["text"].(string); ok {
					text += t
				}
			}
		}
		return text
	default:
		return ""
	}
}

// Reply is the model output for one request.
type Reply struct {
	Items        []map[string]any
	InputTokens  int64
	OutputTokens int64
	Incomplete   string // "max_output_tokens" or "content_filter" makes the response incomplete
}

func Text(text string) Reply {
	return Reply{Items: []map[string]any{{
		"type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}}}
}

func ToolCall(callID, name string, arguments any) Reply {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		panic(err)
	}
	return Reply{Items: []map[string]any{{
		"type": "function_call", "call_id": callID, "name": name,
		"arguments": string(encoded), "status": "completed",
	}}}
}

func Reasoning(summary string) Reply {
	return Reply{Items: []map[string]any{{
		"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": summary}},
	}}}
}

// Then appends the items of next after the items of reply.
func (reply Reply) Then(next Reply) Reply {
	reply.Items = append(append([]map[string]any(nil), reply.Items...), next.Items...)
	return reply
}

// Sequence answers successive requests with replies in order, then with Text("done").
func Sequence(replies ...Reply) func(Request) Reply {
	var mu sync.Mutex
	return func(Request) Reply {
		mu.Lock()
		defer mu.Unlock()
		if len(replies) == 0 {
			return Text("done")
		}
		next := replies[0]
		replies = replies[1:]
		return next
	}
}

type Server struct {
	URL string

	mu       sync.Mutex
	requests []Request
	counter  atomic.Int64
}

// Start serves POST /responses until the test ends.
func Start(t interface {
	Cleanup(func())
	Helper()
}, handler func(Request) Reply,
) *Server {
	t.Helper()
	server, httpHandler := New(handler)
	httpServer := httptest.NewServer(httpHandler)
	t.Cleanup(httpServer.Close)
	server.URL = httpServer.URL
	return server
}

// New returns a server whose HTTP handler answers POST /responses with handler.
// The caller serves the handler and sets URL.
func New(handler func(Request) Reply) (*Server, http.Handler) {
	server := &Server{}
	return server, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.serve(w, r, handler)
	})
}

// Requests returns every request received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Env is the unreal-agent environment that points the openai provider at s.
func (s *Server) Env() map[string]string {
	return map[string]string{
		"UNREAL_HARNESS_LLM_PROVIDER":     "openai",
		"UNREAL_HARNESS_LLM_BASE_URL":     s.URL,
		"UNREAL_HARNESS_LLM_API_KEY":      "test-key",
		"UNREAL_HARNESS_LLM_MODEL":        "fake-model",
		"UNREAL_HARNESS_LLM_MAX_ATTEMPTS": "1",
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, handler func(Request) Reply) {
	if r.Method != http.MethodPost || r.URL.Path != "/responses" {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	request := Request{Body: body, ctx: r.Context()}
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()

	reply := handler(request)
	if r.Context().Err() != nil {
		return
	}
	number := s.counter.Add(1)
	output := make([]map[string]any, 0, len(reply.Items))
	for index, item := range reply.Items {
		copied := make(map[string]any, len(item)+1)
		for key, value := range item {
			copied[key] = value
		}
		if _, hasID := copied["id"]; !hasID {
			copied["id"] = fmt.Sprintf("item_%d_%d", number, index)
		}
		output = append(output, copied)
	}
	status := "completed"
	response := map[string]any{
		"id": fmt.Sprintf("resp_%d", number), "object": "response", "created_at": 0,
		"model": request.Model(), "output": output,
		"usage": map[string]any{
			"input_tokens": reply.InputTokens, "output_tokens": reply.OutputTokens,
			"total_tokens":          reply.InputTokens + reply.OutputTokens,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		},
	}
	if reply.Incomplete != "" {
		status = "incomplete"
		response["incomplete_details"] = map[string]any{"reason": reply.Incomplete}
	}
	response["status"] = status
	event, err := json.Marshal(map[string]any{"type": "response." + status, "response": response})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "event: response.%s\ndata: %s\n\n", status, event)
}
