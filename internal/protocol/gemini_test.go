package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// ---- IsInboundStream / path detection ----

func TestGemini_IsInboundStream(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	if !h.IsInboundStream("/proxy/g/v1beta/models/gemini-2.0:streamGenerateContent", nil) {
		t.Error("stream path should report stream")
	}
	if h.IsInboundStream("/proxy/g/v1beta/models/gemini-2.0:generateContent", nil) {
		t.Error("non-stream path should not report stream")
	}
}

func TestExtractGeminiModel(t *testing.T) {
	cases := map[string]string{
		"/v1beta/models/gemini-2.0:generateContent":           "gemini-2.0",
		"/v1beta/models/gemini-2.0:streamGenerateContent":     "gemini-2.0",
		"/proxy/g/v1beta/models/gemini-1.5-flash:generateContent": "gemini-1.5-flash",
		"/v1/chat/completions": "",
	}
	for path, want := range cases {
		if got := extractGeminiModel(path); got != want {
			t.Errorf("extractGeminiModel(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestGemini_UpstreamPath(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	path, q := h.UpstreamPath("gemini-2.0", false)
	if path != "/v1beta/models/gemini-2.0:generateContent" || q != "" {
		t.Errorf("non-stream path=%q q=%q", path, q)
	}
	path, q = h.UpstreamPath("gemini-2.0", true)
	if path != "/v1beta/models/gemini-2.0:streamGenerateContent" || q != "alt=sse" {
		t.Errorf("stream path=%q q=%q", path, q)
	}
}

// ---- ParseInboundRequest (gemini client → IR) ----

func TestGemini_ParseInboundRequest(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	ir, err := h.ParseInboundRequest("/v1beta/models/gemini-2.0:generateContent", []byte(`{
		"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"yo"}]}],
		"systemInstruction":{"parts":[{"text":"be brief"}]},
		"generationConfig":{"temperature":0.7,"maxOutputTokens":100,"topP":0.9}}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest: %v", err)
	}
	if ir.Model != "gemini-2.0" {
		t.Errorf("model=%q want gemini-2.0", ir.Model)
	}
	if ir.System != "be brief" {
		t.Errorf("system=%q", ir.System)
	}
	if len(ir.Messages) != 2 || ir.Messages[0].Content != "hi" || ir.Messages[1].Role != "assistant" || ir.Messages[1].Content != "yo" {
		t.Errorf("messages=%+v", ir.Messages)
	}
	if ir.Temperature == nil || *ir.Temperature != 0.7 || ir.MaxTokens == nil || *ir.MaxTokens != 100 || ir.TopP == nil || *ir.TopP != 0.9 {
		t.Errorf("params=%+v", ir)
	}
}

func TestGemini_ParseInboundRequest_Tools(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	ir, err := h.ParseInboundRequest("/v1beta/models/m:generateContent", []byte(`{
		"contents":[{"role":"user","parts":[{"text":"x"}]}],
		"tools":[{"functionDeclarations":[{"name":"get_weather","description":"d","parameters":{"type":"object"}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["get_weather"]}}}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest tools: %v", err)
	}
	if len(ir.Tools) != 1 || ir.Tools[0].Name != "get_weather" {
		t.Errorf("tools=%+v", ir.Tools)
	}
	if ir.ToolChoice == nil || ir.ToolChoice.Type != "function" || ir.ToolChoice.Name != "get_weather" {
		t.Errorf("tool_choice=%+v", ir.ToolChoice)
	}
}

func TestGemini_ParseInboundRequest_FunctionCallAndResponse(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	ir, err := h.ParseInboundRequest("/v1beta/models/m:generateContent", []byte(`{"contents":[
		{"role":"user","parts":[{"text":"weather?"}]},
		{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"q":"sf"}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"get_weather","response":{"output":"sunny"}}}]}]}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest: %v", err)
	}
	// model functionCall → ToolCalls
	if len(ir.Messages[1].ToolCalls) != 1 || ir.Messages[1].ToolCalls[0].Name != "get_weather" {
		t.Errorf("tool_calls=%+v", ir.Messages[1].ToolCalls)
	}
	// functionResponse → role="tool" message
	var toolMsgs []ChatMessage
	for _, m := range ir.Messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 1 || toolMsgs[0].Name != "get_weather" {
		t.Errorf("tool msgs=%+v", toolMsgs)
	}
	if !strings.Contains(toolMsgs[0].Content, "sunny") {
		t.Errorf("tool content=%q", toolMsgs[0].Content)
	}
}

func TestGemini_ParseInboundRequest_Image(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	ir, _ := h.ParseInboundRequest("/v1beta/models/m:generateContent",
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"look"},{"inline_data":{"mime_type":"image/png","data":"QUJD="}}]}]}`))
	if len(ir.Messages[0].Parts) != 2 || ir.Messages[0].Parts[1].Image.Base64 != "QUJD=" {
		t.Errorf("parts=%+v", ir.Messages[0].Parts)
	}
}

func TestGemini_EmitUpstreamRequest_RejectsURLImage(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	ir := &ChatRequest{Model: "m", Messages: []ChatMessage{{
		Role: "user",
		Parts: []ContentPart{{Type: "image", Image: &Image{URL: "https://example.com/x.png"}}},
	}}}
	if _, err := h.EmitUpstreamRequest(ir); err == nil {
		t.Fatal("expected URL image rejection for gemini")
	}
}

func TestGemini_EmitUpstreamRequest_ToolsAndResults(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	ir := &ChatRequest{Model: "m",
		Tools: []ToolDef{{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Messages: []ChatMessage{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "u1", Name: "get_weather", Arguments: json.RawMessage(`{"q":"sf"}`)}}},
			{Role: "tool", ToolCallID: "u1", Name: "get_weather", Content: `{"output":"sunny"}`},
		},
	}
	out, err := h.EmitUpstreamRequest(ir)
	if err != nil {
		t.Fatalf("EmitUpstreamRequest: %v", err)
	}
	m := mustDecode(t, out)
	if m["tools"] == nil {
		t.Error("missing tools")
	}
	contents := m["contents"].([]any)
	// assistant functionCall part
	asst := contents[1].(map[string]any)
	parts := asst["parts"].([]any)
	if parts[0].(map[string]any)["functionCall"] == nil {
		t.Errorf("assistant parts=%v", parts)
	}
	// tool result merged into user content with functionResponse
	toolUser := contents[2].(map[string]any)
	if toolUser["role"] != "user" {
		t.Errorf("tool user role=%v", toolUser["role"])
	}
	frs := toolUser["parts"].([]any)
	if frs[0].(map[string]any)["functionResponse"] == nil {
		t.Errorf("functionResponse=%v", frs)
	}
}

func TestGemini_ParseUpstreamResponse_FunctionCall(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	ir, err := h.ParseUpstreamResponse([]byte(`{
		"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"q":"sf"}}}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}`))
	if err != nil {
		t.Fatalf("ParseUpstreamResponse: %v", err)
	}
	if len(ir.Choices[0].Message.ToolCalls) != 1 || ir.Choices[0].Message.ToolCalls[0].Name != "get_weather" {
		t.Errorf("tool_calls=%+v", ir.Choices[0].Message.ToolCalls)
	}
	if ir.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish=%v want tool_calls", ir.Choices[0].FinishReason)
	}
}

func TestGemini_EmitInboundResponse_FunctionCall(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	ir := &ChatResponse{ID: "r", Model: "m",
		Choices: []ChatChoice{{Message: ChatMessage{
			ToolCalls: []ToolCall{{ID: "u1", Name: "get_weather", Arguments: json.RawMessage(`{"q":"sf"}`)}},
		}, FinishReason: "tool_calls"}},
		PromptTokens: 5, CompletionTokens: 2}
	out, _ := h.EmitInboundResponse(ir)
	m := mustDecode(t, out)
	parts := m["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if parts[0].(map[string]any)["functionCall"] == nil {
		t.Errorf("parts=%v", parts)
	}
}

// ---- EmitUpstreamRequest (IR → gemini body) ----

func TestGemini_EmitUpstreamRequest(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	mt := 100
	tp := 0.7
	ir := &ChatRequest{Model: "gemini-2.0", System: "sys", MaxTokens: &mt, Temperature: &tp,
		Messages: []ChatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}}}
	out, err := h.EmitUpstreamRequest(ir)
	if err != nil {
		t.Fatalf("EmitUpstreamRequest: %v", err)
	}
	m := mustDecode(t, out)
	contents := m["contents"].([]any)
	// assistant → model role
	if contents[0].(map[string]any)["role"] != "user" || contents[1].(map[string]any)["role"] != "model" {
		t.Errorf("roles=%v", contents)
	}
	if contents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] != "hi" {
		t.Errorf("parts=%v", contents[0])
	}
	if m["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] != "sys" {
		t.Errorf("systemInstruction=%v", m["systemInstruction"])
	}
	gc := m["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(100) || gc["temperature"] != 0.7 {
		t.Errorf("generationConfig=%v", gc)
	}
}

// ---- ParseUpstreamResponse (gemini response → IR) ----

func TestGemini_ParseUpstreamResponse(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	ir, err := h.ParseUpstreamResponse([]byte(`{
		"candidates":[{"content":{"role":"model","parts":[{"text":"Hello!"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7},
		"modelVersion":"gemini-2.0","responseId":"r1"}`))
	if err != nil {
		t.Fatalf("ParseUpstreamResponse: %v", err)
	}
	if ir.ID != "r1" || ir.Model != "gemini-2.0" {
		t.Errorf("ir=%+v", ir)
	}
	if len(ir.Choices) != 1 || ir.Choices[0].Message.Content != "Hello!" || ir.Choices[0].FinishReason != "stop" {
		t.Errorf("choices=%+v", ir.Choices)
	}
	if ir.PromptTokens != 5 || ir.CompletionTokens != 2 {
		t.Errorf("usage=%d/%d", ir.PromptTokens, ir.CompletionTokens)
	}
}

func TestGemini_ParseUpstreamResponse_FinishReasons(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	cases := map[string]string{"STOP": "stop", "MAX_TOKENS": "length", "SAFETY": "stop"}
	for in, want := range cases {
		ir, _ := h.ParseUpstreamResponse([]byte(`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"` + in + `"}]}`))
		if ir.Choices[0].FinishReason != want {
			t.Errorf("finishReason %s -> %q, want %q", in, ir.Choices[0].FinishReason, want)
		}
	}
}

// ---- EmitInboundResponse (IR → gemini response) ----

func TestGemini_EmitInboundResponse(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	ir := &ChatResponse{ID: "r1", Model: "gemini-2.0",
		Choices: []ChatChoice{{Message: ChatMessage{Content: "hi"}, FinishReason: "stop"}},
		PromptTokens: 3, CompletionTokens: 1}
	out, err := h.EmitInboundResponse(ir)
	if err != nil {
		t.Fatalf("EmitInboundResponse: %v", err)
	}
	m := mustDecode(t, out)
	cands := m["candidates"].([]any)
	c := cands[0].(map[string]any)
	if c["finishReason"] != "STOP" {
		t.Errorf("finishReason=%v want STOP", c["finishReason"])
	}
	if c["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] != "hi" {
		t.Errorf("text=%v", c["content"])
	}
	usage := m["usageMetadata"].(map[string]any)
	if usage["totalTokenCount"] != float64(4) {
		t.Errorf("usage=%v", usage)
	}
}

// ---- Stream parser (gemini upstream SSE → IR events) ----

func TestGemini_StreamParser(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	p := h.NewUpstreamStreamParser()

	chunks := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]},"finishReason":null}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":null}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}`,
	}
	var events []StreamEvent
	for i, ch := range chunks {
		evs, err := p.Parse("", []byte(ch))
		if err != nil {
			t.Fatalf("Parse[%d]: %v", i, err)
		}
		events = append(events, evs...)
	}
	// Expect: role, content(Hel), content(lo), finish(stop), usage
	wantTypes := []string{EventRole, EventContent, EventContent, EventFinish, EventUsage}
	gotTypes := make([]string, len(events))
	for i, e := range events {
		gotTypes[i] = e.Type
	}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("event types=%v want %v", gotTypes, wantTypes)
	}
	if events[1].Delta != "Hel" || events[2].Delta != "lo" {
		t.Errorf("deltas=%q %q", events[1].Delta, events[2].Delta)
	}
	if events[3].FinishReason != "stop" {
		t.Errorf("finish=%v", events[3])
	}
	if events[4].PromptTokens != 5 || events[4].CompletionTokens != 2 {
		t.Errorf("usage=%+v", events[4])
	}
}

