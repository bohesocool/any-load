package protocol

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

// ---- ParseInboundRequest (Responses client → IR) ----

func TestResponses_ParseInboundRequest_StringInput(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	ir, err := h.ParseInboundRequest("", []byte(`{
		"model":"gpt-4.1","input":"hi","instructions":"be brief",
		"max_output_tokens":100,"temperature":0.7,"stream":true}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest: %v", err)
	}
	if ir.Model != "gpt-4.1" || ir.System != "be brief" {
		t.Errorf("ir=%+v", ir)
	}
	if len(ir.Messages) != 1 || ir.Messages[0].Role != "user" || ir.Messages[0].Content != "hi" {
		t.Errorf("messages=%+v", ir.Messages)
	}
	if ir.MaxTokens == nil || *ir.MaxTokens != 100 {
		t.Errorf("max_tokens=%v", ir.MaxTokens)
	}
	if !ir.Stream {
		t.Error("stream should be true")
	}
}

func TestResponses_ParseInboundRequest_ArrayInput(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	ir, err := h.ParseInboundRequest("", []byte(`{"model":"m","input":[
		{"role":"user","content":[{"type":"input_text","text":"hello"}]},
		{"role":"assistant","content":[{"type":"output_text","text":"world"}]}]}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest: %v", err)
	}
	if len(ir.Messages) != 2 || ir.Messages[0].Content != "hello" || ir.Messages[1].Content != "world" {
		t.Errorf("messages=%+v", ir.Messages)
	}
}

func TestResponses_ParseInboundRequest_SystemInInput(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	ir, _ := h.ParseInboundRequest("", []byte(`{"model":"m","input":[
		{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`))
	if ir.System != "sys" {
		t.Errorf("system=%q want sys", ir.System)
	}
	if len(ir.Messages) != 1 || ir.Messages[0].Content != "hi" {
		t.Errorf("messages=%+v", ir.Messages)
	}
}

func TestResponses_ParseInboundRequest_Tools(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	ir, err := h.ParseInboundRequest("", []byte(`{
		"model":"m","input":"hi",
		"tools":[{"type":"function","name":"get_weather","description":"d","parameters":{"type":"object"}}],
		"tool_choice":"auto"}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest tools: %v", err)
	}
	if len(ir.Tools) != 1 || ir.Tools[0].Name != "get_weather" {
		t.Errorf("tools=%+v", ir.Tools)
	}
	if ir.ToolChoice == nil || ir.ToolChoice.Type != "auto" {
		t.Errorf("tool_choice=%+v", ir.ToolChoice)
	}
}

func TestResponses_ParseInboundRequest_FunctionCallItems(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	ir, err := h.ParseInboundRequest("", []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
		{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"q\":\"sf\"}"},
		{"type":"function_call_output","call_id":"c1","output":"sunny"}]}`))
	if err != nil {
		t.Fatalf("ParseInboundRequest: %v", err)
	}
	// function_call → assistant ToolCalls
	var asst *ChatMessage
	var toolMsgs []ChatMessage
	for i := range ir.Messages {
		if ir.Messages[i].Role == "assistant" && len(ir.Messages[i].ToolCalls) > 0 {
			asst = &ir.Messages[i]
		}
		if ir.Messages[i].Role == "tool" {
			toolMsgs = append(toolMsgs, ir.Messages[i])
		}
	}
	if asst == nil || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "c1" {
		t.Errorf("assistant tool_calls=%+v", asst)
	}
	if len(toolMsgs) != 1 || toolMsgs[0].ToolCallID != "c1" || toolMsgs[0].Content != "sunny" {
		t.Errorf("tool msgs=%+v", toolMsgs)
	}
}

func TestResponses_ParseInboundRequest_Image(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	ir, _ := h.ParseInboundRequest("", []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"look"},
		{"type":"input_image","image_url":"data:image/png;base64,QUJD="}]}]}`))
	if len(ir.Messages[0].Parts) != 2 || ir.Messages[0].Parts[1].Image.Base64 != "QUJD=" {
		t.Errorf("parts=%+v", ir.Messages[0].Parts)
	}
}

func TestResponses_EmitUpstreamRequest_ToolsAndResults(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	ir := &ChatRequest{Model: "m",
		Tools: []ToolDef{{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Messages: []ChatMessage{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: json.RawMessage(`{"q":"sf"}`)}}},
			{Role: "tool", ToolCallID: "c1", Content: "sunny"},
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
	input := m["input"].([]any)
	// function_call item
	var fcItem map[string]any
	var fcoItem map[string]any
	for _, it := range input {
		im := it.(map[string]any)
		if im["type"] == "function_call" {
			fcItem = im
		}
		if im["type"] == "function_call_output" {
			fcoItem = im
		}
	}
	if fcItem == nil || fcItem["call_id"] != "c1" || fcItem["arguments"] != `{"q":"sf"}` {
		t.Errorf("function_call item=%v", fcItem)
	}
	if fcoItem == nil || fcoItem["call_id"] != "c1" || fcoItem["output"] != "sunny" {
		t.Errorf("function_call_output item=%v", fcoItem)
	}
}

func TestResponses_ParseUpstreamResponse_FunctionCall(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	ir, err := h.ParseUpstreamResponse([]byte(`{
		"id":"r","object":"response","status":"completed","model":"m",
		"output":[{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"q\":\"sf\"}"}],
		"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`))
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

func TestResponses_EmitInboundResponse_FunctionCall(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	ir := &ChatResponse{ID: "r", Model: "m",
		Choices: []ChatChoice{{Message: ChatMessage{
			ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: json.RawMessage(`{"q":"sf"}`)}},
		}, FinishReason: "tool_calls"}},
		PromptTokens: 5, CompletionTokens: 2}
	out, _ := h.EmitInboundResponse(ir)
	m := mustDecode(t, out)
	var fcItem map[string]any
	for _, it := range m["output"].([]any) {
		im := it.(map[string]any)
		if im["type"] == "function_call" {
			fcItem = im
		}
	}
	if fcItem == nil || fcItem["name"] != "get_weather" || fcItem["arguments"] != `{"q":"sf"}` {
		t.Errorf("function_call output=%v", fcItem)
	}
}

