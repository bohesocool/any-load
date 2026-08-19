package protocol

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"any-load/internal/models"
)

// openaiResponseHandler converts between the OpenAI Responses API (/v1/responses)
// and the chat IR, in both directions and for stream + non-stream. The
// Responses API uses `input` (string or message array), `instructions` (system),
// `max_output_tokens`, and reports output via `output[]` message items with
// `output_text` content parts.
type openaiResponseHandler struct{}

func init() {
	Register(&openaiResponseHandler{})
}

func (h *openaiResponseHandler) Format() string             { return FormatOpenAIResponse }
func (h *openaiResponseHandler) InboundContentType() string { return "application/json" }

// IsInboundStream reports stream intent for a Responses inbound request (body
// `stream` field).
func (h *openaiResponseHandler) IsInboundStream(path string, body []byte) bool {
	return isBodyStreamFlag(body)
}

func (h *openaiResponseHandler) UpstreamPath(model string, stream bool) (string, string) {
	return "/v1/responses", ""
}

func (h *openaiResponseHandler) ApplyAuth(req *http.Request, apiKey *models.APIKey) {
	req.Header.Set("Authorization", "Bearer "+apiKey.KeyValue)
}

// --- Inbound direction (Responses client → IR) ---

// ParseInboundRequest parses a Responses API request body into the IR. The
// `input` field may be a string or an array of {role, content} messages.
func (h *openaiResponseHandler) ParseInboundRequest(path string, body []byte) (*ChatRequest, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	ir := &ChatRequest{Model: getString(raw, "model")}
	if instr, ok := raw["instructions"].(string); ok {
		ir.System = instr
	}
	// Tools (flatter form: {type:"function", name, description, parameters}).
	if tools, ok := raw["tools"].([]any); ok {
		for _, t := range tools {
			td, ok := t.(map[string]any)
			if !ok {
				continue
			}
			ir.Tools = append(ir.Tools, ToolDef{
				Name:        getString(td, "name"),
				Description: getString(td, "description"),
				Parameters:  rawJSON(td["parameters"]),
			})
		}
	}
	ir.ToolChoice = normalizeToolChoiceOpenAI(raw["tool_choice"])
	if t, ok := raw["temperature"].(float64); ok {
		ir.Temperature = &t
	}
	if p, ok := raw["top_p"].(float64); ok {
		ir.TopP = &p
	}
	if mt, ok := raw["max_output_tokens"].(float64); ok {
		v := int(mt)
		ir.MaxTokens = &v
	}
	if s, ok := raw["stream"].(bool); ok {
		ir.Stream = s
	}

	switch input := raw["input"].(type) {
	case string:
		ir.Messages = []ChatMessage{{Role: "user", Content: input}}
	case []any:
		for _, m := range input {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := msg["type"].(string)
			switch itemType {
			case "function_call":
				// A prior assistant tool call.
				ir.Messages = append(ir.Messages, ChatMessage{
					Role: "assistant",
					ToolCalls: []ToolCall{{
						ID:        getString(msg, "call_id"),
						Name:      getString(msg, "name"),
						Arguments: json.RawMessage(getString(msg, "arguments")),
					}},
				})
			case "function_call_output":
				// A tool result.
				ir.Messages = append(ir.Messages, ChatMessage{
					Role:       "tool",
					ToolCallID: getString(msg, "call_id"),
					Content:    getString(msg, "output"),
				})
			default:
				role, _ := msg["role"].(string)
				text, parts, hasImage, err := responsesContentParts(msg["content"])
				if err != nil {
					return nil, err
				}
				if role == "system" || role == "developer" {
					if ir.System == "" {
						ir.System = text
					} else if text != "" {
						ir.System += "\n\n" + text
					}
					continue
				}
				cm := ChatMessage{Role: role}
				if hasImage {
					cm.Parts = parts
				} else {
					cm.Content = text
				}
				ir.Messages = append(ir.Messages, cm)
			}
		}
	case nil:
		return nil, errors.New("input is required")
	}
	return ir, nil
}

