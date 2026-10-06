// Package translate turns unreal-agent session items into neutral,
// protocol-independent conversation updates.
//
// The same Session translates live items as they are appended and persisted
// items during replay, so a client that loads a session sees the same
// updates it would have seen live.
package translate

import (
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/bash"
)

const maxTitleLength = 120

type Kind string

const (
	UserMessage     Kind = "user_message"
	AgentMessage    Kind = "agent_message"
	AgentThought    Kind = "agent_thought"
	ToolCallStarted Kind = "tool_call"
	ToolCallUpdated Kind = "tool_call_update"
)

type ToolKind string

const (
	KindRead    ToolKind = "read"
	KindExecute ToolKind = "execute"
	KindOther   ToolKind = "other"
)

type ToolStatus string

const (
	StatusPending    ToolStatus = "pending"
	StatusInProgress ToolStatus = "in_progress"
	StatusCompleted  ToolStatus = "completed"
	StatusFailed     ToolStatus = "failed"
)

type Update struct {
	Kind Kind
	// MessageID identifies the message a UserMessage, AgentMessage, or AgentThought belongs to.
	MessageID string
	Text      string
	// Tool is set for ToolCallStarted and ToolCallUpdated.
	Tool *Tool
}

type Tool struct {
	CallID string
	// Name, Title, Kind, RawInput, and Locations are set on ToolCallStarted.
	Name      string
	Title     string
	Kind      ToolKind
	Status    ToolStatus
	RawInput  any
	Output    string
	Locations []string
}

// Session translates the item stream of one session. It is not safe for concurrent use.
type Session struct {
	calls    map[string]llm.ToolCall
	reported map[string]ToolStatus
}

func NewSession() *Session {
	return &Session{calls: map[string]llm.ToolCall{}, reported: map[string]ToolStatus{}}
}

func (s *Session) Translate(item sessionstore.Item) []Update {
	switch data := item.Data.(type) {
	case inbox.Input:
		return translateInput(data)
	case sessionstore.ModelResponse:
		return s.translateResponse(data.Response)
	case sessionstore.ToolCallStatus:
		return s.translateStatus(data)
	default:
		return nil
	}
}

func translateInput(input inbox.Input) []Update {
	if input.Kind != inbox.InputExternal {
		return nil
	}
	var text string
	if err := json.Unmarshal(input.Payload, &text); err != nil {
		return nil
	}
	return []Update{{Kind: UserMessage, MessageID: string(input.ID), Text: text}}
}

func (s *Session) translateResponse(response llm.Response) []Update {
	var updates []Update
	for index, output := range response.Output {
		id := output.ProviderID
		if id == "" {
			id = fmt.Sprintf("%s-%d", response.ID, index)
		}
		switch data := output.Data.(type) {
		case llm.Message:
			if data.Text != "" {
				updates = append(updates, Update{Kind: AgentMessage, MessageID: id, Text: data.Text})
			}
		case llm.Reasoning:
			if text := strings.TrimSpace(strings.Join(data.Summary, "\n\n")); text != "" {
				updates = append(updates, Update{Kind: AgentThought, MessageID: id, Text: text})
			}
		case llm.ToolCall:
			s.calls[data.CallID] = data
			s.reported[data.CallID] = StatusPending
			updates = append(updates, Update{Kind: ToolCallStarted, Tool: started(data)})
		}
	}
	return updates
}

func (s *Session) translateStatus(status sessionstore.ToolCallStatus) []Update {
	current, output := result(s.calls[status.CallID], status)
	if previous, seen := s.reported[status.CallID]; seen && previous == current && current == StatusInProgress {
		return nil
	}
	s.reported[status.CallID] = current
	return []Update{{Kind: ToolCallUpdated, Tool: &Tool{CallID: status.CallID, Status: current, Output: output}}}
}

func started(call llm.ToolCall) *Tool {
	var arguments map[string]any
	_ = json.Unmarshal([]byte(call.Arguments), &arguments)
	argument := func(name string) string {
		value, _ := arguments[name].(string)
		return value
	}
	started := &Tool{CallID: call.CallID, Name: call.Name, Title: call.Name, Kind: KindOther, Status: StatusPending}
	if arguments != nil {
		started.RawInput = arguments
	}
	switch call.Name {
	case tool.BashName:
		started.Kind = KindExecute
		if command := argument("command"); command != "" {
			started.Title = shorten(command)
		}
	case tool.ViewImageName:
		started.Kind = KindRead
		if path := argument("path"); path != "" {
			started.Title = "View " + shorten(path)
			started.Locations = []string{path}
		}
	case tool.SkillUseName:
		started.Kind = KindRead
		if name := argument("name"); name != "" {
			started.Title = "Load skill " + shorten(name)
		}
	}
	return started
}

func result(call llm.ToolCall, status sessionstore.ToolCallStatus) (ToolStatus, string) {
	if status.Status.Error != "" && len(status.Operations) == 0 {
		return StatusFailed, "Error: " + status.Status.Error
	}
	current := StatusCompleted
	for _, value := range status.Operations {
		switch value.Status {
		case operation.StatusFailed, operation.StatusCanceled:
			if current != StatusInProgress {
				current = StatusFailed
			}
		case operation.StatusCompleted:
		default:
			current = StatusInProgress
		}
	}
	if current == StatusInProgress {
		return current, ""
	}
	return current, outputText(call, status)
}

// outputText renders what the model sees for a finished call.
func outputText(call llm.ToolCall, status sessionstore.ToolCallStatus) string {
	var translator tool.ResultTranslator
	switch call.Name {
	case tool.BashName:
		translator = bash.New(bash.Config{})
	default:
		if len(status.Operations) == 1 && status.Operations[0].Type == operation.TypeShell {
			translator = bash.New(bash.Config{})
		}
	}
	if translator == nil {
		return status.Status.Error
	}
	translated, err := translator.TranslateResult(status.CallID, status.Status, status.Operations)
	if err != nil {
		return "Error: " + err.Error()
	}
	var parts []string
	for _, output := range translated.Output {
		if output.Kind == llm.ToolResultText {
			parts = append(parts, output.Value)
		}
	}
	return strings.Join(parts, "\n")
}

func shorten(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > maxTitleLength {
		return string(runes[:maxTitleLength-1]) + "…"
	}
	return text
}