func TestResponses_ToolCall_Stream(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	em := h.NewInboundStreamEmitter("m")
	var chunks []StreamChunk
	emit := func(evs []StreamEvent) {
		for _, ev := range evs {
			cs, _ := em.Emit(ev)
			chunks = append(chunks, cs...)
		}
	}
	emit([]StreamEvent{{Type: EventRole, Delta: "assistant"}})
	emit([]StreamEvent{{Type: EventToolCallStart, ToolCallIndex: 0, ToolCallID: "c1", ToolCallName: "get_weather"}})
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
		if ch.Event == "response.function_call_arguments.delta" {
			if d, ok := m["delta"].(string); ok {
				args.WriteString(d)
			}
		}
	}
	if !contains(types, "response.output_item.added") {
		t.Errorf("missing output_item.added: %v", types)
	}
	if args.String() != `{"q":"sf"}` {
		t.Errorf("args=%q want {\"q\":\"sf\"}", args.String())
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestResponses_ParseInboundRequest_MissingInput(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	if _, err := h.ParseInboundRequest("", []byte(`{"model":"m"}`)); err == nil {
		t.Fatal("expected missing-input error")
	}
}

// ---- EmitUpstreamRequest (IR → Responses body) ----

func TestResponses_EmitUpstreamRequest(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	mt := 100
	tp := 0.7
	ir := &ChatRequest{Model: "gpt-4.1", System: "sys", MaxTokens: &mt, Temperature: &tp,
		Messages: []ChatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}}}
	out, err := h.EmitUpstreamRequest(ir)
	if err != nil {
		t.Fatalf("EmitUpstreamRequest: %v", err)
	}
	m := mustDecode(t, out)
	if m["model"] != "gpt-4.1" || m["instructions"] != "sys" {
		t.Errorf("model/instructions=%v/%v", m["model"], m["instructions"])
	}
	if m["max_output_tokens"] != float64(100) || m["temperature"] != 0.7 {
		t.Errorf("params=%v/%v", m["max_output_tokens"], m["temperature"])
	}
	input := m["input"].([]any)
	if input[0].(map[string]any)["role"] != "user" {
		t.Errorf("first input role=%v", input[0])
	}
	if input[0].(map[string]any)["content"].([]any)[0].(map[string]any)["type"] != "input_text" {
		t.Errorf("content type=%v", input[0])
	}
}

// ---- ParseUpstreamResponse (Responses response → IR) ----

