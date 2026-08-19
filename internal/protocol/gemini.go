package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"any-load/internal/models"
)

// geminiHandler converts between Gemini generateContent and the chat IR, in
// both directions and for stream + non-stream. The model is carried in the
// request path (not the body), and streaming is selected by the path suffix
// :streamGenerateContent.
type geminiHandler struct{}

func init() {
	Register(&geminiHandler{})
}

func (h *geminiHandler) Format() string            { return FormatGemini }
func (h *geminiHandler) InboundContentType() string { return "application/json" }

// IsInboundStream reports stream intent for a Gemini inbound request: the
// path suffix :streamGenerateContent.
func (h *geminiHandler) IsInboundStream(path string, body []byte) bool {
	return strings.Contains(path, ":streamGenerateContent")
}

// UpstreamPath returns the Gemini native path with the model embedded, and
// alt=sse for streaming.
func (h *geminiHandler) UpstreamPath(model string, stream bool) (string, string) {
	if stream {
		return "/v1beta/models/" + model + ":streamGenerateContent", "alt=sse"
	}
	return "/v1beta/models/" + model + ":generateContent", ""
}

// ApplyAuth adds the API key as a query parameter for Gemini.
func (h *geminiHandler) ApplyAuth(req *http.Request, apiKey *models.APIKey) {
	q := req.URL.Query()
	q.Set("key", apiKey.KeyValue)
	req.URL.RawQuery = q.Encode()
}

// --- Inbound direction (Gemini client → IR) ---

// ParseInboundRequest parses a Gemini generateContent body into the IR. The
// model is taken from the path (Gemini carries it in the URL, not the body).
func (h *geminiHandler) ParseInboundRequest(path string, body []byte) (*ChatRequest, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	ir := &ChatRequest{Model: extractGeminiModel(path)}

	if sys, ok := raw["systemInstruction"].(map[string]any); ok {
		ir.System = geminiPartsText(sys["parts"])
	}

	// Tools (function declarations).
	if tools, ok := raw["tools"].([]any); ok {
		for _, t := range tools {
			td, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fds, _ := td["functionDeclarations"].([]any)
			for _, fd := range fds {
				f, ok := fd.(map[string]any)
				if !ok {
					continue
				}
				ir.Tools = append(ir.Tools, ToolDef{
					Name:        getString(f, "name"),
					Description: getString(f, "description"),
					Parameters:  rawJSON(f["parameters"]),
				})
			}
		}
	}
	if tc, ok := raw["toolConfig"].(map[string]any); ok {
		ir.ToolChoice = normalizeToolChoiceGemini(tc["functionCallingConfig"])
	}

	if contents, ok := raw["contents"].([]any); ok {
		for ci, c := range contents {
			content, ok := c.(map[string]any)
			if !ok {
				continue
			}
			role, _ := content["role"].(string)
			if role == "model" {
				role = "assistant"
			}
			cm := ChatMessage{Role: role}
			if err := parseGeminiParts(content["parts"], &cm, &ir.Messages, ci); err != nil {
				return nil, err
			}
			ir.Messages = append(ir.Messages, cm)
		}
	}

	if gc, ok := raw["generationConfig"].(map[string]any); ok {
		if t, ok := gc["temperature"].(float64); ok {
			ir.Temperature = &t
		}
		if p, ok := gc["topP"].(float64); ok {
			ir.TopP = &p
		}
		if mt, ok := gc["maxOutputTokens"].(float64); ok {
			v := int(mt)
			ir.MaxTokens = &v
		}
	}
	return ir, nil
}

