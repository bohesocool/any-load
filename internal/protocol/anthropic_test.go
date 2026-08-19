package protocol

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

func mustDecode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("failed to decode %q: %v", string(b), err)
	}
	return m
}

func ptrInt(v int) *int { return &v }

// ---- DetectInboundFormat / PickTarget ----

func TestDetectInboundFormat(t *testing.T) {
	cases := map[string]string{
		"/proxy/g/v1/chat/completions":         FormatOpenAIChat,
		"/v1/chat/completions":                  FormatOpenAIChat,
		"/proxy/g/v1/messages":                 FormatAnthropic,
		"/v1/messages":                          FormatAnthropic,
		"/proxy/g/v1/responses":                FormatOpenAIResponse,
		"/v1beta/models/gemini-2.0:generateContent":       FormatGemini,
		"/v1beta/models/gemini-2.0:streamGenerateContent": FormatGemini,
		"/v1beta/openai/v1/chat/completions":   FormatOpenAIChat,
		"/v1/models":                            "",
		"/proxy/g/v1/embeddings":               "",
		"/unknown":                              "",
	}
	for path, want := range cases {
		if got := DetectInboundFormat(path); got != want {
			t.Errorf("DetectInboundFormat(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestPickTarget(t *testing.T) {
	tests := []struct {
		formats []string
		inbound string
		want    string
	}{
		{[]string{"anthropic"}, FormatOpenAIChat, "anthropic"},       // chat in → anthropic target
		{[]string{"openai-chat", "anthropic"}, FormatOpenAIChat, "openai-chat"}, // smart passthrough
		{[]string{"anthropic", "openai-chat"}, FormatAnthropic, "anthropic"},    // anthropic in, supported → passthrough
		{[]string{"openai-chat"}, FormatAnthropic, "openai-chat"},    // anthropic in → chat target
		{[]string{"gemini", "anthropic"}, FormatOpenAIChat, "gemini"}, // first when no match
		{[]string{}, FormatOpenAIChat, ""},
		{nil, FormatOpenAIChat, ""},
	}
	for _, tt := range tests {
		got := PickTarget(tt.formats, tt.inbound)
		if got != tt.want {
			t.Errorf("PickTarget(%v, %q) = %q, want %q", tt.formats, tt.inbound, got, tt.want)
		}
	}
}

func TestSupportedFormats(t *testing.T) {
	supported := SupportedFormats()
	got := map[string]bool{}
	for _, f := range supported {
		got[f] = true
	}
	// All four formats are registered: openai-chat, anthropic, gemini, openai-response.
	want := map[string]bool{
		FormatOpenAIChat: true, FormatAnthropic: true,
		FormatGemini: true, FormatOpenAIResponse: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SupportedFormats = %v, want %v", got, want)
	}
}

// ---- openai-chat identity ----

func TestOpenAIChat_ParseInboundRequest(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIChat)
	ir, err := h.ParseInboundRequest("", []byte(`{
		"model":"gpt-4",
		"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],
		"max_tokens":50,"temperature":0.7,"stream":true}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest: %v", err)
	}
	if ir.Model != "gpt-4" || ir.System != "sys" || len(ir.Messages) != 1 || ir.Messages[0].Content != "hi" {
		t.Errorf("ir = %+v", ir)
	}
	if ir.MaxTokens == nil || *ir.MaxTokens != 50 || ir.Temperature == nil || *ir.Temperature != 0.7 || !ir.Stream {
		t.Errorf("ir params = %+v", ir)
	}
}

func TestOpenAIChat_ParseInboundRequest_MergesSystem(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIChat)
	ir, _ := h.ParseInboundRequest("", []byte(`{"model":"m","messages":[
		{"role":"system","content":"a"},{"role":"system","content":"b"},{"role":"user","content":"x"}]}`))
	if ir.System != "a\n\nb" {
		t.Errorf("system = %q, want 'a\\n\\nb'", ir.System)
	}
}

func TestOpenAIChat_ParseInboundRequest_Tools(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIChat)
	ir, err := h.ParseInboundRequest("", []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":"x"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest with tools: %v", err)
	}
	if len(ir.Tools) != 1 || ir.Tools[0].Name != "get_weather" {
		t.Errorf("tools=%+v", ir.Tools)
	}
	if ir.ToolChoice == nil || ir.ToolChoice.Type != "function" || ir.ToolChoice.Name != "get_weather" {
		t.Errorf("tool_choice=%+v", ir.ToolChoice)
	}
}

func TestOpenAIChat_ParseInboundRequest_ToolCallsAndResult(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIChat)
	ir, err := h.ParseInboundRequest("", []byte(`{"model":"m","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"q\":\"sf\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"}]}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest: %v", err)
	}
	if len(ir.Messages) != 3 {
		t.Fatalf("messages=%d", len(ir.Messages))
	}
	asst := ir.Messages[1]
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_1" || asst.ToolCalls[0].Name != "get_weather" {
		t.Errorf("tool_calls=%+v", asst.ToolCalls)
	}
	if string(asst.ToolCalls[0].Arguments) != `{"q":"sf"}` {
		t.Errorf("arguments=%q", asst.ToolCalls[0].Arguments)
	}
	tool := ir.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" || tool.Content != "sunny" {
		t.Errorf("tool message=%+v", tool)
	}
}

