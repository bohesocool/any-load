// Package protocol implements bidirectional format conversion between any
// supported LLM API format (OpenAI Chat, Anthropic Messages, Gemini, OpenAI
// Responses) using a canonical intermediate representation (IR).
//
// The IR is OpenAI Chat Completions. Each format registers a FormatHandler
// that knows how to parse that format into the IR and emit the IR into that
// format, in both request and response (including streaming) directions.
// A conversion from inbound format A to upstream format B is composed as
// A→IR→B (request) and B→IR→A (response), so only 2N converters are needed
// rather than N² pairwise translators.
package protocol

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"any-load/internal/models"
)

// Format ids. These are the values stored in a group's UpstreamFormats list
// and returned by DetectInboundFormat.
const (
	FormatOpenAIChat     = "openai-chat"
	FormatOpenAIResponse = "openai-response"
	FormatAnthropic      = "anthropic"
	FormatGemini         = "gemini"
)

// ChatMessage is a single text message in the IR.
// ChatMessage is one message in the IR (OpenAI Chat shape). Content is the
// text-only fast path (Parts == nil). Parts carries text+image parts when
// multimodal. ToolCalls carries assistant tool invocations. ToolCallID +
// Name identify a role="tool" result message.
type ChatMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`        // text-only fast path
	Parts      []ContentPart `json:"parts,omitempty"`          // multimodal content
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`    // assistant tool calls
	ToolCallID string        `json:"tool_call_id,omitempty"`  // role="tool" messages
	Name       string        `json:"name,omitempty"`           // role="tool" tool name
}

// TextContent returns the text of this message: Content when Parts is nil,
// otherwise the concatenation of text-type Parts.
func (m ChatMessage) TextContent() string {
	if len(m.Parts) == 0 {
		return m.Content
	}
	var sb strings.Builder
	for _, p := range m.Parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// ContentPart is one part of a multimodal message. Type is "text" or "image".
// Used only when a message carries non-text content; text-only messages use
// ChatMessage.Content directly (Parts == nil).
type ContentPart struct {
	Type  string  `json:"type"` // "text" | "image"
	Text  string  `json:"text,omitempty"`
	Image *Image  `json:"image,omitempty"`
}

// Image is an image attached to a message. Either URL (http/https/data: URI)
// or pre-split Base64 + MIMEType. Emitters that require base64 parse data:
// URIs via parseDataURI; emitters that cannot handle remote URLs reject.
type Image struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
}

// ToolDef is a function/tool definition (OpenAI Chat shape, normalized).
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON schema (object)
}