// parseGeminiParts parses a Gemini parts array into the IR message cm.
// text → Content/Parts; inline_data → image Parts; functionCall (model) →
// cm.ToolCalls; functionResponse (user) → a separate role="tool" message
// appended to *out (Gemini has no call IDs; we synthesize name#index).
func parseGeminiParts(parts any, cm *ChatMessage, out *[]ChatMessage, contentIdx int) error {
	arr, ok := parts.([]any)
	if !ok {
		return nil
	}
	var sb strings.Builder
	toolCallCount := 0
	for _, p := range arr {
		part, ok := p.(map[string]any)
		if !ok {
			return errInvalidContent
		}
		switch {
		case part["text"] != nil:
			if text, ok := part["text"].(string); ok {
				sb.WriteString(text)
				cm.Parts = append(cm.Parts, ContentPart{Type: "text", Text: text})
			}
		case part["inline_data"] != nil:
			data, _ := part["inline_data"].(map[string]any)
			cm.Parts = append(cm.Parts, ContentPart{Type: "image", Image: &Image{
				Base64:   getString(data, "data"),
				MIMEType: getString(data, "mime_type"),
			}})
		case part["functionCall"] != nil:
			fc, _ := part["functionCall"].(map[string]any)
			name := getString(fc, "name")
			args, _ := json.Marshal(fc["args"])
			cm.ToolCalls = append(cm.ToolCalls, ToolCall{
				ID:        fmt.Sprintf("%s_%d", name, contentIdx*10+toolCallCount),
				Name:      name,
				Arguments: json.RawMessage(args),
			})
			toolCallCount++
		case part["functionResponse"] != nil:
			fr, _ := part["functionResponse"].(map[string]any)
			name := getString(fr, "name")
			resp, _ := json.Marshal(fr["response"])
			*out = append(*out, ChatMessage{
				Role:       "tool",
				ToolCallID: fmt.Sprintf("%s_%d", name, contentIdx),
				Name:       name,
				Content:    string(resp),
			})
		default:
			return errMultimodalUnsupported
		}
	}
	// Collapse to text-only fast path if no image.
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
	return nil
}

// EmitInboundResponse emits the IR as a Gemini generateContent response body.
func (h *geminiHandler) EmitInboundResponse(ir *ChatResponse) ([]byte, error) {
	text := ""
	finishReason := "STOP"
	var toolCalls []ToolCall
	if len(ir.Choices) > 0 {
		text = ir.Choices[0].Message.Content
		toolCalls = ir.Choices[0].Message.ToolCalls
		if ir.Choices[0].FinishReason != "" {
			finishReason = mapChatFinishToGemini(ir.Choices[0].FinishReason)
		}
	}
	parts := []map[string]any{}
	if text != "" || len(toolCalls) == 0 {
		parts = append(parts, map[string]any{"text": text})
	}
	for _, tc := range toolCalls {
		args, _ := argsToObject(tc.Arguments)
		parts = append(parts, map[string]any{
			"functionCall": map[string]any{"name": tc.Name, "args": args},
		})
	}
	out := map[string]any{
		"candidates": []map[string]any{{
			"content": map[string]any{
				"role":  "model",
				"parts": parts,
			},
			"finishReason": finishReason,
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":     ir.PromptTokens,
			"candidatesTokenCount": ir.CompletionTokens,
			"totalTokenCount":      ir.PromptTokens + ir.CompletionTokens,
		},
	}
	return json.Marshal(out)
}

// NewInboundStreamEmitter returns an emitter producing Gemini SSE chunks.
func (h *geminiHandler) NewInboundStreamEmitter(model string) StreamEmitter {
	return &geminiStreamEmitter{}
}

// --- Upstream direction (IR → Gemini upstream) ---

// EmitUpstreamRequest emits the IR as a Gemini generateContent request body.
func (h *geminiHandler) EmitUpstreamRequest(ir *ChatRequest) ([]byte, error) {
	if len(ir.Messages) == 0 {
		return nil, errors.New("at least one message is required")
	}
	contents, err := buildGeminiContents(ir.Messages)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"contents": contents}
	if ir.System != "" {
		out["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": ir.System}},
		}
	}
	if len(ir.Tools) > 0 {
		fds := make([]map[string]any, 0, len(ir.Tools))
		for _, t := range ir.Tools {
			fd := map[string]any{"name": t.Name, "description": t.Description}
			if len(t.Parameters) > 0 {
				fd["parameters"] = json.RawMessage(t.Parameters)
			}
			fds = append(fds, fd)
		}
		out["tools"] = []map[string]any{{"functionDeclarations": fds}}
	}
	if tc := emitToolChoiceGemini(ir.ToolChoice); tc != nil {
		out["toolConfig"] = tc
	}
	gc := map[string]any{}
	if ir.Temperature != nil {
		gc["temperature"] = *ir.Temperature
	}
	if ir.TopP != nil {
		gc["topP"] = *ir.TopP
	}
	if ir.MaxTokens != nil {
		gc["maxOutputTokens"] = *ir.MaxTokens
	}
	if len(gc) > 0 {
		out["generationConfig"] = gc
	}
	return json.Marshal(out)
}