func TestOpenAIChat_ParseInboundRequest_Image(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIChat)
	ir, err := h.ParseInboundRequest("", []byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"what is this?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD="}}]}]}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest image: %v", err)
	}
	if len(ir.Messages[0].Parts) != 2 {
		t.Fatalf("parts=%+v", ir.Messages[0].Parts)
	}
	if ir.Messages[0].Parts[1].Type != "image" {
		t.Errorf("part1=%+v", ir.Messages[0].Parts[1])
	}
	if ir.Messages[0].Parts[1].Image.Base64 != "QUJD=" || ir.Messages[0].Parts[1].Image.MIMEType != "image/png" {
		t.Errorf("image=%+v", ir.Messages[0].Parts[1].Image)
	}
}

func TestOpenAIChat_EmitInboundResponse(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIChat)
	mt := 5
	ir := &ChatResponse{ID: "id1", Model: "gpt-4",
		Choices: []ChatChoice{{Index: 0, Message: ChatMessage{Role: "assistant", Content: "hi"}, FinishReason: "stop"}},
		PromptTokens: 3, CompletionTokens: mt}
	out, err := h.EmitInboundResponse(ir)
	if err != nil {
		t.Fatalf("EmitInboundResponse: %v", err)
	}
	m := mustDecode(t, out)
	if m["object"] != "chat.completion" || m["id"] != "id1" {
		t.Errorf("bad: %v", m)
	}
	choice := m["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "hi" || choice["finish_reason"] != "stop" {
		t.Errorf("choice = %v", choice)
	}
	usage := m["usage"].(map[string]any)
	if usage["total_tokens"] != float64(8) {
		t.Errorf("usage = %v", usage)
	}
}