// EmitInboundResponse emits the IR as a Responses API non-stream response body.
func (h *openaiResponseHandler) EmitInboundResponse(ir *ChatResponse) ([]byte, error) {
	text := ""
	var toolCalls []ToolCall
	if len(ir.Choices) > 0 {
		text = ir.Choices[0].Message.Content
		toolCalls = ir.Choices[0].Message.ToolCalls
	}
	output := []map[string]any{}
	if text != "" || len(toolCalls) == 0 {
		output = append(output, map[string]any{
			"type": "message", "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": text}},
		})
	}
	for _, tc := range toolCalls {
		output = append(output, map[string]any{
			"type":      "function_call",
			"call_id":   tc.ID,
			"name":      tc.Name,
			"arguments": argsToString(tc.Arguments),
		})
	}
	out := map[string]any{
		"id":     ir.ID,
		"object": "response",
		"status": "completed",
		"model":  ir.Model,
		"output": output,
		"usage": map[string]any{
			"input_tokens":  ir.PromptTokens,
			"output_tokens": ir.CompletionTokens,
			"total_tokens":  ir.PromptTokens + ir.CompletionTokens,
		},
	}
	return json.Marshal(out)
}

// NewInboundStreamEmitter returns an emitter producing Responses SSE events.
func (h *openaiResponseHandler) NewInboundStreamEmitter(model string) StreamEmitter {
	return &responsesStreamEmitter{model: model, created: createdNow()}
}

// --- Upstream direction (IR → Responses upstream) ---

