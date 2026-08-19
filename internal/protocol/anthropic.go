package protocol

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"any-load/internal/models"
)

const anthropicAPIVersion = "2023-06-01"

// anthropicHandler converts between Anthropic Messages and the chat IR, in
// both directions (inbound and upstream) and for stream + non-stream.
type anthropicHandler struct{}

func init() {
	Register(&anthropicHandler{})
}

func (h *anthropicHandler) Format() string          { return FormatAnthropic }
func (h *anthropicHandler) InboundContentType() string { return "application/json" }

// IsInboundStream reports stream intent for an Anthropic Messages inbound
// request (body `stream` field / Accept header / ?stream query).
func (h *anthropicHandler) IsInboundStream(path string, body []byte) bool {
	return isBodyStreamFlag(body)
}

func (h *anthropicHandler) UpstreamPath(model string, stream bool) (string, string) {
	return "/v1/messages", ""
}

func (h *anthropicHandler) ApplyAuth(req *http.Request, apiKey *models.APIKey) {
	req.Header.Set("x-api-key", apiKey.KeyValue)
	req.Header.Set("anthropic-version", anthropicAPIVersion)
}

// --- Inbound direction (Anthropic Messages client → IR) ---

// ParseInboundRequest parses an Anthropic Messages request body into the IR.
func (h *anthropicHandler) ParseInboundRequest(path string, body []byte) (*ChatRequest, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	ir := &ChatRequest{
		Model: getString(raw, "model"),
	}
	if sys, ok := raw["system"].(string); ok {
		ir.System = sys
	}
	if sysBlocks, ok := raw["system"].([]any); ok {
		ir.System = joinAnthropicTextBlocks(sysBlocks)
	}
	// Tools (definitions).
	if tools, ok := raw["tools"].([]any); ok {
		for _, t := range tools {
			td, ok := t.(map[string]any)
			if !ok {
				continue
			}
			ir.Tools = append(ir.Tools, ToolDef{
				Name:        getString(td, "name"),
				Description: getString(td, "description"),
				Parameters:  rawJSON(td["input_schema"]),
			})
		}
	}
	ir.ToolChoice = normalizeToolChoiceAnthropic(raw["tool_choice"])
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

	messagesRaw, _ := raw["messages"].([]any)
	for _, m := range messagesRaw {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		cm := ChatMessage{Role: role}
		if err := parseAnthropicContent(msg["content"], &cm, &ir.Messages); err != nil {
			return nil, err
		}
		ir.Messages = append(ir.Messages, cm)
	}
	return ir, nil
}

// EmitInboundResponse emits the IR as an Anthropic Messages non-stream
// response body (what an Anthropic-format client expects).
func (h *anthropicHandler) EmitInboundResponse(ir *ChatResponse) ([]byte, error) {
	text := ""
	finishReason := "end_turn"
	var toolCalls []ToolCall
	if len(ir.Choices) > 0 {
		text = ir.Choices[0].Message.Content
		toolCalls = ir.Choices[0].Message.ToolCalls
		if ir.Choices[0].FinishReason != "" {
			finishReason = mapChatFinishToAnthropic(ir.Choices[0].FinishReason)
		}
	}
	content := []map[string]any{}
	if text != "" || len(toolCalls) == 0 {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, tc := range toolCalls {
		input, _ := argsToObject(tc.Arguments)
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Name,
			"input": input,
		})
	}
	out := map[string]any{
		"id":            ir.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         ir.Model,
		"content":       content,
		"stop_reason":   finishReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  ir.PromptTokens,
			"output_tokens": ir.CompletionTokens,
		},
	}
	return json.Marshal(out)
}

func (h *anthropicHandler) NewInboundStreamEmitter(model string) StreamEmitter {
	return &anthropicStreamEmitter{model: model}
}

// --- Upstream direction (IR → Anthropic upstream) ---

