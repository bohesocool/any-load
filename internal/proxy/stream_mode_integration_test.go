package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- Stream mode: fake non-stream (upstream streams, client gets one JSON) ----

func TestStreamMode_FakeNonStream_ChatToAnthropic(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeFrame := func(event, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeFrame("message_start", `{"type":"message_start","message":{"id":"msg_1","model":"test-model","usage":{"input_tokens":5,"output_tokens":1}}}`)
		writeFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`)
		writeFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`)
		writeFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`)
		writeFrame("message_stop", `{"type":"message_stop"}`)
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "anthropic", `["anthropic"]`, withStreamMode("fake_non_stream"))
	defer env.cleanup()

	// Client requests NON-stream.
	w := env.post(t, "/v1/chat/completions",
		[]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Upstream was forced to stream.
	if gotBody["stream"] != true {
		t.Errorf("upstream stream=%v want true", gotBody["stream"])
	}
	// Client received a single non-stream OpenAI Chat JSON (not SSE).
	contentType := w.Header().Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		t.Errorf("client got SSE stream, want non-stream JSON (Content-Type=%s)", contentType)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal client response: %v body=%s", err, w.Body.String())
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v want chat.completion", resp["object"])
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v want stop", choice["finish_reason"])
	}
	if choice["message"].(map[string]any)["content"] != "Hello" {
		t.Errorf("content=%v want Hello", choice["message"].(map[string]any)["content"])
	}
}

// ---- Stream mode: fake stream (upstream non-stream, client gets SSE) ----

func TestStreamMode_FakeStream_AnthropicToChat(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl_1",
			"object":  "chat.completion",
			"model":   "test-model",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "Hello world"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2},
		})
	}))
	defer upstream.Close()

	// Group channel type anthropic, upstream formats openai-chat: inbound
	// anthropic → upstream openai-chat. Client requests STREAM.
	env := newTestEnv(t, upstream.URL, "anthropic", `["openai-chat"]`, withStreamMode("fake_stream"))
	defer env.cleanup()

	w := env.post(t, "/v1/messages",
		[]byte(`{"model":"test-model","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Upstream was forced to non-stream.
	if gotBody["stream"] != false {
		t.Errorf("upstream stream=%v want false", gotBody["stream"])
	}
	// Client received an SSE stream (Anthropic format) ending with message_stop.
	contentType := w.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/event-stream") {
		t.Errorf("client Content-Type=%s want text/event-stream", contentType)
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: message_start") {
		t.Errorf("client SSE missing message_start: %s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Errorf("client SSE missing message_stop (terminal frame): %s", body)
	}
	// Concatenate text deltas to verify content survived the non-stream→stream
	// expansion.
	var gotText strings.Builder
	for _, frame := range bytes.Split([]byte(body), []byte("\n\n")) {
		for _, line := range bytes.Split(frame, []byte("\n")) {
			ln := strings.TrimRight(string(line), "\r")
			if strings.HasPrefix(ln, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(ln, "data:"))
				var ev map[string]any
				if json.Unmarshal([]byte(payload), &ev) == nil {
					if d, _ := ev["delta"].(map[string]any); d != nil {
						if txt, _ := d["text"].(string); txt != "" {
							gotText.WriteString(txt)
						}
					}
				}
			}
		}
	}
	if gotText.String() != "Hello world" {
		t.Errorf("expanded content=%q want Hello world", gotText.String())
	}
}

// ---- Stream mode: Gemini multi-tool index-reuse regression (fake non-stream) ----
//
// Gemini's stream parser uses the parts-array index as the tool-call index and
// resets it to 0 on every SSE chunk. Two tool calls delivered across two chunks
// both carry index 0; a naive map[index] accumulator would overwrite the first
// with the second. This test verifies the accumulator keeps both.