// ToolChoice selects tool behavior. Type ∈ {"auto","none","required","function"}.
// Name is set only when Type=="function" (forced tool).
type ToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// ToolCall is one assistant tool invocation. Arguments is a JSON object
// (json.RawMessage). OpenAI emitters wrap it as a string; Anthropic/Gemini
// emitters embed it as an object. May hold partial JSON during streaming.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ChatRequest is the canonical request IR (OpenAI Chat Completions shape).
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	System      string        `json:"system,omitempty"`
	Tools       []ToolDef     `json:"tools,omitempty"`
	ToolChoice  *ToolChoice   `json:"tool_choice,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

// ChatChoice is a single completion choice in the response IR.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatResponse is the canonical non-stream response IR.
type ChatResponse struct {
	ID              string        `json:"id"`
	Model           string        `json:"model"`
	Choices         []ChatChoice  `json:"choices"`
	PromptTokens     int          `json:"prompt_tokens"`
	CompletionTokens int         `json:"completion_tokens"`
}

// StreamEvent is a single incremental event in the streaming IR.
type StreamEvent struct {
	// Type is one of: "role", "content", "finish", "usage",
	// "tool_call_start", "tool_call_delta", "tool_call_finish".
	Type string `json:"type"`
	// Delta is the text delta for "content" events.
	Delta string `json:"delta,omitempty"`
	// FinishReason is set for "finish" events.
	FinishReason string `json:"finish_reason,omitempty"`
	// Token counts for "usage" events.
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	// Tool-call fields (for "tool_call_*" events). ToolCallIndex identifies
	// which parallel tool call; ToolCallID/ToolCallName set on start;
	// ArgumentsDelta carries partial JSON argument bytes for "tool_call_delta".
	ToolCallIndex  int    `json:"tool_call_index,omitempty"`
	ToolCallID     string `json:"tool_call_id,omitempty"`
	ToolCallName   string `json:"tool_call_name,omitempty"`
	ArgumentsDelta string `json:"arguments_delta,omitempty"`
}

// Stream event type constants.
const (
	EventRole           = "role"
	EventContent        = "content"
	EventFinish         = "finish"
	EventUsage          = "usage"
	EventToolCallStart  = "tool_call_start"  // {index, id, name}
	EventToolCallDelta  = "tool_call_delta"  // {index, arguments_delta}
	EventToolCallFinish = "tool_call_finish" // {index}
)

// StreamParser consumes one upstream SSE event at a time (in the parser's
// format) and emits zero or more IR StreamEvents. io.EOF signals the upstream
// stream has terminated and the parser has flushed its terminal state.
type StreamParser interface {
	Parse(eventType string, data []byte) ([]StreamEvent, error)
}

// StreamChunk is one SSE frame to write to the client. Event is the SSE event
// type (the `event:` line); when empty, no event line is written (OpenAI Chat
// uses data-only frames). Data is the JSON payload (without `data: ` framing).
type StreamChunk struct {
	Event string
	Data  []byte
}

// StreamEmitter consumes IR StreamEvents and emits zero or more SSE chunks in
// the emitter's inbound format. Done returns the terminal chunk(s) for the
// stream (e.g. `data: [DONE]` for openai-chat, or `message_stop` for anthropic).
type StreamEmitter interface {
	Emit(ev StreamEvent) ([]StreamChunk, error)
	Done() []StreamChunk
}

// FormatHandler converts between one API format and the chat IR, in both
// directions and for both request and response (stream + non-stream).
type FormatHandler interface {
	// Format returns this handler's format id.
	Format() string

	// Inbound direction (client format → IR).

	// IsInboundStream reports whether an inbound request (this format) asks
	// for streaming, given the inbound path and body. Most formats signal via
	// the body `stream` field; Gemini signals via the path (:streamGenerateContent).
	IsInboundStream(path string, body []byte) bool
	// ParseInboundRequest parses an inbound request (this format) into the IR.
	// path is the inbound URL path (after /proxy/:group); formats whose request
	// carries the model in the path (Gemini) use it to populate IR.Model. Returns
	// an error for unsupported features (treated as 400).
	ParseInboundRequest(path string, body []byte) (*ChatRequest, error)
	// EmitInboundResponse emits the IR as a non-stream response body in this
	// format (what the client expects).
	EmitInboundResponse(ir *ChatResponse) ([]byte, error)
	// NewInboundStreamEmitter returns an emitter that converts IR stream
	// events into this format's SSE chunks.
	NewInboundStreamEmitter(model string) StreamEmitter
	// InboundContentType is the Content-Type for this format's responses.
	InboundContentType() string

	// Upstream direction (IR → upstream format).

	// UpstreamPath returns the native upstream path and raw query for this
	// format given the (redirected) model and stream flag.
	UpstreamPath(model string, stream bool) (path, rawQuery string)
	// ApplyAuth sets this format's auth on the upstream request.
	ApplyAuth(req *http.Request, apiKey *models.APIKey)
	// EmitUpstreamRequest emits the IR as an upstream request body in this
	// format.
	EmitUpstreamRequest(ir *ChatRequest) ([]byte, error)
	// ParseUpstreamResponse parses an upstream non-stream response body (this
	// format) into the IR.
	ParseUpstreamResponse(body []byte) (*ChatResponse, error)
	// NewUpstreamStreamParser returns a parser that converts this format's
	// upstream SSE events into IR stream events.
	NewUpstreamStreamParser() StreamParser
}

var registry = map[string]FormatHandler{}

// Register adds a format handler to the registry. Panics on duplicates.
func Register(h FormatHandler) {
	if _, exists := registry[h.Format()]; exists {
		panic(fmt.Sprintf("protocol handler %q already registered", h.Format()))
	}
	registry[h.Format()] = h
}

// GetHandler returns the handler for the given format id.
func GetHandler(format string) (FormatHandler, bool) {
	h, ok := registry[format]
	return h, ok
}

// SupportedFormats returns all registered format ids.
func SupportedFormats() []string {
	out := make([]string, 0, len(registry))
	for f := range registry {
		out = append(out, f)
	}
	return out
}

// IsValidFormat reports whether f is a registered format.
func IsValidFormat(f string) bool {
	_, ok := registry[f]
	return ok
}

// DetectInboundFormat determines the inbound request format from the request
// path (the part after /proxy/:group). Returns "" when the path is not a
// conversion-eligible endpoint (e.g. model lists, unknown paths).
func DetectInboundFormat(path string) string {
	switch {
	case strings.HasSuffix(path, "/v1/chat/completions"):
		return FormatOpenAIChat
	case strings.HasSuffix(path, "/v1/messages"):
		return FormatAnthropic
	case strings.HasSuffix(path, "/v1/responses"):
		return FormatOpenAIResponse
	case strings.Contains(path, "/v1beta/openai/"):
		return FormatOpenAIChat // gemini's openai-compat layer speaks openai chat
	case strings.Contains(path, ":generateContent"), strings.Contains(path, ":streamGenerateContent"):
		return FormatGemini
	}
	return ""
}

// PickTarget selects the upstream target format given the group's configured
// upstream formats and the inbound format. If the inbound format is already
// in the list, it is returned unchanged (smart passthrough — no conversion).
// Otherwise the first listed format is the preferred target. Returns "" when
// the list is empty.
func PickTarget(upstreamFormats []string, inbound string) string {
	for _, f := range upstreamFormats {
		if f == inbound {
			return f
		}
	}
	if len(upstreamFormats) > 0 {
		return upstreamFormats[0]
	}
	return ""
}

// EmitInboundError builds an error response body in the given inbound format,
// suitable for returning directly to a client that speaks that format. Used by
// the proxy to surface a generic upstream-failure message (e.g. the "fuzzy
// failover" masked error) in the client's native error shape rather than a
// raw upstream body. format is a format id (as returned by DetectInboundFormat);
// an empty/unknown format falls back to the OpenAI-style envelope.
func EmitInboundError(format string, statusCode int, message string) []byte {
	var body map[string]any
	switch format {
	case FormatAnthropic:
		body = map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "overloaded_error", "message": message},
		}
	case FormatGemini:
		body = map[string]any{
			"error": map[string]any{
				"code":    statusCode,
				"message": message,
				"status":  "UNAVAILABLE",
			},
		}
	default: // openai-chat, openai-response, or unknown
		body = map[string]any{
			"error": map[string]any{"message": message, "type": "server_error"},
		}
	}
	b, _ := json.Marshal(body)
	return b
}