// ---- Stream emitter (IR events → gemini SSE) ----

func TestGemini_StreamEmitter(t *testing.T) {
	h, _ := GetHandler(FormatGemini)
	em := h.NewInboundStreamEmitter("gemini-2.0")

	var chunks []StreamChunk
	emit := func(evs []StreamEvent) {
		for _, ev := range evs {
			cs, err := em.Emit(ev)
			if err != nil {
				t.Fatalf("Emit(%+v): %v", ev, err)
			}
			chunks = append(chunks, cs...)
		}
	}
	emit([]StreamEvent{{Type: EventRole, Delta: "assistant"}})
	emit([]StreamEvent{{Type: EventContent, Delta: "Hel"}})
	emit([]StreamEvent{{Type: EventContent, Delta: "lo"}})
	emit([]StreamEvent{{Type: EventFinish, FinishReason: "stop"}})
	emit([]StreamEvent{{Type: EventUsage, PromptTokens: 5, CompletionTokens: 2}})
	chunks = append(chunks, em.Done()...) // gemini Done() returns nil

	// Each chunk should be a valid GenerateContentResponse with role "model".
	var texts []string
	var finishSet bool
	for _, ch := range chunks {
		var m map[string]any
		if err := json.Unmarshal(ch.Data, &m); err != nil {
			t.Fatalf("bad chunk %q: %v", string(ch.Data), err)
		}
		cands := m["candidates"].([]any)
		c := cands[0].(map[string]any)
		if c["content"].(map[string]any)["role"] != "model" {
			t.Errorf("role=%v want model", c["content"])
		}
		if text := c["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string); text != "" {
			texts = append(texts, text)
		}
		if fr, ok := c["finishReason"].(string); ok && fr != "" {
			if fr != "STOP" {
				t.Errorf("finishReason=%v want STOP", fr)
			}
			finishSet = true
		}
	}
	if !reflect.DeepEqual(texts, []string{"Hel", "lo"}) {
		t.Errorf("texts=%v want [Hel lo]", texts)
	}
	if !finishSet {
		t.Error("no chunk carried finishReason")
	}
	// Done() returns no terminal chunk for gemini (no explicit terminator).
	// The last emitted chunk should be the usage one.
	last := chunks[len(chunks)-1]
	var m map[string]any
	json.Unmarshal(last.Data, &m)
	if m["usageMetadata"] == nil {
		t.Error("last chunk should carry usageMetadata")
	}
}