// EmitUpstreamRequest emits the IR as an Anthropic Messages request body.
func (h *anthropicHandler) EmitUpstreamRequest(ir *ChatRequest) ([]byte, error) {
	if len(ir.Messages) == 0 {
		return nil, errors.New("at least one non-system message is required")
	}
	out := map[string]any{
		"model":    ir.Model,
		"messages": buildAnthropicMessages(ir.Messages),
	}
	if ir.System != "" {
		out["system"] = ir.System
	}
	if len(ir.Tools) > 0 {
		tools := make([]map[string]any, 0, len(ir.Tools))
		for _, t := range ir.Tools {
			tools = append(tools, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"input_schema": json.RawMessage(t.Parameters),
			})
		}
		out["tools"] = tools
	}
	if tc := emitToolChoiceAnthropic(ir.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if ir.MaxTokens != nil {
		out["max_tokens"] = *ir.MaxTokens
	} else {
		out["max_tokens"] = 4096 // Anthropic requires max_tokens
	}
	if ir.Temperature != nil {
		out["temperature"] = *ir.Temperature
	}
	if ir.TopP != nil {
		out["top_p"] = *ir.TopP
	}
	if ir.Stream {
		out["stream"] = true
	}
	return json.Marshal(out)
}

// ParseUpstreamResponse parses an Anthropic Messages non-stream response into
// the IR.
func (h *anthropicHandler) ParseUpstreamResponse(body []byte) (*ChatResponse, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	ir := &ChatResponse{
		ID:    getString(raw, "id"),
		Model: getString(raw, "model"),
	}
	text := extractAnthropicText(raw["content"])
	finish := mapAnthropicStopReason(getString(raw, "stop_reason"))
	cm := ChatMessage{Role: "assistant", Content: text}
	if blocks, ok := raw["content"].([]any); ok {
		for _, b := range blocks {
			blk, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if getString(blk, "type") == "tool_use" {
				input, _ := json.Marshal(blk["input"])
				cm.ToolCalls = append(cm.ToolCalls, ToolCall{
					ID:        getString(blk, "id"),
					Name:      getString(blk, "name"),
					Arguments: json.RawMessage(input),
				})
			}
		}
	}
	ir.Choices = []ChatChoice{{
		Index:        0,
		Message:      cm,
		FinishReason: finish,
	}}
	if usage, ok := raw["usage"].(map[string]any); ok {
		ir.PromptTokens = toInt(usage["input_tokens"])
		ir.CompletionTokens = toInt(usage["output_tokens"])
	}
	return ir, nil
}

func (h *anthropicHandler) NewUpstreamStreamParser() StreamParser {
	return &anthropicStreamParser{}
}

// --- Anthropic stream parser (upstream SSE → IR events) ---

type anthropicStreamParser struct {
	finishReason string
	promptTokens int
	outputTokens int
	terminated  bool
	roleSent    bool
	toolBlocks  map[int]bool
}

func (p *anthropicStreamParser) Parse(eventType string, data []byte) ([]StreamEvent, error) {
	if p.terminated {
		return nil, errEOF
	}
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
	ensureRole := func() {
		if !p.roleSent {
			p.roleSent = true
			events = append(events, StreamEvent{Type: EventRole, Delta: "assistant"})
		}
	}
	switch et {
	case "message_start":
		if msg, ok := ev["message"].(map[string]any); ok {
			if usage, ok := msg["usage"].(map[string]any); ok {
				p.promptTokens = toInt(usage["input_tokens"])
				p.outputTokens = toInt(usage["output_tokens"])
			}
		}
		ensureRole()

	case "content_block_start":
		ensureRole()
		if blk, ok := ev["content_block"].(map[string]any); ok {
			if getString(blk, "type") == "tool_use" {
				idx := toInt(ev["index"])
				if p.toolBlocks == nil {
					p.toolBlocks = make(map[int]bool)
				}
				p.toolBlocks[idx] = true
				events = append(events, StreamEvent{
					Type: EventToolCallStart, ToolCallIndex: idx,
					ToolCallID: getString(blk, "id"), ToolCallName: getString(blk, "name"),
				})
			}
		}

	case "content_block_delta":
		if delta, ok := ev["delta"].(map[string]any); ok {
			switch dt, _ := delta["type"].(string); dt {
			case "text_delta":
				if text, _ := delta["text"].(string); text != "" {
					ensureRole()
					events = append(events, StreamEvent{Type: EventContent, Delta: text})
				}
			case "input_json_delta":
				if pj, _ := delta["partial_json"].(string); pj != "" {
					events = append(events, StreamEvent{
						Type: EventToolCallDelta, ToolCallIndex: toInt(ev["index"]),
						ArgumentsDelta: pj,
					})
				}
			}
		}

	case "content_block_stop":
		// If this block index was a tool call, emit a tool_call_finish. We
		// can't tell from this event alone; track tool-call indices instead.
		if idx := toInt(ev["index"]); p.isToolBlock(idx) {
			events = append(events, StreamEvent{Type: EventToolCallFinish, ToolCallIndex: idx})
		}

	case "message_delta":
		if delta, ok := ev["delta"].(map[string]any); ok {
			p.finishReason = mapAnthropicStopReason(getString(delta, "stop_reason"))
		}
		if usage, ok := ev["usage"].(map[string]any); ok {
			if ot := toInt(usage["output_tokens"]); ot > 0 {
				p.outputTokens = ot
			}
		}

	case "message_stop":
		finish := p.finishReason
		if finish == "" {
			finish = "stop"
		}
		events = append(events, StreamEvent{Type: EventFinish, FinishReason: finish})
		events = append(events, StreamEvent{
			Type:             EventUsage,
			PromptTokens:     p.promptTokens,
			CompletionTokens: p.outputTokens,
		})
		p.terminated = true
		return events, errEOF
	}
	return events, nil
}