// buildGeminiContents converts IR messages to Gemini contents. Consecutive
// role="tool" IR messages are merged into one user content with
// functionResponse parts. Returns errGeminiURLImage if an image only has a URL.
func buildGeminiContents(msgs []ChatMessage) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(msgs))
	i := 0
	for i < len(msgs) {
		m := msgs[i]
		if m.Role == "tool" {
			parts := []map[string]any{}
			for i < len(msgs) && msgs[i].Role == "tool" {
				name := msgs[i].Name
				if name == "" {
					name = "tool"
				}
				var resp any
				if err := json.Unmarshal([]byte(msgs[i].Content), &resp); err != nil {
					resp = map[string]any{"output": msgs[i].Content}
				}
				parts = append(parts, map[string]any{
					"functionResponse": map[string]any{"name": name, "response": resp},
				})
				i++
			}
			out = append(out, map[string]any{"role": "user", "parts": parts})
			continue
		}
		parts, err := buildGeminiParts(m)
		if err != nil {
			return nil, err
		}
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		out = append(out, map[string]any{"role": role, "parts": parts})
		i++
	}
	return out, nil
}

// buildGeminiParts builds Gemini parts for one IR message: text/image parts,
// plus functionCall parts for assistant tool calls. Images require base64.
func buildGeminiParts(m ChatMessage) ([]map[string]any, error) {
	var parts []map[string]any
	if len(m.Parts) > 0 {
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				parts = append(parts, map[string]any{"text": p.Text})
			case "image":
				gp, err := geminiImagePart(p.Image)
				if err != nil {
					return nil, err
				}
				parts = append(parts, gp)
			}
		}
	} else if m.Content != "" || len(m.ToolCalls) == 0 {
		parts = append(parts, map[string]any{"text": m.Content})
	}
	for _, tc := range m.ToolCalls {
		args, _ := argsToObject(tc.Arguments)
		parts = append(parts, map[string]any{
			"functionCall": map[string]any{"name": tc.Name, "args": args},
		})
	}
	return parts, nil
}

// geminiImagePart builds a Gemini inline_data part from an IR image. Gemini
// requires base64; a plain URL (non-data: URI) is rejected.
func geminiImagePart(img *Image) (map[string]any, error) {
	if img == nil {
		return nil, errInvalidContent
	}
	if img.Base64 != "" {
		mime := img.MIMEType
		if mime == "" {
			mime = "image/png"
		}
		return map[string]any{"inline_data": map[string]any{"mime_type": mime, "data": img.Base64}}, nil
	}
	if mime, b64, ok := parseDataURI(img.URL); ok {
		return map[string]any{"inline_data": map[string]any{"mime_type": mime, "data": b64}}, nil
	}
	return nil, errGeminiURLImage
}