// EmitUpstreamRequest emits the IR as a Responses API request body.
func (h *openaiResponseHandler) EmitUpstreamRequest(ir *ChatRequest) ([]byte, error) {
	if len(ir.Messages) == 0 {
		return nil, errors.New("at least one message is required")
	}
	out := map[string]any{
		"model":  ir.Model,
		"input":  buildResponsesInput(ir),
		"stream": ir.Stream,
	}
	if ir.System != "" {
		out["instructions"] = ir.System
	}
	if len(ir.Tools) > 0 {
		tools := make([]map[string]any, 0, len(ir.Tools))
		for _, t := range ir.Tools {
			tool := map[string]any{"type": "function", "name": t.Name, "description": t.Description}
			if len(t.Parameters) > 0 {
				tool["parameters"] = json.RawMessage(t.Parameters)
			}
			tools = append(tools, tool)
		}
		out["tools"] = tools
	}
	if tc := emitToolChoiceOpenAI(ir.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if ir.MaxTokens != nil {
		out["max_output_tokens"] = *ir.MaxTokens
	}
	if ir.Temperature != nil {
		out["temperature"] = *ir.Temperature
	}
	if ir.TopP != nil {
		out["top_p"] = *ir.TopP
	}
	return json.Marshal(out)
}

// ParseUpstreamResponse parses a Responses API non-stream response into the IR.
func (h *openaiResponseHandler) ParseUpstreamResponse(body []byte) (*ChatResponse, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	ir := &ChatResponse{
		ID:    getString(raw, "id"),
		Model: getString(raw, "model"),
	}
	text := extractResponsesOutputText(raw["output"])
	finish := mapResponsesStatusToFinish(getString(raw, "status"))
	var toolCalls []ToolCall
	if items, ok := raw["output"].([]any); ok {
		for _, it := range items {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if getString(item, "type") == "function_call" {
				toolCalls = append(toolCalls, ToolCall{
					ID:        getString(item, "call_id"),
					Name:      getString(item, "name"),
					Arguments: json.RawMessage(getString(item, "arguments")),
				})
			}
		}
	}
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	ir.Choices = []ChatChoice{{
		Index:        0,
		Message:      ChatMessage{Role: "assistant", Content: text, ToolCalls: toolCalls},
		FinishReason: finish,
	}}
	if usage, ok := raw["usage"].(map[string]any); ok {
		ir.PromptTokens = toInt(usage["input_tokens"])
		ir.CompletionTokens = toInt(usage["output_tokens"])
	}
	return ir, nil
}

// NewUpstreamStreamParser returns a parser for Responses SSE events.
func (h *openaiResponseHandler) NewUpstreamStreamParser() StreamParser {
	return &responsesStreamParser{roleSent: false}
}

// --- Responses stream parser (upstream SSE → IR events) ---

type responsesStreamParser struct {
	roleSent    bool
	finish      string
	promptTokens int
	outputTokens int
}

func (p *responsesStreamParser) Parse(eventType string, data []byte) ([]StreamEvent, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var ev map[string]any
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, nil
	}
	et, _ := ev["type"].(string)
	if et == "" {
		et = eventType
	}

	var events []StreamEvent
	switch et {
	case "response.created", "response.in_progress":
		if !p.roleSent {
			p.roleSent = true
			events = append(events, StreamEvent{Type: EventRole, Delta: "assistant"})
		}
	case "response.output_item.added":
		if !p.roleSent {
			p.roleSent = true
			events = append(events, StreamEvent{Type: EventRole, Delta: "assistant"})
		}
		if item, ok := ev["item"].(map[string]any); ok {
			if getString(item, "type") == "function_call" {
				idx := toInt(ev["output_index"])
				events = append(events, StreamEvent{
					Type: EventToolCallStart, ToolCallIndex: idx,
					ToolCallID: getString(item, "call_id"), ToolCallName: getString(item, "name"),
				})
			}
		}
	case "response.function_call_arguments.delta":
		if d, ok := ev["delta"].(string); ok && d != "" {
			events = append(events, StreamEvent{
				Type: EventToolCallDelta, ToolCallIndex: toInt(ev["output_index"]),
				ArgumentsDelta: d,
			})
		}
	case "response.output_item.done":
		if item, ok := ev["item"].(map[string]any); ok {
			if getString(item, "type") == "function_call" {
				events = append(events, StreamEvent{
					Type: EventToolCallFinish, ToolCallIndex: toInt(ev["output_index"]),
				})
			}
		}
	case "response.content_part.added":
		if !p.roleSent {
			p.roleSent = true
			events = append(events, StreamEvent{Type: EventRole, Delta: "assistant"})
		}
	case "response.output_text.delta":
		if !p.roleSent {
			p.roleSent = true
			events = append(events, StreamEvent{Type: EventRole, Delta: "assistant"})
		}
		if d, ok := ev["delta"].(string); ok && d != "" {
			events = append(events, StreamEvent{Type: EventContent, Delta: d})
		}
	case "response.completed":
		if !p.roleSent {
			p.roleSent = true
			events = append(events, StreamEvent{Type: EventRole, Delta: "assistant"})
		}
		if resp, ok := ev["response"].(map[string]any); ok {
			p.finish = mapResponsesStatusToFinish(getString(resp, "status"))
			if usage, ok := resp["usage"].(map[string]any); ok {
				p.promptTokens = toInt(usage["input_tokens"])
				p.outputTokens = toInt(usage["output_tokens"])
			}
		}
		finish := p.finish
		if finish == "" {
			finish = "stop"
		}
		events = append(events, StreamEvent{Type: EventFinish, FinishReason: finish})
		events = append(events, StreamEvent{
			Type: EventUsage, PromptTokens: p.promptTokens, CompletionTokens: p.outputTokens,
		})
		return events, errEOF
	case "response.failed", "response.incomplete":
		if !p.roleSent {
			p.roleSent = true
			events = append(events, StreamEvent{Type: EventRole, Delta: "assistant"})
		}
		finish := "stop"
		if et == "response.incomplete" {
			finish = "length"
		}
		events = append(events, StreamEvent{Type: EventFinish, FinishReason: finish})
		events = append(events, StreamEvent{Type: EventUsage, PromptTokens: p.promptTokens, CompletionTokens: p.outputTokens})
		return events, errEOF
	}
	return events, nil
}

// --- Responses stream emitter (IR events → Responses SSE) ---

type responsesStreamEmitter struct {
	id           string
	model        string
	created      int64
	roleSent     bool
	itemID       string
	contentIndex int
	finishReason string
	promptTokens int
	outputTokens int
	nextToolIdx  int // next output_index for a function_call item
}