// isToolBlock reports whether a content block index was a tool_use. Tracked by
// content_block_start; tool blocks use indices distinct from the text block.
func (p *anthropicStreamParser) isToolBlock(idx int) bool {
	// Anthropic assigns block indices sequentially; the first text block is
	// index 0, tool_use blocks follow. Track them via a small set.
	if p.toolBlocks == nil {
		return false
	}
	_, ok := p.toolBlocks[idx]
	return ok
}

// --- Anthropic stream emitter (IR events → Anthropic SSE) ---

type anthropicStreamEmitter struct {
	model        string
	id           string
	roleSent     bool
	blockStarted bool // text block (index 0) started
	textOpen     bool // text block currently open
	nextBlockIdx int  // next content-block index (tools get indices after text)
	openTools    map[int]bool   // tool indices currently open
	finishReason string
	promptTokens int
	outputTokens int
}

func (e *anthropicStreamEmitter) Emit(ev StreamEvent) ([]StreamChunk, error) {
	switch ev.Type {
	case EventRole:
		if !e.roleSent {
			e.roleSent = true
			return []StreamChunk{e.messageStart()}, nil
		}
	case EventContent:
		var out []StreamChunk
		if !e.roleSent {
			e.roleSent = true
			out = append(out, e.messageStart())
		}
		if !e.textOpen {
			e.textOpen = true
			e.blockStarted = true
			out = append(out, e.contentBlockStart(0))
		}
		out = append(out, e.contentBlockDelta(0, ev.Delta))
		return out, nil
	case EventToolCallStart:
		var out []StreamChunk
		if !e.roleSent {
			e.roleSent = true
			out = append(out, e.messageStart())
		}
		// Close the text block before starting a tool block.
		if e.textOpen {
			e.textOpen = false
			out = append(out, e.contentBlockStop(0))
		}
		e.nextBlockIdx++
		idx := e.nextBlockIdx
		if e.openTools == nil {
			e.openTools = make(map[int]bool)
		}
		e.openTools[idx] = true
		// Map the IR tool-call index to an Anthropic block index. We use the
		// IR index+1 as the block index so block indices are unique and stable.
		blkIdx := ev.ToolCallIndex + 1
		out = append(out, e.toolBlockStart(blkIdx, ev.ToolCallID, ev.ToolCallName))
		return out, nil
	case EventToolCallDelta:
		blkIdx := ev.ToolCallIndex + 1
		return []StreamChunk{e.toolBlockDelta(blkIdx, ev.ArgumentsDelta)}, nil
	case EventToolCallFinish:
		blkIdx := ev.ToolCallIndex + 1
		return []StreamChunk{e.contentBlockStop(blkIdx)}, nil
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

// Done emits the closing frames: close any open text block, message_delta
// (with stop_reason + usage), message_stop.
func (e *anthropicStreamEmitter) Done() []StreamChunk {
	var out []StreamChunk
	if !e.roleSent {
		out = append(out, e.messageStart())
	}
	if !e.blockStarted {
		// No content at all: emit an empty text block for compliance.
		out = append(out, e.contentBlockStart(0), e.contentBlockStop(0))
	} else if e.textOpen {
		out = append(out, e.contentBlockStop(0))
	}

	stopReason := mapChatFinishToAnthropic(e.finishReason)
	out = append(out, e.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": e.outputTokens},
	}))
	out = append(out, e.event("message_stop", map[string]any{"type": "message_stop"}))
	return out
}