func TestOpenAIChat_EmitUpstreamRequest_Tools(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIChat)
	ir := &ChatRequest{
		Model: "m",
		Tools: []ToolDef{{Name: "get_weather", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: &ToolChoice{Type: "function", Name: "get_weather"},
		Messages: []ChatMessage{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"q":"sf"}`)}}},
			{Role: "tool", ToolCallID: "call_1", Name: "get_weather", Content: "sunny"},
		},
	}
	out, err := h.EmitUpstreamRequest(ir)
	if err != nil {
		t.Fatalf("EmitUpstreamRequest: %v", err)
	}
	m := mustDecode(t, out)
	tools := m["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool name=%v", fn["name"])
	}
	tc := m["tool_choice"].(map[string]any)
	if tc["type"] != "function" {
		t.Errorf("tool_choice=%v", tc)
	}
	msgs := m["messages"].([]any)
	asst := msgs[1].(map[string]any)
	tcArr := asst["tool_calls"].([]any)[0].(map[string]any)
	if tcArr["id"] != "call_1" || tcArr["function"].(map[string]any)["arguments"] != `{"q":"sf"}` {
		t.Errorf("tool_call=%v", tcArr)
	}
	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "sunny" {
		t.Errorf("tool msg=%v", tool)
	}
}

func TestOpenAIChat_ToolCall_Stream(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIChat)
	em := h.NewInboundStreamEmitter("gpt-4")
	var chunks []StreamChunk
	emit := func(evs []StreamEvent) {
		for _, ev := range evs {
			cs, _ := em.Emit(ev)
			chunks = append(chunks, cs...)
		}
	}
	emit([]StreamEvent{{Type: EventRole, Delta: "assistant"}})
	emit([]StreamEvent{{Type: EventToolCallStart, ToolCallIndex: 0, ToolCallID: "call_1", ToolCallName: "get_weather"}})
	emit([]StreamEvent{{Type: EventToolCallDelta, ToolCallIndex: 0, ArgumentsDelta: `{"q":`}})
	emit([]StreamEvent{{Type: EventToolCallDelta, ToolCallIndex: 0, ArgumentsDelta: `"sf"}`}})
	emit([]StreamEvent{{Type: EventToolCallFinish, ToolCallIndex: 0}})
	emit([]StreamEvent{{Type: EventFinish, FinishReason: "tool_calls"}})
	chunks = append(chunks, em.Done()...)

	// Collect tool_calls deltas from the emitted chunks.
	var args strings.Builder
	var sawStart bool
	for _, ch := range chunks {
		if string(ch.Data) == "[DONE]" {
			continue
		}
		var m map[string]any
		if json.Unmarshal(ch.Data, &m) != nil {
			continue
		}
		choice, _ := m["choices"].([]any)
		if len(choice) == 0 {
			continue
		}
		delta := choice[0].(map[string]any)["delta"].(map[string]any)
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			tc := tcs[0].(map[string]any)
			fn := tc["function"].(map[string]any)
			if name, ok := fn["name"].(string); ok && name != "" {
				sawStart = true
				if name != "get_weather" {
					t.Errorf("name=%v", name)
				}
			}
			if a, ok := fn["arguments"].(string); ok && a != "" {
				args.WriteString(a)
			}
		}
	}
	if !sawStart {
		t.Error("no tool_call start chunk with name")
	}
	if args.String() != `{"q":"sf"}` {
		t.Errorf("streamed args=%q want {\"q\":\"sf\"}", args.String())
	}
}

// ---- anthropic bidirectional ----

func TestAnthropic_ParseInboundRequest(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	ir, err := h.ParseInboundRequest("", []byte(`{
		"model":"claude-3","system":"be brief","max_tokens":100,"temperature":0.5,
		"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest: %v", err)
	}
	if ir.Model != "claude-3" || ir.System != "be brief" {
		t.Errorf("ir = %+v", ir)
	}
	if ir.MaxTokens == nil || *ir.MaxTokens != 100 {
		t.Errorf("max_tokens = %v", ir.MaxTokens)
	}
	if len(ir.Messages) != 2 || ir.Messages[0].Content != "hi" || ir.Messages[1].Role != "assistant" {
		t.Errorf("messages = %+v", ir.Messages)
	}
}

func TestAnthropic_ParseInboundRequest_SystemBlocks(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	ir, _ := h.ParseInboundRequest("", []byte(`{"model":"c","system":[{"type":"text","text":"s1"},{"type":"text","text":"s2"}],"max_tokens":10,"messages":[{"role":"user","content":"x"}]}`))
	if ir.System != "s1s2" {
		t.Errorf("system = %q, want 's1s2'", ir.System)
	}
}

func TestAnthropic_ParseInboundRequest_ContentBlocks(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	ir, _ := h.ParseInboundRequest("", []byte(`{"model":"c","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`))
	if len(ir.Messages) != 1 || ir.Messages[0].Content != "ab" {
		t.Errorf("content = %q", ir.Messages[0].Content)
	}
}

func TestAnthropic_ParseInboundRequest_Tools(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	ir, err := h.ParseInboundRequest("", []byte(`{
		"model":"c","max_tokens":100,
		"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"get_weather","description":"d","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],
		"tool_choice":{"type":"any"}}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest tools: %v", err)
	}
	if len(ir.Tools) != 1 || ir.Tools[0].Name != "get_weather" {
		t.Errorf("tools=%+v", ir.Tools)
	}
	if ir.ToolChoice == nil || ir.ToolChoice.Type != "required" {
		t.Errorf("tool_choice=%+v (any→required)", ir.ToolChoice)
	}
}

func TestAnthropic_ParseInboundRequest_ToolUseAndResult(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	ir, err := h.ParseInboundRequest("", []byte(`{"model":"c","max_tokens":100,"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":[{"type":"tool_use","id":"u1","name":"get_weather","input":{"q":"sf"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"u1","content":"sunny"}]}]}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest: %v", err)
	}
	// assistant tool_use → ToolCalls
	if len(ir.Messages[1].ToolCalls) != 1 || ir.Messages[1].ToolCalls[0].ID != "u1" {
		t.Errorf("tool_calls=%+v", ir.Messages[1].ToolCalls)
	}
	// tool_result → separate role="tool" message
	// Messages: [user, assistant, tool(s)]
	var toolMsgs []ChatMessage
	for _, m := range ir.Messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 1 || toolMsgs[0].ToolCallID != "u1" || toolMsgs[0].Content != "sunny" {
		t.Errorf("tool result msgs=%+v", toolMsgs)
	}
}

func TestAnthropic_ParseInboundRequest_Image(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	ir, _ := h.ParseInboundRequest("", []byte(`{"model":"c","max_tokens":10,"messages":[{"role":"user","content":[
		{"type":"text","text":"look"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD="}}]}]}`))
	if len(ir.Messages[0].Parts) != 2 || ir.Messages[0].Parts[1].Image.Base64 != "QUJD=" {
		t.Errorf("parts=%+v", ir.Messages[0].Parts)
	}
}

func TestAnthropic_EmitUpstreamRequest_ToolsAndResults(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	ir := &ChatRequest{Model: "c", MaxTokens: ptrInt(100),
		Tools: []ToolDef{{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Messages: []ChatMessage{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "u1", Name: "get_weather", Arguments: json.RawMessage(`{"q":"sf"}`)}}},
			{Role: "tool", ToolCallID: "u1", Content: "sunny"},
			{Role: "user", Content: "thanks"},
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
	msgs := m["messages"].([]any)
	// assistant tool_use block
	asst := msgs[1].(map[string]any)
	blocks := asst["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "tool_use" {
		t.Errorf("assistant blocks=%v", blocks)
	}
	// tool result merged into one user message (followed by the "thanks" user msg)
	toolUser := msgs[2].(map[string]any)
	if toolUser["role"] != "user" {
		t.Errorf("tool user role=%v", toolUser["role"])
	}
	results := toolUser["content"].([]any)
	if results[0].(map[string]any)["type"] != "tool_result" || results[0].(map[string]any)["tool_use_id"] != "u1" {
		t.Errorf("tool_result=%v", results)
	}
}

func TestAnthropic_ParseUpstreamResponse_ToolUse(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	ir, err := h.ParseUpstreamResponse([]byte(`{
		"id":"m","model":"c","stop_reason":"tool_use",
		"content":[{"type":"tool_use","id":"u1","name":"get_weather","input":{"q":"sf"}}],
		"usage":{"input_tokens":5,"output_tokens":2}}`))
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

func TestAnthropic_EmitInboundResponse_ToolUse(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	ir := &ChatResponse{ID: "m", Model: "c",
		Choices: []ChatChoice{{Message: ChatMessage{
			ToolCalls: []ToolCall{{ID: "u1", Name: "get_weather", Arguments: json.RawMessage(`{"q":"sf"}`)}},
		}, FinishReason: "tool_calls"}},
		PromptTokens: 5, CompletionTokens: 2}
	out, _ := h.EmitInboundResponse(ir)
	m := mustDecode(t, out)
	if m["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v want tool_use", m["stop_reason"])
	}
	blocks := m["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "tool_use" || blocks[0].(map[string]any)["name"] != "get_weather" {
		t.Errorf("content=%v", blocks)
	}
}

func TestAnthropic_ToolCall_Stream(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	em := h.NewInboundStreamEmitter("c")
	var chunks []StreamChunk
	emit := func(evs []StreamEvent) {
		for _, ev := range evs {
			cs, _ := em.Emit(ev)
			chunks = append(chunks, cs...)
		}
	}
	emit([]StreamEvent{{Type: EventRole, Delta: "assistant"}})
	emit([]StreamEvent{{Type: EventToolCallStart, ToolCallIndex: 0, ToolCallID: "u1", ToolCallName: "get_weather"}})
	emit([]StreamEvent{{Type: EventToolCallDelta, ToolCallIndex: 0, ArgumentsDelta: `{"q":`}})
	emit([]StreamEvent{{Type: EventToolCallDelta, ToolCallIndex: 0, ArgumentsDelta: `"sf"}`}})
	emit([]StreamEvent{{Type: EventToolCallFinish, ToolCallIndex: 0}})
	emit([]StreamEvent{{Type: EventFinish, FinishReason: "tool_calls"}})
	emit([]StreamEvent{{Type: EventUsage, PromptTokens: 5, CompletionTokens: 2}})
	chunks = append(chunks, em.Done()...)

	var types []string
	var args strings.Builder
	for _, ch := range chunks {
		if ch.Event == "" {
			continue
		}
		types = append(types, ch.Event)
		var m map[string]any
		json.Unmarshal(ch.Data, &m)
		if ch.Event == "content_block_delta" {
			if d, ok := m["delta"].(map[string]any); ok {
				if pj, ok := d["partial_json"].(string); ok {
					args.WriteString(pj)
				}
			}
		}
	}
	// Expect message_start, content_block_start(tool_use), content_block_delta(s), content_block_stop,
	// message_delta, message_stop.
	if types[1] != "content_block_start" {
		t.Errorf("second event=%v want content_block_start", types[1])
	}
	// tool start block index should be 1 (text block index 0 not started).
	if args.String() != `{"q":"sf"}` {
		t.Errorf("args=%q want {\"q\":\"sf\"}", args.String())
	}
	// message_delta should carry stop_reason tool_use.
	found := false
	for _, ch := range chunks {
		if ch.Event != "message_delta" {
			continue
		}
		var m map[string]any
		json.Unmarshal(ch.Data, &m)
		if m["delta"].(map[string]any)["stop_reason"] == "tool_use" {
			found = true
		}
	}
	if !found {
		t.Error("no message_delta with stop_reason tool_use")
	}
}

func TestAnthropic_EmitUpstreamRequest(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	mt := 50
	tp := 0.7
	ir := &ChatRequest{Model: "claude", System: "sys", MaxTokens: &mt, Temperature: &tp,
		Messages: []ChatMessage{{Role: "user", Content: "hi"}}}
	out, err := h.EmitUpstreamRequest(ir)
	if err != nil {
		t.Fatalf("EmitUpstreamRequest: %v", err)
	}
	m := mustDecode(t, out)
	if m["model"] != "claude" || m["system"] != "sys" || m["max_tokens"] != float64(50) || m["temperature"] != 0.7 {
		t.Errorf("bad emit: %v", m)
	}
	msgs := m["messages"].([]any)
	// Content is now an array of blocks; the first block carries the text.
	content := msgs[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["text"] != "hi" {
		t.Errorf("messages = %v", msgs)
	}
}

func TestAnthropic_EmitUpstreamRequest_DefaultMaxTokens(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	out, _ := h.EmitUpstreamRequest(&ChatRequest{Model: "c", Messages: []ChatMessage{{Role: "user", Content: "x"}}})
	m := mustDecode(t, out)
	if m["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want 4096", m["max_tokens"])
	}
}

func TestAnthropic_EmitUpstreamRequest_StreamFlag(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	out, _ := h.EmitUpstreamRequest(&ChatRequest{Model: "c", Stream: true, Messages: []ChatMessage{{Role: "user", Content: "x"}}})
	if mustDecode(t, out)["stream"] != true {
		t.Error("stream should be true")
	}
}

func TestAnthropic_ParseUpstreamResponse(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	ir, err := h.ParseUpstreamResponse([]byte(`{
		"id":"msg_1","model":"claude-3","content":[{"type":"text","text":"Hello!"}],
		"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
	if err != nil {
		t.Fatalf("ParseUpstreamResponse: %v", err)
	}
	if ir.ID != "msg_1" || ir.Model != "claude-3" {
		t.Errorf("ir = %+v", ir)
	}
	if len(ir.Choices) != 1 || ir.Choices[0].Message.Content != "Hello!" || ir.Choices[0].FinishReason != "stop" {
		t.Errorf("choices = %+v", ir.Choices)
	}
	if ir.PromptTokens != 10 || ir.CompletionTokens != 5 {
		t.Errorf("usage = %d/%d", ir.PromptTokens, ir.CompletionTokens)
	}
}

func TestAnthropic_ParseUpstreamResponse_StopReasons(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	cases := map[string]string{
		"end_turn": "stop", "max_tokens": "length", "stop_sequence": "stop", "tool_use": "tool_calls",
	}
	for in, want := range cases {
		ir, _ := h.ParseUpstreamResponse([]byte(`{"id":"m","model":"c","content":[{"type":"text","text":"x"}],"stop_reason":"` + in + `","usage":{"input_tokens":1,"output_tokens":1}}`))
		if ir.Choices[0].FinishReason != want {
			t.Errorf("stop_reason %s -> %q, want %q", in, ir.Choices[0].FinishReason, want)
		}
	}
}

func TestAnthropic_EmitInboundResponse(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	ir := &ChatResponse{ID: "msg_1", Model: "claude-3",
		Choices: []ChatChoice{{Message: ChatMessage{Content: "Hello!"}, FinishReason: "stop"}},
		PromptTokens: 10, CompletionTokens: 5}
	out, err := h.EmitInboundResponse(ir)
	if err != nil {
		t.Fatalf("EmitInboundResponse: %v", err)
	}
	m := mustDecode(t, out)
	if m["type"] != "message" || m["role"] != "assistant" || m["stop_reason"] != "end_turn" {
		t.Errorf("bad: %v", m)
	}
	blocks := m["content"].([]any)
	if blocks[0].(map[string]any)["text"] != "Hello!" {
		t.Errorf("content = %v", blocks)
	}
	usage := m["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(5) {
		t.Errorf("usage = %v", usage)
	}
}

// ---- anthropic streaming (parser + emitter round trip) ----

func TestAnthropic_StreamParser(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	p := h.NewUpstreamStreamParser()

	events := []struct {
		et   string
		data string
	}{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":8,"output_tokens":1}}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}

	var allEvents []StreamEvent
	var gotEOF bool
	for _, e := range events {
		evs, err := p.Parse(e.et, []byte(e.data))
		allEvents = append(allEvents, evs...)
		if err == io.EOF {
			gotEOF = true
			break
		}
		if err != nil {
			t.Fatalf("Parse(%s): %v", e.et, err)
		}
	}
	if !gotEOF {
		t.Fatal("expected io.EOF on message_stop")
	}
	// Expect: role, content, content, finish, usage
	if len(allEvents) != 5 {
		t.Fatalf("events = %d, want 5: %+v", len(allEvents), allEvents)
	}
	if allEvents[0].Type != EventRole || allEvents[0].Delta != "assistant" {
		t.Errorf("ev0 = %+v", allEvents[0])
	}
	if allEvents[1].Type != EventContent || allEvents[1].Delta != "Hel" {
		t.Errorf("ev1 = %+v", allEvents[1])
	}
	if allEvents[2].Delta != "lo" {
		t.Errorf("ev2 = %+v", allEvents[2])
	}
	if allEvents[3].Type != EventFinish || allEvents[3].FinishReason != "stop" {
		t.Errorf("ev3 = %+v", allEvents[3])
	}
	if allEvents[4].Type != EventUsage || allEvents[4].PromptTokens != 8 || allEvents[4].CompletionTokens != 3 {
		t.Errorf("ev4 = %+v", allEvents[4])
	}
}

func TestAnthropic_StreamEmitter_RoundTrip(t *testing.T) {
	h, _ := GetHandler(FormatAnthropic)
	emitter := h.NewInboundStreamEmitter("claude-3")

	// Feed IR events then Done; collect emitted chunk JSON.
	var chunks []StreamChunk
	emit := func(evs []StreamEvent) {
		for _, ev := range evs {
			cs, err := emitter.Emit(ev)
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
	emit([]StreamEvent{{Type: EventUsage, PromptTokens: 8, CompletionTokens: 3}})
	chunks = append(chunks, emitter.Done()...)

	// Verify the chunk sequence has the expected event types in order.
	types := []string{}
	for _, ch := range chunks {
		if ch.Event == "" {
			continue
		}
		types = append(types, ch.Event)
	}
	want := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if !reflect.DeepEqual(types, want) {
		t.Errorf("event types = %v, want %v", types, want)
	}

	// First delta should carry "Hel", second "lo".
	deltas := []string{}
	for _, ch := range chunks {
		if ch.Event != "content_block_delta" {
			continue
		}
		var m map[string]any
		json.Unmarshal(ch.Data, &m)
		deltas = append(deltas, m["delta"].(map[string]any)["text"].(string))
	}
	if !reflect.DeepEqual(deltas, []string{"Hel", "lo"}) {
		t.Errorf("deltas = %v, want [Hel lo]", deltas)
	}
}

// ---- openai-chat streaming round trip (identity) ----

func TestOpenAIChat_StreamEmitter_Done(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIChat)
	emitter := h.NewInboundStreamEmitter("gpt-4")
	cs, _ := emitter.Emit(StreamEvent{Type: EventRole, Delta: "assistant"})
	if len(cs) != 1 {
		t.Fatalf("role chunk = %d, want 1", len(cs))
	}
	done := emitter.Done()
	if len(done) != 1 || string(done[0].Data) != "[DONE]" {
		t.Errorf("done = %v, want [[DONE]]", done)
	}
}