func TestStreamMode_FakeNonStream_GeminiMultiTool(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeFrame := func(data string) {
			fmt.Fprintf(w, "data: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Upstream format is openai-chat, so emit OpenAI Chat SSE chunks with
		// two parallel tool calls at index 0 and 1. (This tests the accumulator
		// distinguishing tool calls by index; OpenAI uses stable unique indices,
		// so this is the openai-chat upstream variant. The Gemini index-reuse
		// case is covered by stream_convert_test.go's GeminiIndexReuse test
		// which feeds the parser events directly.)
		writeFrame(`{"id":"x","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
		writeFrame(`{"id":"x","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"callA","type":"function","function":{"name":"getWeather","arguments":""}}]},"finish_reason":null}]}`)
		writeFrame(`{"id":"x","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"NYC\"}"}}]},"finish_reason":null}]}`)
		writeFrame(`{"id":"x","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"callB","type":"function","function":{"name":"getTime","arguments":""}}]},"finish_reason":null}]}`)
		writeFrame(`{"id":"x","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"tz\":\"UTC\"}"}}]},"finish_reason":null}]}`)
		writeFrame(`{"id":"x","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		writeFrame(`[DONE]`)
	}))
	defer upstream.Close()

	// Inbound gemini → upstream openai-chat (forces conversion, so conv != nil),
	// and fake_non_stream so upstream streams. The accumulated IR (two tool
	// calls) is emitted back to the client in the inbound (gemini) format as a
	// single non-stream response with two functionCall parts.
	env := newTestEnv(t, upstream.URL, "gemini", `["openai-chat"]`, withStreamMode("fake_non_stream"))
	defer env.cleanup()

	// Client sends a non-stream Gemini request.
	w := env.post(t, "/v1beta/models/gemini-test:generateContent",
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"weather and time"}]}],"tools":[{"functionDeclarations":[{"name":"getWeather","parameters":{}},{"name":"getTime","parameters":{}}]}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Upstream forced to stream (openai-chat signals via body stream:true).
	if gotBody["stream"] != true {
		t.Errorf("upstream stream=%v want true", gotBody["stream"])
	}
	// Client received a single non-stream Gemini JSON with TWO functionCall parts.
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal client response: %v body=%s", err, w.Body.String())
	}
	candidates, _ := resp["candidates"].([]any)
	if len(candidates) == 0 {
		t.Fatalf("no candidates in response: %s", w.Body.String())
	}
	c, _ := candidates[0].(map[string]any)
	content, _ := c["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	var fnNames []string
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if fc, _ := pm["functionCall"].(map[string]any); fc != nil {
			if name, ok := fc["name"].(string); ok {
				fnNames = append(fnNames, name)
			}
		}
	}
	if len(fnNames) != 2 {
		t.Fatalf("function calls=%d want 2 (must not collapse), names=%v body=%s", len(fnNames), fnNames, w.Body.String())
	}
	if fnNames[0] != "getWeather" || fnNames[1] != "getTime" {
		t.Errorf("function call names=%v want [getWeather getTime]", fnNames)
	}
}

// ---- Stream mode: passthrough unchanged (regression guard) ----

func TestStreamMode_Passthrough_StreamUnchanged(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeFrame := func(data string) {
			fmt.Fprintf(w, "data: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeFrame(`{"id":"x","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
		writeFrame(`{"id":"x","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`)
		writeFrame(`{"id":"x","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		writeFrame(`[DONE]`)
	}))
	defer upstream.Close()

	// Explicit passthrough mode + client stream → upstream stream (unchanged).
	env := newTestEnv(t, upstream.URL, "anthropic", `["openai-chat"]`, withStreamMode("passthrough"))
	defer env.cleanup()

	w := env.post(t, "/v1/messages",
		[]byte(`{"model":"test-model","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotBody["stream"] != true {
		t.Errorf("upstream stream=%v want true (passthrough keeps client intent)", gotBody["stream"])
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Errorf("client got non-stream in passthrough: %s", w.Header().Get("Content-Type"))
	}
	// Anthropic inbound stream terminates with message_stop (not [DONE], which
	// is the OpenAI Chat terminator).
	if !strings.Contains(w.Body.String(), "event: message_stop") {
		t.Errorf("client SSE missing message_stop terminal frame: %s", w.Body.String())
	}
}
