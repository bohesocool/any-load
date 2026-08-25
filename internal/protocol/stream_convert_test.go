package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"any-load/internal/models"
)

// fakeStreamParser replays a canned list of (eventType, data) frames as IR
// events via a fixed event list, so AccumulateStreamToResponse can be tested
// without a real upstream format. Each Parse call pops the next frame.
type fakeStreamParser struct {
	frames []fakeFrame
	idx    int
	events []StreamEvent // events to return for the next Parse
}

type fakeFrame struct {
	eventType string
	data      []byte
	events    []StreamEvent
	eof      bool // return io.EOF after this frame's events
}

func (p *fakeStreamParser) Parse(eventType string, data []byte) ([]StreamEvent, error) {
	if p.idx >= len(p.frames) {
		return nil, nil
	}
	f := p.frames[p.idx]
	p.idx++
	if f.eof {
		return f.events, io.EOF
	}
	return f.events, nil
}

// sseFrame builds one SSE frame string from event/data.
func sseFrame(event string, data string) string {
	if event == "" {
		return "data: " + data + "\n\n"
	}
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// fakeHandler lets us inject a custom stream parser; the other FormatHandler
// methods are unused by AccumulateStreamToResponse / ExpandResponseToEvents.
type fakeHandler struct{ parser StreamParser }

func (h *fakeHandler) Format() string                                  { return "fake" }
func (h *fakeHandler) InboundContentType() string                       { return "application/json" }
func (h *fakeHandler) IsInboundStream(path string, body []byte) bool     { return false }
func (h *fakeHandler) ParseInboundRequest(path string, body []byte) (*ChatRequest, error) {
	return nil, nil
}
func (h *fakeHandler) EmitInboundResponse(ir *ChatResponse) ([]byte, error) { return nil, nil }
func (h *fakeHandler) NewInboundStreamEmitter(model string) StreamEmitter { return nil }
func (h *fakeHandler) UpstreamPath(model string, stream bool) (string, string) {
	return "", ""
}
func (h *fakeHandler) ApplyAuth(req *http.Request, apiKey *models.APIKey)    {}
func (h *fakeHandler) EmitUpstreamRequest(ir *ChatRequest) ([]byte, error)   { return nil, nil }
func (h *fakeHandler) ParseUpstreamResponse(body []byte) (*ChatResponse, error) { return nil, nil }
func (h *fakeHandler) NewUpstreamStreamParser() StreamParser                 { return h.parser }

func TestAccumulateStreamToResponse_TextAndUsage(t *testing.T) {
	// Two content deltas + finish + usage, terminated by parser EOF.
	parser := &fakeStreamParser{frames: []fakeFrame{
		{events: []StreamEvent{{Type: EventRole, Delta: "assistant"}}},
		{events: []StreamEvent{{Type: EventContent, Delta: "Hel"}}},
		{events: []StreamEvent{{Type: EventContent, Delta: "lo"}}},
		{events: []StreamEvent{
			{Type: EventFinish, FinishReason: "stop"},
			{Type: EventUsage, PromptTokens: 5, CompletionTokens: 2},
		}, eof: true},
	}}
	h := &fakeHandler{parser: parser}

	// SSE body with one data frame per Parse call.
	var body strings.Builder
	for range parser.frames {
		body.WriteString(sseFrame("", `{"x":1}`))
	}

	ir, err := AccumulateStreamToResponse(h, strings.NewReader(body.String()), "test-model")
	if err != nil {
		t.Fatalf("accumulate: %v", err)
	}
	if ir.Model != "test-model" {
		t.Errorf("model=%q want test-model", ir.Model)
	}
	if len(ir.Choices) != 1 {
		t.Fatalf("choices=%d want 1", len(ir.Choices))
	}
	if ir.Choices[0].Message.Content != "Hello" {
		t.Errorf("content=%q want Hello", ir.Choices[0].Message.Content)
	}
	if ir.Choices[0].FinishReason != "stop" {
		t.Errorf("finish=%q want stop", ir.Choices[0].FinishReason)
	}
	if ir.PromptTokens != 5 || ir.CompletionTokens != 2 {
		t.Errorf("usage=%d/%d want 5/2", ir.PromptTokens, ir.CompletionTokens)
	}
}

func TestAccumulateStreamToResponse_GeminiIndexReuse(t *testing.T) {
	// Simulate Gemini's per-frame index reuse: two tool calls each delivered
	// as Start+Delta+Finish at index 0 across two separate frames. A naive
	// map[index]ToolCall would overwrite the first call with the second.
	parser := &fakeStreamParser{frames: []fakeFrame{
		// Frame 1: tool call A at index 0, fully delivered in one Parse call.
		{events: []StreamEvent{
			{Type: EventToolCallStart, ToolCallIndex: 0, ToolCallID: "callA", ToolCallName: "getWeather"},
			{Type: EventToolCallDelta, ToolCallIndex: 0, ArgumentsDelta: `{"city":"NYC"}`},
			{Type: EventToolCallFinish, ToolCallIndex: 0},
		}},
		// Frame 2: tool call B reusing index 0 (Gemini resets per frame).
		{events: []StreamEvent{
			{Type: EventToolCallStart, ToolCallIndex: 0, ToolCallID: "callB", ToolCallName: "getTime"},
			{Type: EventToolCallDelta, ToolCallIndex: 0, ArgumentsDelta: `{"tz":"UTC"}`},
			{Type: EventToolCallFinish, ToolCallIndex: 0},
		}},
	}}
	h := &fakeHandler{parser: parser}

	var body strings.Builder
	for range parser.frames {
		body.WriteString(sseFrame("", `{"x":1}`))
	}

	ir, err := AccumulateStreamToResponse(h, strings.NewReader(body.String()), "m")
	if err != nil {
		t.Fatalf("accumulate: %v", err)
	}
	tcs := ir.Choices[0].Message.ToolCalls
	if len(tcs) != 2 {
		t.Fatalf("tool calls=%d want 2 (index reuse must not overwrite)", len(tcs))
	}
	if tcs[0].Name != "getWeather" || string(tcs[0].Arguments) != `{"city":"NYC"}` {
		t.Errorf("tc0=%+v args=%s", tcs[0], string(tcs[0].Arguments))
	}
	if tcs[1].Name != "getTime" || string(tcs[1].Arguments) != `{"tz":"UTC"}` {
		t.Errorf("tc1=%+v args=%s", tcs[1], string(tcs[1].Arguments))
	}
}

func TestAccumulateStreamToResponse_GeminiNoParserEOF(t *testing.T) {
	// Gemini never returns io.EOF from Parse; the loop must end on the
	// ReadSSEFrame io.EOF (stream body exhausted). Frames carry finish/usage
	// but no parser EOF.
	parser := &fakeStreamParser{frames: []fakeFrame{
		{events: []StreamEvent{{Type: EventContent, Delta: "hi"}}},
		{events: []StreamEvent{
			{Type: EventFinish, FinishReason: "stop"},
			{Type: EventUsage, PromptTokens: 1, CompletionTokens: 1},
		}},
	}}
	h := &fakeHandler{parser: parser}

	var body strings.Builder
	for range parser.frames {
		body.WriteString(sseFrame("", `{"x":1}`))
	}

	ir, err := AccumulateStreamToResponse(h, strings.NewReader(body.String()), "m")
	if err != nil {
		t.Fatalf("accumulate: %v", err)
	}
	if ir.Choices[0].Message.Content != "hi" {
		t.Errorf("content=%q want hi", ir.Choices[0].Message.Content)
	}
	if ir.PromptTokens != 1 {
		t.Errorf("prompt=%d want 1", ir.PromptTokens)
	}
}

func TestExpandResponseToEvents_TextOnly(t *testing.T) {
	ir := &ChatResponse{
		Model: "m",
		Choices: []ChatChoice{{
			Message:      ChatMessage{Role: "assistant", Content: "hello"},
			FinishReason: "stop",
		}},
		PromptTokens:    3,
		CompletionTokens: 1,
	}
	events := ExpandResponseToEvents(ir)

	// Expect: role, content, finish, usage.
	wantTypes := []string{EventRole, EventContent, EventFinish, EventUsage}
	if len(events) != len(wantTypes) {
		t.Fatalf("events=%d want %d", len(events), len(wantTypes))
	}
	for i, w := range wantTypes {
		if events[i].Type != w {
			t.Errorf("event[%d].Type=%q want %q", i, events[i].Type, w)
		}
	}
	if events[1].Delta != "hello" {
		t.Errorf("content delta=%q want hello", events[1].Delta)
	}
	if events[2].FinishReason != "stop" {
		t.Errorf("finish=%q want stop", events[2].FinishReason)
	}
}

func TestExpandResponseToEvents_ToolCalls(t *testing.T) {
	ir := &ChatResponse{
		Choices: []ChatChoice{{
			Message: ChatMessage{
				Role:      "assistant",
				Content:   "ok",
				ToolCalls: []ToolCall{
					{ID: "a", Name: "f1", Arguments: json.RawMessage(`{"x":1}`)},
					{ID: "b", Name: "f2", Arguments: json.RawMessage(`{"y":2}`)},
				},
			},
			FinishReason: "tool_calls",
		}},
	}
	events := ExpandResponseToEvents(ir)

	// Expect: role, content, then per tool: start/delta/finish x2, then finish, usage.
	// indices: 0=role,1=content,2=tc0start,3=tc0delta,4=tc0finish,
	//          5=tc1start,6=tc1delta,7=tc1finish,8=finish,9=usage
	wantTypes := []string{
		EventRole, EventContent,
		EventToolCallStart, EventToolCallDelta, EventToolCallFinish,
		EventToolCallStart, EventToolCallDelta, EventToolCallFinish,
		EventFinish, EventUsage,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("events=%d want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, w := range wantTypes {
		if events[i].Type != w {
			t.Errorf("event[%d].Type=%q want %q", i, events[i].Type, w)
		}
	}
	// Tool call indices must be sequential 0,1.
	if events[2].ToolCallIndex != 0 || events[5].ToolCallIndex != 1 {
		t.Errorf("tool indices: %d,%d want 0,1", events[2].ToolCallIndex, events[5].ToolCallIndex)
	}
	if events[2].ToolCallName != "f1" || events[5].ToolCallName != "f2" {
		t.Errorf("tool names: %q,%q want f1,f2", events[2].ToolCallName, events[5].ToolCallName)
	}
	if events[3].ArgumentsDelta != `{"x":1}` {
		t.Errorf("tc0 delta=%q", events[3].ArgumentsDelta)
	}
	if events[8].FinishReason != "tool_calls" {
		t.Errorf("finish=%q want tool_calls", events[8].FinishReason)
	}
}

func TestExpandResponseToEvents_EmptyChoices(t *testing.T) {
	ir := &ChatResponse{}
	events := ExpandResponseToEvents(ir)
	// Defensive: role + finish + usage only.
	wantTypes := []string{EventRole, EventFinish, EventUsage}
	if len(events) != len(wantTypes) {
		t.Fatalf("events=%d want %d", len(events), len(wantTypes))
	}
	for i, w := range wantTypes {
		if events[i].Type != w {
			t.Errorf("event[%d].Type=%q want %q", i, events[i].Type, w)
		}
	}
	if events[1].FinishReason != "stop" {
		t.Errorf("default finish=%q want stop", events[1].FinishReason)
	}
}

// TestRoundTrip_AccumulateThenExpand verifies the two bridges compose:
// events → accumulate → IR → expand → events (content + usage preserved).
func TestRoundTrip_AccumulateThenExpand(t *testing.T) {
	parser := &fakeStreamParser{frames: []fakeFrame{
		{events: []StreamEvent{{Type: EventRole, Delta: "assistant"}}},
		{events: []StreamEvent{{Type: EventContent, Delta: "round"}}},
		{events: []StreamEvent{{Type: EventContent, Delta: "trip"}}},
		{events: []StreamEvent{
			{Type: EventFinish, FinishReason: "stop"},
			{Type: EventUsage, PromptTokens: 7, CompletionTokens: 3},
		}, eof: true},
	}}
	h := &fakeHandler{parser: parser}

	var body strings.Builder
	for range parser.frames {
		body.WriteString(sseFrame("", `{"x":1}`))
	}
	ir, err := AccumulateStreamToResponse(h, strings.NewReader(body.String()), "m")
	if err != nil {
		t.Fatalf("accumulate: %v", err)
	}
	if ir.Choices[0].Message.Content != "roundtrip" {
		t.Fatalf("content=%q want roundtrip", ir.Choices[0].Message.Content)
	}

	events := ExpandResponseToEvents(ir)
	// Find the content event; its delta must equal the concatenated content.
	var contentDelta string
	for _, e := range events {
		if e.Type == EventContent {
			contentDelta = e.Delta
		}
	}
	if contentDelta != "roundtrip" {
		t.Errorf("expanded content delta=%q want roundtrip", contentDelta)
	}
	// Silence unused bytes import if this test is the only consumer in some
	// build configurations.
	_ = bytes.Equal
}
