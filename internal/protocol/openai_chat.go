package protocol

import (
	"encoding/json"
	"net/http"
	"strings"

	"any-load/internal/models"
)

// openaiChatHandler is the identity handler: the IR is OpenAI Chat, so parsing
// and emitting are near-identity (marshal/unmarshal). It is selected as the
// upstream target when a group's UpstreamFormats includes openai-chat, and as
// the inbound handler when the client sends OpenAI Chat.
type openaiChatHandler struct{}

func init() {
	Register(&openaiChatHandler{})
}

func (h *openaiChatHandler) Format() string { return FormatOpenAIChat }

func (h *openaiChatHandler) InboundContentType() string { return "application/json" }

func (h *openaiChatHandler) UpstreamPath(model string, stream bool) (string, string) {
	return "/v1/chat/completions", ""
}

func (h *openaiChatHandler) ApplyAuth(req *http.Request, apiKey *models.APIKey) {
	req.Header.Set("Authorization", "Bearer "+apiKey.KeyValue)
}

// IsInboundStream reports stream intent for an OpenAI Chat inbound request
// (body `stream` field / Accept header / ?stream query). The path is unused.
func (h *openaiChatHandler) IsInboundStream(path string, body []byte) bool {
	return isBodyStreamFlag(body)
}

// ParseInboundRequest parses an OpenAI Chat Completions body into the IR.
// Supports text, images, and tool calling (definitions, calls, results).
func (h *openaiChatHandler) ParseInboundRequest(path string, body []byte) (*ChatRequest, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	model, _ := raw["model"].(string)
	ir := &ChatRequest{Model: model}

	// Tools (definitions).
	if tools, ok := raw["tools"].([]any); ok {
		for _, t := range tools {
			td, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := td["function"].(map[string]any)
			if fn == nil {
				continue
			}
			ir.Tools = append(ir.Tools, ToolDef{
				Name:        getString(fn, "name"),
				Description: getString(fn, "description"),
				Parameters:  rawJSON(fn["parameters"]),
			})
		}
	}
	ir.ToolChoice = normalizeToolChoiceOpenAI(raw["tool_choice"])

	messagesRaw, _ := raw["messages"].([]any)
	for _, m := range messagesRaw {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "system" {
			text, _, _, err := openAIContentParts(msg["content"])
			if err != nil {
				return nil, err
			}
			if ir.System == "" {
				ir.System = text
			} else if text != "" {
				ir.System += "\n\n" + text
			}
			continue
		}
		cm := ChatMessage{Role: role}
		if role == "tool" {
			cm.ToolCallID = getString(msg, "tool_call_id")
			cm.Name = getString(msg, "name")
		}
		text, parts, hasImage, err := openAIContentParts(msg["content"])
		if err != nil {
			return nil, err
		}
		if hasImage {
			cm.Parts = parts
		} else {
			cm.Content = text
		}
		// Assistant tool calls.
		if tcs, ok := msg["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				tcMap, ok := tc.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := tcMap["function"].(map[string]any)
				cm.ToolCalls = append(cm.ToolCalls, ToolCall{
					ID:        getString(tcMap, "id"),
					Name:      getString(fn, "name"),
					Arguments: json.RawMessage(getString(fn, "arguments")),
				})
			}
		}
		ir.Messages = append(ir.Messages, cm)
	}
	if t, ok := raw["temperature"].(float64); ok {
		ir.Temperature = &t
	}
	if p, ok := raw["top_p"].(float64); ok {
		ir.TopP = &p
	}
	if mt, ok := raw["max_tokens"].(float64); ok {
		v := int(mt)
		ir.MaxTokens = &v
	}
	if s, ok := raw["stream"].(bool); ok {
		ir.Stream = s
	}
	return ir, nil
}

// EmitUpstreamRequest emits the IR as an OpenAI Chat Completions request body.
func (h *openaiChatHandler) EmitUpstreamRequest(ir *ChatRequest) ([]byte, error) {
	out := map[string]any{
		"model":    ir.Model,
		"messages": buildOpenAIMessages(ir),
		"stream":   ir.Stream,
	}
	if len(ir.Tools) > 0 {
		tools := make([]map[string]any, 0, len(ir.Tools))
		for _, t := range ir.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  json.RawMessage(t.Parameters),
				},
			})
		}
		out["tools"] = tools
	}
	if tc := emitToolChoiceOpenAI(ir.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if ir.MaxTokens != nil {
		out["max_tokens"] = *ir.MaxTokens
	}
	if ir.Temperature != nil {
		out["temperature"] = *ir.Temperature
	}
	if ir.TopP != nil {
		out["top_p"] = *ir.TopP
	}
	return json.Marshal(out)
}