// ParseUpstreamResponse parses a Gemini generateContent response into the IR.
func (h *geminiHandler) ParseUpstreamResponse(body []byte) (*ChatResponse, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	ir := &ChatResponse{
		ID:    getString(raw, "responseId"),
		Model: getString(raw, "modelVersion"),
	}
	text := ""
	finish := "stop"
	var toolCalls []ToolCall
	if candidates, ok := raw["candidates"].([]any); ok && len(candidates) > 0 {
		c, _ := candidates[0].(map[string]any)
		if content, ok := c["content"].(map[string]any); ok {
			text = geminiPartsText(content["parts"])
			// functionCall parts → tool calls.
			if parts, ok := content["parts"].([]any); ok {
				for idx, p := range parts {
					part, ok := p.(map[string]any)
					if !ok {
						continue
					}
					if fc, ok := part["functionCall"].(map[string]any); ok {
						name := getString(fc, "name")
						args, _ := json.Marshal(fc["args"])
						toolCalls = append(toolCalls, ToolCall{
							ID:        fmt.Sprintf("%s_%d", name, idx),
							Name:      name,
							Arguments: json.RawMessage(args),
						})
					}
				}
			}
		}
		if fr, ok := c["finishReason"].(string); ok && fr != "" {
			finish = mapGeminiFinishToChat(fr)
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
	if usage, ok := raw["usageMetadata"].(map[string]any); ok {
		ir.PromptTokens = toInt(usage["promptTokenCount"])
		ir.CompletionTokens = toInt(usage["candidatesTokenCount"])
	}
	return ir, nil
}

// NewUpstreamStreamParser returns a parser for Gemini SSE chunks.
func (h *geminiHandler) NewUpstreamStreamParser() StreamParser {
	return &geminiStreamParser{roleSent: false}
}

// --- Gemini stream parser (upstream SSE → IR events) ---

type geminiStreamParser struct {
	roleSent    bool
	toolStarted map[int]bool
}

func (p *geminiStreamParser) Parse(eventType string, data []byte) ([]StreamEvent, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var chunk map[string]any
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, nil
	}

	var events []StreamEvent
	if !p.roleSent {
		p.roleSent = true
		events = append(events, StreamEvent{Type: EventRole, Delta: "assistant"})
	}

	if candidates, ok := chunk["candidates"].([]any); ok && len(candidates) > 0 {
		c, _ := candidates[0].(map[string]any)
		if content, ok := c["content"].(map[string]any); ok {
			if parts, ok := content["parts"].([]any); ok {
				for idx, pp := range parts {
					part, ok := pp.(map[string]any)
					if !ok {
						continue
					}
					if text, ok := part["text"].(string); ok && text != "" {
						events = append(events, StreamEvent{Type: EventContent, Delta: text})
					}
					if fc, ok := part["functionCall"].(map[string]any); ok {
						if p.toolStarted == nil {
							p.toolStarted = make(map[int]bool)
						}
						if !p.toolStarted[idx] {
							p.toolStarted[idx] = true
							name := getString(fc, "name")
							events = append(events, StreamEvent{
								Type: EventToolCallStart, ToolCallIndex: idx,
								ToolCallID: fmt.Sprintf("%s_%d", name, idx), ToolCallName: name,
							})
							args, _ := json.Marshal(fc["args"])
							events = append(events, StreamEvent{
								Type: EventToolCallDelta, ToolCallIndex: idx, ArgumentsDelta: string(args),
							})
							events = append(events, StreamEvent{Type: EventToolCallFinish, ToolCallIndex: idx})
						}
					}
				}
			}
		}
		if fr, ok := c["finishReason"].(string); ok && fr != "" {
			finish := mapGeminiFinishToChat(fr)
			if len(p.toolStarted) > 0 {
				finish = "tool_calls"
			}
			events = append(events, StreamEvent{Type: EventFinish, FinishReason: finish})
		}
	}
	if usage, ok := chunk["usageMetadata"].(map[string]any); ok {
		events = append(events, StreamEvent{
			Type:             EventUsage,
			PromptTokens:     toInt(usage["promptTokenCount"]),
			CompletionTokens: toInt(usage["candidatesTokenCount"]),
		})
	}
	return events, nil
}

// --- Gemini stream emitter (IR events → Gemini SSE) ---

type geminiStreamEmitter struct {
	roleSent bool
	finish   string
	prompt   int
	output   int
	// Tool calls being assembled: index → accumulated argument bytes + name.
	toolArgs map[int]string
	toolName map[int]string
}

func (e *geminiStreamEmitter) Emit(ev StreamEvent) ([]StreamChunk, error) {
	switch ev.Type {
	case EventRole:
		if !e.roleSent {
			e.roleSent = true
			return []StreamChunk{e.chunk("", nil, nil, nil)}, nil
		}
	case EventContent:
		if !e.roleSent {
			e.roleSent = true
			return []StreamChunk{e.chunk("", nil, nil, nil), e.chunk(ev.Delta, nil, nil, nil)}, nil
		}
		return []StreamChunk{e.chunk(ev.Delta, nil, nil, nil)}, nil
	case EventToolCallStart:
		if e.toolArgs == nil {
			e.toolArgs = make(map[int]string)
			e.toolName = make(map[int]string)
		}
		e.toolName[ev.ToolCallIndex] = ev.ToolCallName
		e.toolArgs[ev.ToolCallIndex] = ""
		// Gemini delivers whole function calls; emit nothing yet.
		return nil, nil
	case EventToolCallDelta:
		if e.toolArgs == nil {
			e.toolArgs = make(map[int]string)
		}
		e.toolArgs[ev.ToolCallIndex] += ev.ArgumentsDelta
		return nil, nil
	case EventToolCallFinish:
		// Emit the assembled functionCall part. Gemini has no partial-args
		// streaming, so we emit the whole call at finish.
		name := e.toolName[ev.ToolCallIndex]
		argsRaw := e.toolArgs[ev.ToolCallIndex]
		var args any
		if err := json.Unmarshal([]byte(argsRaw), &args); err != nil {
			args = map[string]any{}
		}
		fc := map[string]any{"name": name, "args": args}
		return []StreamChunk{e.chunk("", nil, fc, nil)}, nil
	case EventFinish:
		e.finish = ev.FinishReason
		return []StreamChunk{e.chunk("", &ev.FinishReason, nil, nil)}, nil
	case EventUsage:
		e.prompt = ev.PromptTokens
		e.output = ev.CompletionTokens
		return []StreamChunk{e.chunk("", nil, nil, e.usageMap(ev))}, nil
	}
	return nil, nil
}

// Done returns the terminal chunk(s). Gemini SSE has no explicit terminator,
// so nothing is emitted here; the stream simply ends.
func (e *geminiStreamEmitter) Done() []StreamChunk {
	return nil
}

func (e *geminiStreamEmitter) chunk(text string, finishReason *string, functionCall map[string]any, usage map[string]any) StreamChunk {
	parts := []map[string]any{}
	if text != "" {
		parts = append(parts, map[string]any{"text": text})
	}
	if functionCall != nil {
		parts = append(parts, map[string]any{"functionCall": functionCall})
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]any{"text": ""})
	}
	candidate := map[string]any{
		"content": map[string]any{"role": "model", "parts": parts},
	}
	if finishReason != nil {
		candidate["finishReason"] = mapChatFinishToGemini(*finishReason)
	}
	out := map[string]any{"candidates": []map[string]any{candidate}}
	if usage != nil {
		out["usageMetadata"] = usage
	}
	b, _ := json.Marshal(out)
	return StreamChunk{Data: b}
}