func (e *responsesStreamEmitter) Emit(ev StreamEvent) ([]StreamChunk, error) {
	switch ev.Type {
	case EventRole:
		if !e.roleSent {
			e.roleSent = true
			return []StreamChunk{e.createdEvent(), e.outputItemAdded(), e.contentPartAdded()}, nil
		}
	case EventContent:
		var out []StreamChunk
		if !e.roleSent {
			e.roleSent = true
			out = append(out, e.createdEvent(), e.outputItemAdded(), e.contentPartAdded())
		}
		out = append(out, e.textDelta(ev.Delta))
		return out, nil
	case EventToolCallStart:
		// Tool calls get their own output_index (after the text item at 0).
		e.nextToolIdx++
		idx := e.nextToolIdx
		return []StreamChunk{e.event("response.output_item.added", map[string]any{
			"type": "response.output_item.added",
			"output_index": idx,
			"item": map[string]any{
				"type": "function_call", "call_id": ev.ToolCallID,
				"name": ev.ToolCallName, "arguments": "",
			},
		})}, nil
	case EventToolCallDelta:
		// Map the IR tool-call index back to our allocated output_index.
		idx := ev.ToolCallIndex + 1
		return []StreamChunk{e.event("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta",
			"output_index": idx, "delta": ev.ArgumentsDelta,
		})}, nil
	case EventToolCallFinish:
		idx := ev.ToolCallIndex + 1
		return []StreamChunk{e.event("response.output_item.done", map[string]any{
			"type": "response.output_item.done",
			"output_index": idx,
			"item": map[string]any{
				"type": "function_call", "call_id": "", "name": "", "arguments": "",
			},
		})}, nil
	case EventFinish:
		e.finishReason = ev.FinishReason
		return nil, nil
	case EventUsage:
		e.promptTokens = ev.PromptTokens
		e.outputTokens = ev.CompletionTokens
		return nil, nil
	}
	return nil, nil
}

// Done emits the terminal events: response.output_text.done,
// response.content_part.done, response.output_item.done, response.completed.
func (e *responsesStreamEmitter) Done() []StreamChunk {
	var out []StreamChunk
	if !e.roleSent {
		out = append(out, e.createdEvent(), e.outputItemAdded(), e.contentPartAdded())
	}
	out = append(out,
		e.textDone(),
		e.contentPartDone(),
		e.outputItemDone(),
		e.completed(),
	)
	return out
}

func (e *responsesStreamEmitter) createdEvent() StreamChunk {
	return e.event("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": e.id, "object": "response", "status": "in_progress", "model": e.model,
		},
	})
}

func (e *responsesStreamEmitter) outputItemAdded() StreamChunk {
	return e.event("response.output_item.added", map[string]any{
		"type": "response.output_item.added",
		"output_index": 0,
		"item": map[string]any{
			"id": e.id, "type": "message", "role": "assistant", "status": "in_progress",
			"content": []any{},
		},
	})
}

func (e *responsesStreamEmitter) contentPartAdded() StreamChunk {
	return e.event("response.content_part.added", map[string]any{
		"type": "response.content_part.added",
		"output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": ""},
	})
}

func (e *responsesStreamEmitter) textDelta(text string) StreamChunk {
	return e.event("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta",
		"output_index": 0, "content_index": 0,
		"delta": text,
	})
}

func (e *responsesStreamEmitter) textDone() StreamChunk {
	return e.event("response.output_text.done", map[string]any{
		"type": "response.output_text.done",
		"output_index": 0, "content_index": 0,
		"text": "",
	})
}

func (e *responsesStreamEmitter) contentPartDone() StreamChunk {
	return e.event("response.content_part.done", map[string]any{
		"type": "response.content_part.done",
		"output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": ""},
	})
}

func (e *responsesStreamEmitter) outputItemDone() StreamChunk {
	return e.event("response.output_item.done", map[string]any{
		"type": "response.output_item.done",
		"output_index": 0,
		"item": map[string]any{
			"id": e.id, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{},
		},
	})
}

func (e *responsesStreamEmitter) completed() StreamChunk {
	status := "completed"
	if e.finishReason == "length" {
		status = "incomplete"
	}
	return e.event("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": e.id, "object": "response", "status": status, "model": e.model,
			"output": []any{},
			"usage": map[string]any{
				"input_tokens":  e.promptTokens,
				"output_tokens": e.outputTokens,
				"total_tokens":  e.promptTokens + e.outputTokens,
			},
		},
	})
}

func (e *responsesStreamEmitter) event(eventType string, payload map[string]any) StreamChunk {
	b, _ := json.Marshal(payload)
	return StreamChunk{Event: eventType, Data: b}
}