// ParseUpstreamResponse parses an OpenAI Chat non-stream response into the IR.
func (h *openaiChatHandler) ParseUpstreamResponse(body []byte) (*ChatResponse, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	ir := &ChatResponse{
		ID:    getString(raw, "id"),
		Model: getString(raw, "model"),
	}
	if choices, ok := raw["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			idx := toInt(choice["index"])
			msg, _ := choice["message"].(map[string]any)
			finish, _ := choice["finish_reason"].(string)
			cm := ChatMessage{Role: getString(msg, "role"), Content: getString(msg, "content")}
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcMap, _ := tc.(map[string]any)
					fn, _ := tcMap["function"].(map[string]any)
					cm.ToolCalls = append(cm.ToolCalls, ToolCall{
						ID:        getString(tcMap, "id"),
						Name:      getString(fn, "name"),
						Arguments: json.RawMessage(getString(fn, "arguments")),
					})
				}
			}
			ir.Choices = append(ir.Choices, ChatChoice{
				Index:        idx,
				Message:      cm,
				FinishReason: finish,
			})
		}
	}
	if usage, ok := raw["usage"].(map[string]any); ok {
		ir.PromptTokens = toInt(usage["prompt_tokens"])
		ir.CompletionTokens = toInt(usage["completion_tokens"])
	}
	return ir, nil
}

// EmitInboundResponse emits the IR as an OpenAI Chat non-stream response body.
func (h *openaiChatHandler) EmitInboundResponse(ir *ChatResponse) ([]byte, error) {
	choices := make([]map[string]any, 0, len(ir.Choices))
	for _, c := range ir.Choices {
		msg := map[string]any{"role": c.Message.Role, "content": c.Message.Content}
		if len(c.Message.ToolCalls) > 0 {
			tcs := make([]map[string]any, 0, len(c.Message.ToolCalls))
			for _, tc := range c.Message.ToolCalls {
				tcs = append(tcs, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": argsToString(tc.Arguments),
					},
				})
			}
			msg["tool_calls"] = tcs
		}
		choices = append(choices, map[string]any{
			"index":         c.Index,
			"message":       msg,
			"finish_reason": c.FinishReason,
		})
	}
	out := map[string]any{
		"id":      ir.ID,
		"object":  "chat.completion",
		"created": createdNow(),
		"model":   ir.Model,
		"choices": choices,
		"usage": map[string]any{
			"prompt_tokens":     ir.PromptTokens,
			"completion_tokens": ir.CompletionTokens,
			"total_tokens":      ir.PromptTokens + ir.CompletionTokens,
		},
	}
	return json.Marshal(out)
}

func (h *openaiChatHandler) NewUpstreamStreamParser() StreamParser {
	return &openaiChatStreamParser{}
}

func (h *openaiChatHandler) NewInboundStreamEmitter(model string) StreamEmitter {
	return &openaiChatStreamEmitter{model: model, created: createdNow()}
}

// openaiChatStreamParser converts OpenAI Chat SSE chunks into IR events.
type openaiChatStreamParser struct {
	done         bool
	toolCallSeen map[int]bool
}

func (p *openaiChatStreamParser) Parse(eventType string, data []byte) ([]StreamEvent, error) {
	if p.done {
		return nil, errEOF
	}
	s := string(data)
	if s == "[DONE]" {
		p.done = true
		return nil, errEOF
	}
	var chunk map[string]any
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, nil
	}
	var events []StreamEvent
	if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if role, ok := delta["role"].(string); ok && role != "" {
			events = append(events, StreamEvent{Type: EventRole, Delta: role})
		}
		if content, ok := delta["content"].(string); ok && content != "" {
			events = append(events, StreamEvent{Type: EventContent, Delta: content})
		}
		// Tool call deltas (parallel calls keyed by index).
		if tcs, ok := delta["tool_calls"].([]any); ok {
			if p.toolCallSeen == nil {
				p.toolCallSeen = make(map[int]bool)
			}
			for _, tc := range tcs {
				tcMap, ok := tc.(map[string]any)
				if !ok {
					continue
				}
				idx := toInt(tcMap["index"])
				if !p.toolCallSeen[idx] {
					p.toolCallSeen[idx] = true
					fn, _ := tcMap["function"].(map[string]any)
					events = append(events, StreamEvent{
						Type: EventToolCallStart, ToolCallIndex: idx,
						ToolCallID: getString(tcMap, "id"), ToolCallName: getString(fn, "name"),
					})
				}
				if fn, ok := tcMap["function"].(map[string]any); ok {
					if args, ok := fn["arguments"].(string); ok && args != "" {
						events = append(events, StreamEvent{
							Type: EventToolCallDelta, ToolCallIndex: idx, ArgumentsDelta: args,
						})
					}
				}
			}
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			// Close any open tool calls before the finish.
			for idx := range p.toolCallSeen {
				events = append(events, StreamEvent{Type: EventToolCallFinish, ToolCallIndex: idx})
			}
			p.toolCallSeen = nil
			events = append(events, StreamEvent{Type: EventFinish, FinishReason: fr})
		}
	}
	if usage, ok := chunk["usage"].(map[string]any); ok {
		events = append(events, StreamEvent{
			Type:             EventUsage,
			PromptTokens:     toInt(usage["prompt_tokens"]),
			CompletionTokens: toInt(usage["completion_tokens"]),
		})
	}
	return events, nil
}