func (e *geminiStreamEmitter) usageMap(ev StreamEvent) map[string]any {
	return map[string]any{
		"promptTokenCount":     ev.PromptTokens,
		"candidatesTokenCount": ev.CompletionTokens,
		"totalTokenCount":      ev.PromptTokens + ev.CompletionTokens,
	}
}

// --- helpers ---

// geminiPartsText concatenates text parts from a Gemini parts array. Non-text
// parts are ignored here (the parse path rejects them earlier).
func geminiPartsText(parts any) string {
	arr, ok := parts.([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, p := range arr {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := part["text"].(string); ok {
			sb.WriteString(text)
		}
	}
	return sb.String()
}

// geminiPartsTextErr concatenates text parts and rejects non-text parts.
func geminiPartsTextErr(parts any) (string, error) {
	arr, ok := parts.([]any)
	if !ok {
		return "", nil
	}
	var sb strings.Builder
	for _, p := range arr {
		part, ok := p.(map[string]any)
		if !ok {
			return "", errInvalidContent
		}
		// Gemini text parts only have "text". Anything else (inline_data,
		// file_data) is multimodal — reject in Phase A.
		if text, ok := part["text"].(string); ok {
			sb.WriteString(text)
			continue
		}
		return "", errMultimodalUnsupported
	}
	return sb.String(), nil
}

func mapGeminiFinishToChat(r string) string {
	switch r {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "OTHER":
		return "stop"
	default:
		return "stop"
	}
}

func mapChatFinishToGemini(r string) string {
	switch r {
	case "length":
		return "MAX_TOKENS"
	case "tool_calls":
		return "OTHER"
	default:
		return "STOP"
	}
}
