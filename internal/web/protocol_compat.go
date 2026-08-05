package web

import (
	"encoding/json"
	"fmt"
	"strings"

	"m365-native/internal/chathub"
)

// responsesRequest is the OpenAI Responses API request subset supported by the gateway.
type responsesRequest struct {
	Model              string           `json:"model"`
	Input              any              `json:"input"`
	Tools              []map[string]any `json:"tools,omitempty"`
	ToolChoice         any              `json:"tool_choice,omitempty"`
	Stream             bool             `json:"stream,omitempty"`
	User               string           `json:"user,omitempty"`
	Reasoning          *reasoningConfig `json:"reasoning,omitempty"`
	PreviousResponseID string           `json:"previous_response_id,omitempty"`
	Conversation       string           `json:"conversation,omitempty"`
	NewConversation    bool             `json:"new_conversation,omitempty"`
}

func (r responsesRequest) openAI() (oaiReq, error) {
	o := oaiReq{Model: r.Model, Stream: r.Stream, ToolChoice: r.ToolChoice, User: r.User}
	if r.Reasoning != nil {
		o.Reasoning = r.Reasoning
		o.ReasoningEffort = r.Reasoning.Effort
	}
	switch v := r.Input.(type) {
	case string:
		if v == "" {
			return o, fmt.Errorf("input required")
		}
		o.Messages = []oaiMsg{{Role: "user", Content: v}}
	case []any:
		for _, raw := range v {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "function_call_progress":
				// Progress is deliberately not converted into an assistant/tool
				// message. It is transport metadata from a long-running client-side
				// executor and must not trigger a model turn or tool completion.
				if _, ok := parseToolProgress(m); !ok {
					return o, fmt.Errorf("invalid function_call_progress")
				}
				continue
			case "function_call_output":
				id, _ := m["call_id"].(string)
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: m["output"]})
			case "function_call":
				id, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				args := m["arguments"]
				if s, ok := args.(string); ok {
					var x any
					if json.Unmarshal([]byte(s), &x) == nil {
						args = x
					}
				}
				o.Messages = append(o.Messages, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": mustJSON(args)}}}})
			default:
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				// Responses API content items (input_text/input_image/
				// input_file/input_audio) carry their payload directly on the
				// item instead of a nested content array. Wrap the item so
				// parseContent can extract text and attachments.
				content := m["content"]
				if content == nil {
					content = []any{m}
				}
				o.Messages = append(o.Messages, oaiMsg{Role: role, Content: content})
			}
		}
	default:
		return o, fmt.Errorf("input must be string or array")
	}
	for _, t := range r.Tools {
		if typ, _ := t["type"].(string); typ != "function" {
			continue
		}
		f := map[string]any{"name": t["name"], "description": t["description"], "parameters": t["parameters"]}
		b, _ := json.Marshal(f)
		o.Tools = append(o.Tools, chathub.Tool{Type: "function", Function: b})
	}
	return o, nil
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}
type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}
type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens *int   `json:"budget_tokens,omitempty"`
}
type anthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}
type anthropicRequest struct {
	Model        string                 `json:"model"`
	System       any                    `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	Tools        []anthropicTool        `json:"tools,omitempty"`
	ToolChoice   any                    `json:"tool_choice,omitempty"`
	Stream       bool                   `json:"stream,omitempty"`
	MaxTokens    int                    `json:"max_tokens,omitempty"`
	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
}

func (r anthropicRequest) openAI() (oaiReq, error) {
	o := oaiReq{Model: r.Model, Stream: r.Stream}
	if effort, ok := r.thinkingEffort(); ok {
		o.ReasoningEffort = effort
		o.Reasoning = &reasoningConfig{Effort: effort}
	}
	if r.System != nil {
		o.Messages = append(o.Messages, oaiMsg{Role: "system", Content: r.System})
	}
	for _, m := range r.Messages {
		if s, ok := m.Content.(string); ok {
			o.Messages = append(o.Messages, oaiMsg{Role: m.Role, Content: s})
			continue
		}
		blocks, ok := m.Content.([]any)
		if !ok {
			return o, fmt.Errorf("invalid anthropic content")
		}
		var text []any
		var calls []map[string]any
		for _, raw := range blocks {
			b, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := b["type"].(string)
			switch typ {
			case "text":
				text = append(text, b)
			case "image":
				// Anthropic image blocks must not be silently dropped: forward
				// url or base64 sources as ChatHub attachments.
				if srcm, ok := b["source"].(map[string]any); ok {
					st, _ := srcm["type"].(string)
					switch st {
					case "url":
						if u, _ := srcm["url"].(string); u != "" {
							o.Attachments = append(o.Attachments, chathub.Attachment{Type: "image", URL: u, MimeType: "image/*"})
						}
					case "base64":
						mt, _ := srcm["media_type"].(string)
						if mt == "" {
							mt = "image/png"
						}
						if data, _ := srcm["data"].(string); data != "" {
							o.Attachments = append(o.Attachments, chathub.Attachment{Type: "image", URL: "data:" + mt + ";base64," + data, MimeType: mt})
						}
					}
				}
			case "tool_use":
				calls = append(calls, map[string]any{"id": b["id"], "type": "function", "function": map[string]any{"name": b["name"], "arguments": mustJSON(b["input"])}})
			case "tool_result":
				id, _ := b["tool_use_id"].(string)
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: b["content"]})
			}
		}
		if len(text) > 0 || len(calls) > 0 {
			o.Messages = append(o.Messages, oaiMsg{Role: m.Role, Content: text, ToolCalls: calls})
		}
	}
	for _, t := range r.Tools {
		f := map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}
		b, _ := json.Marshal(f)
		o.Tools = append(o.Tools, chathub.Tool{Type: "function", Function: b})
	}
	if c, ok := r.ToolChoice.(map[string]any); ok {
		switch c["type"] {
		case "auto":
			o.ToolChoice = "auto"
		case "any":
			o.ToolChoice = "required"
		case "none":
			o.ToolChoice = "none"
		case "tool":
			o.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": c["name"]}}
		}
	}
	return o, nil
}

// thinkingEffort maps the Anthropic thinking/output_config blocks that
// new-api and other OpenAI-compatible relays send after converting
// reasoning_effort. The gateway previously ignored these fields entirely,
// which silently downgraded every effort-annotated request to the base tone.
func (r anthropicRequest) thinkingEffort() (string, bool) {
	if r.OutputConfig != nil && strings.TrimSpace(r.OutputConfig.Effort) != "" {
		return r.OutputConfig.Effort, true
	}
	if r.Thinking == nil {
		return "", false
	}
	switch strings.TrimSpace(r.Thinking.Type) {
	case "enabled":
		if r.Thinking.BudgetTokens != nil {
			switch {
			case *r.Thinking.BudgetTokens <= 1600:
				return "low", true
			case *r.Thinking.BudgetTokens <= 3200:
				return "medium", true
			default:
				return "high", true
			}
		}
		return "medium", true
	case "adaptive":
		// Adaptive reasoning without an explicit effort still requires the
		// reasoning tone, so clients see thinking content in the stream.
		return "high", true
	}
	return "", false
}