// openaiChatStreamEmitter converts IR events into OpenAI Chat SSE chunks.
type openaiChatStreamEmitter struct {
	model    string
	created  int64
	id       string
	roleSent bool
}

func (e *openaiChatStreamEmitter) Emit(ev StreamEvent) ([]StreamChunk, error) {
	switch ev.Type {
	case EventRole:
		e.roleSent = true
		return []StreamChunk{{Data: e.chunk(map[string]any{"role": ev.Delta}, "")}}, nil
	case EventContent:
		delta := map[string]any{"content": ev.Delta}
		if !e.roleSent {
			e.roleSent = true
			return []StreamChunk{
				{Data: e.chunk(map[string]any{"role": "assistant"}, "")},
				{Data: e.chunk(delta, "")},
			}, nil
		}
		return []StreamChunk{{Data: e.chunk(delta, "")}}, nil
	case EventToolCallStart:
		tc := map[string]any{
			"index": ev.ToolCallIndex, "id": ev.ToolCallID, "type": "function",
			"function": map[string]any{"name": ev.ToolCallName, "arguments": ""},
		}
		return []StreamChunk{{Data: e.chunk(map[string]any{"tool_calls": []any{tc}}, "")}}, nil
	case EventToolCallDelta:
		tc := map[string]any{
			"index": ev.ToolCallIndex,
			"function": map[string]any{"arguments": ev.ArgumentsDelta},
		}
		return []StreamChunk{{Data: e.chunk(map[string]any{"tool_calls": []any{tc}}, "")}}, nil
	case EventToolCallFinish:
		// OpenAI has no per-call close event; nothing to emit.
		return nil, nil
	case EventFinish:
		return []StreamChunk{{Data: e.chunk(map[string]any{}, ev.FinishReason)}}, nil
	case EventUsage:
		obj := map[string]any{
			"id": e.id, "object": "chat.completion.chunk",
			"created": e.created, "model": e.model, "choices": []any{},
			"usage": map[string]any{
				"prompt_tokens":     ev.PromptTokens,
				"completion_tokens": ev.CompletionTokens,
				"total_tokens":      ev.PromptTokens + ev.CompletionTokens,
			},
		}
		b, _ := json.Marshal(obj)
		return []StreamChunk{{Data: b}}, nil
	}
	return nil, nil
}

func (e *openaiChatStreamEmitter) Done() []StreamChunk {
	return []StreamChunk{{Data: []byte("[DONE]")}}
}

func (e *openaiChatStreamEmitter) chunk(delta map[string]any, finishReason string) []byte {
	choice := map[string]any{"index": 0, "delta": delta}
	if finishReason != "" {
		choice["finish_reason"] = finishReason
	} else {
		choice["finish_reason"] = nil
	}
	obj := map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
		"choices": []map[string]any{choice},
	}
	b, _ := json.Marshal(obj)
	return b
}

// buildOpenAIMessages reconstructs OpenAI messages (with system) from the IR.
func buildOpenAIMessages(ir *ChatRequest) []map[string]any {
	msgs := make([]map[string]any, 0, len(ir.Messages)+1)
	if ir.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": ir.System})
	}
	for _, m := range ir.Messages {
		mm := map[string]any{"role": m.Role}
		switch {
		case m.Role == "tool":
			mm["content"] = m.Content
			mm["tool_call_id"] = m.ToolCallID
			if m.Name != "" {
				mm["name"] = m.Name
			}
		case len(m.Parts) > 0:
			mm["content"] = buildOpenAIContentParts(m.Parts)
		default:
			mm["content"] = m.Content
		}
		if len(m.ToolCalls) > 0 {
			tcs := make([]map[string]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": argsToString(tc.Arguments),
					},
				})
			}
			mm["tool_calls"] = tcs
		}
		msgs = append(msgs, mm)
	}
	return msgs
}

// buildOpenAIContentParts converts IR Parts to an OpenAI content array
// ({type:"text",text} and {type:"image_url",image_url:{url}}).
func buildOpenAIContentParts(parts []ContentPart) []map[string]any {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case "image":
			url := p.Image.URL
			if url == "" && p.Image.Base64 != "" {
				mime := p.Image.MIMEType
				if mime == "" {
					mime = "image/png"
				}
				url = "data:" + mime + ";base64," + p.Image.Base64
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		}
	}
	return out
}

// openAIContentParts parses an OpenAI content field (string or array of parts)
// into text (concatenated), IR Parts (when images present), and hasImage.
// Non-text, non-image parts are rejected.
func openAIContentParts(content any) (text string, parts []ContentPart, hasImage bool, err error) {
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
			case "text":
				if text, ok := p["text"].(string); ok {
					sb.WriteString(text)
					parts = append(parts, ContentPart{Type: "text", Text: text})
				}
			case "image_url":
				imgURL, _ := p["image_url"].(map[string]any)
				url, _ := imgURL["url"].(string)
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
		// If only text parts (no image), collapse to a single text string and
		// drop parts so callers use the fast path.
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