func (e *anthropicStreamEmitter) messageStart() StreamChunk {
	return e.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            e.id,
			"type":          "message",
			"role":          "assistant",
			"model":         e.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  e.promptTokens,
				"output_tokens": 0,
			},
		},
	})
}

func (e *anthropicStreamEmitter) contentBlockStart(idx int) StreamChunk {
	return e.event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (e *anthropicStreamEmitter) contentBlockDelta(idx int, text string) StreamChunk {
	return e.event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (e *anthropicStreamEmitter) contentBlockStop(idx int) StreamChunk {
	return e.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

func (e *anthropicStreamEmitter) toolBlockStart(idx int, id, name string) StreamChunk {
	return e.event("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
	})
}

func (e *anthropicStreamEmitter) toolBlockDelta(idx int, partialJSON string) StreamChunk {
	return e.event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": partialJSON},
	})
}

func (e *anthropicStreamEmitter) event(eventType string, payload map[string]any) StreamChunk {
	b, _ := json.Marshal(payload)
	return StreamChunk{Event: eventType, Data: b}
}

// --- shared helpers ---

// anthropicContentText extracts text from an Anthropic message content field
// (string or array of {type:"text", text} blocks). Non-text blocks are
// rejected (Phase A).
func anthropicContentText(content any) (string, error) {
	switch c := content.(type) {
	case string:
		return c, nil
	case []any:
		var sb strings.Builder
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok {
				return "", errInvalidContent
			}
			if t, _ := p["type"].(string); t != "text" {
				return "", errMultimodalUnsupported
			}
			if text, ok := p["text"].(string); ok {
				sb.WriteString(text)
			}
		}
		return sb.String(), nil
	case nil:
		return "", nil
	default:
		return "", errInvalidContent
	}
}

// parseAnthropicContent parses an Anthropic message content field into the IR
// message cm. text blocks → Content/Parts; image blocks → Parts; tool_use
// blocks (assistant) → cm.ToolCalls; tool_result blocks → separate role="tool"
// messages appended to *out (the caller appends cm itself after).
func parseAnthropicContent(content any, cm *ChatMessage, out *[]ChatMessage) error {
	switch c := content.(type) {
	case string:
		cm.Content = c
	case []any:
		var sb strings.Builder
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok {
				return errInvalidContent
			}
			switch t, _ := p["type"].(string); t {
			case "text":
				if text, ok := p["text"].(string); ok {
					sb.WriteString(text)
					cm.Parts = append(cm.Parts, ContentPart{Type: "text", Text: text})
				}
			case "image":
				img, err := parseAnthropicImage(p)
				if err != nil {
					return err
				}
				cm.Parts = append(cm.Parts, ContentPart{Type: "image", Image: img})
			case "tool_use":
				input, _ := json.Marshal(p["input"])
				cm.ToolCalls = append(cm.ToolCalls, ToolCall{
					ID:        getString(p, "id"),
					Name:      getString(p, "name"),
					Arguments: json.RawMessage(input),
				})
			case "tool_result":
				// Emit a separate role="tool" IR message.
				*out = append(*out, ChatMessage{
					Role:       "tool",
					ToolCallID: getString(p, "tool_use_id"),
					Content:    anthropicToolResultContent(p["content"]),
				})
			default:
				return errMultimodalUnsupported
			}
		}
		// If only text parts (no image/tool), collapse to Content and drop Parts.
		hasImage := false
		for _, pp := range cm.Parts {
			if pp.Type == "image" {
				hasImage = true
			}
		}
		if !hasImage {
			cm.Content = sb.String()
			cm.Parts = nil
		}
	case nil:
		// no content
	default:
		return errInvalidContent
	}
	return nil
}