func TestResponses_ParseUpstreamResponse(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	ir, err := h.ParseUpstreamResponse([]byte(`{
		"id":"resp_1","object":"response","status":"completed","model":"gpt-4.1",
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],
		"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`))
	if err != nil {
		t.Fatalf("ParseUpstreamResponse: %v", err)
	}
	if ir.ID != "resp_1" || ir.Model != "gpt-4.1" {
		t.Errorf("ir=%+v", ir)
	}
	if len(ir.Choices) != 1 || ir.Choices[0].Message.Content != "hello" || ir.Choices[0].FinishReason != "stop" {
		t.Errorf("choices=%+v", ir.Choices)
	}
	if ir.PromptTokens != 5 || ir.CompletionTokens != 2 {
		t.Errorf("usage=%d/%d", ir.PromptTokens, ir.CompletionTokens)
	}
}

func TestResponses_ParseUpstreamResponse_Incomplete(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	ir, _ := h.ParseUpstreamResponse([]byte(`{"id":"r","status":"incomplete","output":[{"content":[{"type":"output_text","text":"x"}]}]}`))
	if ir.Choices[0].FinishReason != "length" {
		t.Errorf("finish=%v want length", ir.Choices[0].FinishReason)
	}
}

// ---- EmitInboundResponse (IR → Responses body) ----

func TestResponses_EmitInboundResponse(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	ir := &ChatResponse{ID: "resp_1", Model: "gpt-4.1",
		Choices: []ChatChoice{{Message: ChatMessage{Content: "yo"}, FinishReason: "stop"}},
		PromptTokens: 3, CompletionTokens: 1}
	out, err := h.EmitInboundResponse(ir)
	if err != nil {
		t.Fatalf("EmitInboundResponse: %v", err)
	}
	m := mustDecode(t, out)
	if m["object"] != "response" || m["status"] != "completed" {
		t.Errorf("object/status=%v/%v", m["object"], m["status"])
	}
	item := m["output"].([]any)[0].(map[string]any)
	if item["type"] != "message" {
		t.Errorf("output type=%v", item["type"])
	}
	if item["content"].([]any)[0].(map[string]any)["text"] != "yo" {
		t.Errorf("text=%v", item["content"])
	}
	usage := m["usage"].(map[string]any)
	if usage["total_tokens"] != float64(4) {
		t.Errorf("usage=%v", usage)
	}
}

// ---- Stream parser (Responses upstream SSE → IR events) ----

func TestResponses_StreamParser(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	p := h.NewUpstreamStreamParser()

	events := []struct {
		et   string
		data string
	}{
		{"response.created", `{"type":"response.created","response":{"id":"resp_1","model":"gpt-4.1"}}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"Hel","output_index":0,"content_index":0}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"lo","output_index":0,"content_index":0}`},
		{"response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-4.1","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`},
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
		t.Fatal("expected io.EOF on response.completed")
	}
	// Expect: role, content, content, finish, usage
	wantTypes := []string{EventRole, EventContent, EventContent, EventFinish, EventUsage}
	gotTypes := make([]string, len(allEvents))
	for i, e := range allEvents {
		gotTypes[i] = e.Type
	}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("event types=%v want %v", gotTypes, wantTypes)
	}
	if allEvents[1].Delta != "Hel" || allEvents[2].Delta != "lo" {
		t.Errorf("deltas=%q %q", allEvents[1].Delta, allEvents[2].Delta)
	}
	if allEvents[3].FinishReason != "stop" {
		t.Errorf("finish=%v", allEvents[3])
	}
	if allEvents[4].PromptTokens != 5 || allEvents[4].CompletionTokens != 2 {
		t.Errorf("usage=%+v", allEvents[4])
	}
}

// ---- Stream emitter (IR events → Responses SSE) ----

func TestResponses_StreamEmitter(t *testing.T) {
	h, _ := GetHandler(FormatOpenAIResponse)
	em := h.NewInboundStreamEmitter("gpt-4.1")

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
	chunks = append(chunks, em.Done()...)

	// Collect event types in order.
	var types []string
	var deltas []string
	for _, ch := range chunks {
		if ch.Event != "" {
			types = append(types, ch.Event)
		}
		if ch.Event == "response.output_text.delta" {
			var m map[string]any
			json.Unmarshal(ch.Data, &m)
			deltas = append(deltas, m["delta"].(string))
		}
	}
	want := []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if !reflect.DeepEqual(types, want) {
		t.Errorf("event types=%v\nwant %v", types, want)
	}
	if !reflect.DeepEqual(deltas, []string{"Hel", "lo"}) {
		t.Errorf("deltas=%v want [Hel lo]", deltas)
	}

	// The terminal response.completed carries usage.
	last := chunks[len(chunks)-1]
	var m map[string]any
	json.Unmarshal(last.Data, &m)
	resp := m["response"].(map[string]any)
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(2) {
		t.Errorf("usage=%v", usage)
	}
}