// --- helpers ---

// responsesContentText extracts text from a Responses message content field.
// Content may be a string or an array of parts with type input_text/output_text.
func responsesContentText(content any) (string, error) {
	text, _, _, err := responsesContentParts(content)
	return text, err
}

// responsesContentParts parses a Responses message content field (string or
// array of parts) into text, IR Parts (when images present), and hasImage.
// input_image parts become image Parts; input_text/output_text/text become
// text.
func responsesContentParts(content any) (text string, parts []ContentPart, hasImage bool, err error) {
	switch c := content.(type) {
	case string:
		return c, nil, false, nil
	case []any:
		var sb strings.Builder
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok {
				return "", nil, false, errInvalidContent
			}
			switch t, _ := p["type"].(string); t {
			case "input_text", "output_text", "text":
				if text, ok := p["text"].(string); ok {
					sb.WriteString(text)
					parts = append(parts, ContentPart{Type: "text", Text: text})
				}
			case "input_image":
				url, _ := p["image_url"].(string)
				img := &Image{URL: url}
				if mime, b64, ok := parseDataURI(url); ok {
					img = &Image{Base64: b64, MIMEType: mime}
				}
				parts = append(parts, ContentPart{Type: "image", Image: img})
				hasImage = true
			default:
				return "", nil, false, errMultimodalUnsupported
			}
		}
		if !hasImage {
			return sb.String(), nil, false, nil
		}
		return sb.String(), parts, true, nil
	case nil:
		return "", nil, false, nil
	default:
		return "", nil, false, errInvalidContent
	}
}

// extractResponsesOutputText concatenates output_text parts from a Responses
// response's output array (the first message item).
func extractResponsesOutputText(output any) string {
	items, ok := output.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		return ""
	}
	content, _ := item["content"].([]any)
	var sb strings.Builder
	for _, part := range content {
		p, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := p["type"].(string); t == "output_text" || t == "text" {
			if text, ok := p["text"].(string); ok {
				sb.WriteString(text)
			}
		}
	}
	return sb.String()
}

// buildResponsesInput converts IR messages to a Responses `input` array.
// Assistant tool calls become top-level function_call items; role="tool"
// messages become function_call_output items; text/image messages become
// message items.
func buildResponsesInput(ir *ChatRequest) []map[string]any {
	out := make([]map[string]any, 0, len(ir.Messages))
	for _, m := range ir.Messages {
		switch {
		case m.Role == "tool":
			out = append(out, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  m.Content,
			})
		case len(m.ToolCalls) > 0:
			for _, tc := range m.ToolCalls {
				out = append(out, map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Name,
					"arguments": argsToString(tc.Arguments),
				})
			}
			// If the assistant message also has text content, emit a message item too.
			if m.Content != "" || len(m.Parts) > 0 {
				out = append(out, responsesMessageItem(m))
			}
		default:
			out = append(out, responsesMessageItem(m))
		}
	}
	return out
}

// responsesMessageItem builds a Responses `message` input item from a text/image
// IR message.
func responsesMessageItem(m ChatMessage) map[string]any {
	content := []map[string]any{}
	if len(m.Parts) > 0 {
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				content = append(content, map[string]any{"type": "input_text", "text": p.Text})
			case "image":
				url := p.Image.URL
				if url == "" && p.Image.Base64 != "" {
					mime := p.Image.MIMEType
					if mime == "" {
						mime = "image/png"
					}
					url = "data:" + mime + ";base64," + p.Image.Base64
				}
				content = append(content, map[string]any{"type": "input_image", "image_url": url})
			}
		}
	} else {
		content = append(content, map[string]any{"type": "input_text", "text": m.Content})
	}
	return map[string]any{"type": "message", "role": m.Role, "content": content}
}

func mapResponsesStatusToFinish(status string) string {
	switch status {
	case "completed":
		return "stop"
	case "incomplete":
		return "length"
	case "failed":
		return "stop"
	default:
		if status == "" {
			return "stop"
		}
		return "stop"
	}
}

// mapChatFinishToResponsesStatus maps IR finish reasons to a Responses status.
func mapChatFinishToResponsesStatus(r string) string {
	switch r {
	case "length":
		return "incomplete"
	default:
		return "completed"
	}
}