// parseAnthropicImage builds an IR Image from an Anthropic image block.
func parseAnthropicImage(p map[string]any) (*Image, error) {
	src, _ := p["source"].(map[string]any)
	if src != nil {
		switch getString(src, "type") {
		case "base64":
			return &Image{Base64: getString(src, "data"), MIMEType: getString(src, "media_type")}, nil
		case "url":
			return &Image{URL: getString(src, "url")}, nil
		}
	}
	// Fallback: a top-level url field.
	if url := getString(p, "url"); url != "" {
		return &Image{URL: url}, nil
	}
	return nil, errInvalidContent
}

// anthropicToolResultContent extracts text from a tool_result block's content
// (string or array of text blocks).
func anthropicToolResultContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := p["type"].(string); t == "text" {
				if text, ok := p["text"].(string); ok {
					sb.WriteString(text)
				}
			}
		}
		return sb.String()
	case nil:
		return ""
	}
	return ""
}

// joinAnthropicTextBlocks joins the text blocks of an Anthropic system field
// (array of {type:"text", text}).
func joinAnthropicTextBlocks(blocks []any) string {
	var sb strings.Builder
	for _, b := range blocks {
		blk, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := blk["type"].(string); t == "text" {
			if text, ok := blk["text"].(string); ok {
				sb.WriteString(text)
			}
		}
	}
	return sb.String()
}

// buildAnthropicMessages converts IR messages to Anthropic messages. Consecutive
// role="tool" IR messages are merged into a single user message with
// tool_result blocks (Anthropic requires tool results inside a user message).
// Assistant messages with ToolCalls emit tool_use blocks; multimodal messages
// emit text/image blocks.
func buildAnthropicMessages(msgs []ChatMessage) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	i := 0
	for i < len(msgs) {
		m := msgs[i]
		if m.Role == "tool" {
			// Merge consecutive tool messages into one user message.
			results := []map[string]any{}
			for i < len(msgs) && msgs[i].Role == "tool" {
				results = append(results, map[string]any{
					"type":        "tool_result",
					"tool_use_id": msgs[i].ToolCallID,
					"content":     msgs[i].Content,
				})
				i++
			}
			out = append(out, map[string]any{"role": "user", "content": results})
			continue
		}
		content := buildAnthropicContent(m)
		out = append(out, map[string]any{"role": m.Role, "content": content})
		i++
	}
	return out
}

// buildAnthropicContent builds the Anthropic content array for one IR message:
// text/image parts, plus tool_use blocks for assistant tool calls.
func buildAnthropicContent(m ChatMessage) []map[string]any {
	var content []map[string]any
	if len(m.Parts) > 0 {
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				content = append(content, map[string]any{"type": "text", "text": p.Text})
			case "image":
				content = append(content, map[string]any{"type": "image", "source": anthropicImageSource(p.Image)})
			}
		}
	} else if m.Content != "" || len(m.ToolCalls) == 0 {
		content = append(content, map[string]any{"type": "text", "text": m.Content})
	}
	for _, tc := range m.ToolCalls {
		input, _ := argsToObject(tc.Arguments)
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Name,
			"input": input,
		})
	}
	return content
}

// anthropicImageSource builds the Anthropic source object for an IR image:
// base64 form when base64 data is available, url form otherwise.
func anthropicImageSource(img *Image) map[string]any {
	if img == nil {
		return nil
	}
	if img.Base64 != "" {
		mime := img.MIMEType
		if mime == "" {
			mime = "image/png"
		}
		return map[string]any{"type": "base64", "media_type": mime, "data": img.Base64}
	}
	if mime, b64, ok := parseDataURI(img.URL); ok {
		return map[string]any{"type": "base64", "media_type": mime, "data": b64}
	}
	return map[string]any{"type": "url", "url": img.URL}
}

// extractAnthropicText concatenates text from an Anthropic content blocks array.
func extractAnthropicText(content any) string {
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		blk, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := blk["type"].(string); t == "text" {
			if text, _ := blk["text"].(string); text != "" {
				sb.WriteString(text)
			}
		}
	}
	return sb.String()
}

func mapAnthropicStopReason(r string) string {
	switch r {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		if r == "" {
			return "stop"
		}
		return "stop"
	}
}

func mapChatFinishToAnthropic(r string) string {
	switch r {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// ensure io.EOF is referenced
var _ = io.EOF
