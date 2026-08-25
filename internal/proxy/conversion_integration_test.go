package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"any-load/internal/affinity"
	"any-load/internal/channel"
	"any-load/internal/config"
	"any-load/internal/encryption"
	"any-load/internal/httpclient"
	"any-load/internal/keypool"
	"any-load/internal/models"
	"any-load/internal/protocol"
	"any-load/internal/services"
	"any-load/internal/store"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// testEnv bundles a proxy server, gin engine, and the group manager so a test
// can drive requests through HandleProxy against a mock upstream.
type testEnv struct {
	ps        *ProxyServer
	engine    *gin.Engine
	groupMgr  *services.GroupManager
	groupID   uint
	cleanup   func()
}

// sqliteDSN returns a per-test in-memory SQLite DSN so concurrent/serial tests
// don't collide on the shared `file::memory:?cache=shared` database. Each test
// gets its own private in-memory DB tied to the test name.
func sqliteDSN(t *testing.T) string {
	t.Helper()
	// Sanitize the test name into a unique in-memory file.
	name := strings.ReplaceAll(t.Name(), "/", "_")
	return fmt.Sprintf("file:%s?mode=memory&cache=shared", name)
}

// testEnvOption configures a test group beyond the defaults in newTestEnv.
type testEnvOption func(*models.Group)

// withStreamMode sets the group's StreamMode (default is empty/passthrough).
func withStreamMode(mode string) testEnvOption {
	return func(g *models.Group) { g.StreamMode = mode }
}

// newTestEnv builds a ProxyServer wired to a mock upstream. channelType is the
// group's ChannelType (a registered channel type, e.g. "openai"/"anthropic");
// upstreamFormatsJSON is the group's UpstreamFormats (e.g. `["anthropic"]`).
// ProtocolConversion is forced on.
func newTestEnv(t *testing.T, upstreamURL, channelType, upstreamFormatsJSON string, opts ...testEnvOption) *testEnv {
	t.Helper()
	memStore := store.NewMemoryStore()
	encSvc, _ := encryption.NewService("") // noop: encrypt/decrypt are identity

	gormDB, err := gorm.Open(sqlite.Open(sqliteDSN(t)), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	if err := gormDB.AutoMigrate(&models.Group{}, &models.GroupSubGroup{}, &models.APIKey{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	upstreams, _ := json.Marshal([]map[string]any{{"url": upstreamURL, "weight": 1}})
	group := &models.Group{
		Name:                "conv-test",
		GroupType:           "standard",
		ChannelType:         channelType,
		Upstreams:           datatypes.JSON(upstreams),
		TestModel:           "test-model",
		ProtocolConversion:  true,
		UpstreamFormats:     datatypes.JSON(upstreamFormatsJSON),
	}
	for _, opt := range opts {
		opt(group)
	}
	if err := gormDB.Create(group).Error; err != nil {
		t.Fatalf("create group: %v", err)
	}

	settingsMgr := config.NewSystemSettingsManager() // uninitialized → defaults (MaxConcurrencyPerKey=0)
	subGroupMgr := services.NewSubGroupManager(memStore)
	groupMgr := services.NewGroupManager(gormDB, memStore, settingsMgr, subGroupMgr)
	if err := groupMgr.Initialize(); err != nil {
		t.Fatalf("gm init: %v", err)
	}

	// Seed one active key directly in the store (no DB row needed for the
	// success path; noopService decrypt is identity).
	const keyID uint = 1
	const keyValue = "secret-upstream-key"
	memStore.HSet(fmt.Sprintf("key:%d", keyID), map[string]any{
		"id":            fmt.Sprint(keyID),
		"key_string":    keyValue,
		"status":        models.KeyStatusActive,
		"failure_count": 0,
		"group_id":      group.ID,
		"created_at":    time.Now().Unix(),
	})
	memStore.LPush(fmt.Sprintf("group:%d:active_keys", group.ID), keyID)

	keyProvider := keypool.NewProvider(nil, memStore, nil, encSvc)
	clientMgr := httpclient.NewHTTPClientManager()
	factory := channel.NewFactory(settingsMgr, clientMgr)
	affMgr := affinity.NewManager(memStore)

	ps, err := NewProxyServer(keyProvider, groupMgr, subGroupMgr, settingsMgr, factory, nil, encSvc, affMgr)
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Any("/proxy/:group_name/*path", ps.HandleProxy)

	return &testEnv{
		ps:       ps,
		engine:   engine,
		groupMgr: groupMgr,
		groupID:  group.ID,
		cleanup: func() {
			groupMgr.Stop(context.Background())
			sqlDB, _ := gormDB.DB()
			if sqlDB != nil {
				sqlDB.Close()
			}
		},
	}
}

// post sends an inbound request to /proxy/conv-test<path> and returns the
// recorded response.
func (e *testEnv) post(t *testing.T, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/proxy/conv-test"+path, io.NopCloser(bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	return w
}

// parseSSEData extracts the `data:` payloads (concatenated per frame) from an
// SSE response body.
func parseSSEData(body []byte) []string {
	var out []string
	for _, frame := range bytes.Split(body, []byte("\n\n")) {
		var data strings.Builder
		for _, line := range bytes.Split(frame, []byte("\n")) {
			ln := strings.TrimRight(string(line), "\r")
			if strings.HasPrefix(ln, "data:") {
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(ln, "data:")))
			}
		}
		if data.Len() > 0 {
			out = append(out, data.String())
		}
	}
	return out
}

// ---- Scenario 1: OpenAI Chat inbound → Anthropic upstream (non-stream) ----

func TestConversion_ChatToAnthropic_NonStream(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "msg_1",
			"type":   "message",
			"role":   "assistant",
			"model":  "test-model",
			"content": []map[string]any{{"type": "text", "text": "hello"}},
			"stop_reason": "end_turn",
			"usage":  map[string]any{"input_tokens": 5, "output_tokens": 1},
		})
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "anthropic", `["anthropic"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/chat/completions",
		[]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Upstream received an Anthropic Messages request.
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path=%q want /v1/messages", gotPath)
	}
	if gotAuth != "secret-upstream-key" {
		t.Errorf("x-api-key=%q want secret-upstream-key", gotAuth)
	}
	if gotBody["model"] != "test-model" {
		t.Errorf("upstream model=%v", gotBody["model"])
	}
	if _, ok := gotBody["max_tokens"]; !ok {
		t.Errorf("upstream body missing required max_tokens: %v", gotBody)
	}

	// Client received an OpenAI Chat Completions response.
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal client response: %v body=%s", err, w.Body.String())
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v want chat.completion", resp["object"])
	}
	choices := resp["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v want stop", choice["finish_reason"])
	}
	if choice["message"].(map[string]any)["content"] != "hello" {
		t.Errorf("content=%v want hello", choice["message"])
	}
}

// ---- Scenario 2: OpenAI Chat inbound → Anthropic upstream (stream) ----

func TestConversion_ChatToAnthropic_Stream(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
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

	env := newTestEnv(t, upstream.URL, "anthropic", `["anthropic"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/chat/completions",
		[]byte(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path=%q want /v1/messages", gotPath)
	}
	if gotBody["stream"] != true {
		t.Errorf("upstream stream=%v want true", gotBody["stream"])
	}

	// Client received OpenAI Chat SSE chunks + terminal [DONE].
	frames := parseSSEData(w.Body.Bytes())
	if len(frames) < 3 {
		t.Fatalf("frames=%d, want >=3: %v", len(frames), frames)
	}
	// First chunk carries the assistant role.
	var first map[string]any
	json.Unmarshal([]byte(frames[0]), &first)
	choice := first["choices"].([]any)[0].(map[string]any)
	if choice["delta"].(map[string]any)["role"] != "assistant" {
		t.Errorf("first chunk delta=%v want role assistant", choice["delta"])
	}
	// Concatenated content deltas should equal "Hello".
	var content strings.Builder
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(f), &m) != nil {
			continue
		}
		if chs, ok := m["choices"].([]any); ok && len(chs) > 0 {
			if d, ok := chs[0].(map[string]any)["delta"].(map[string]any); ok {
				if c, ok := d["content"].(string); ok {
					content.WriteString(c)
				}
			}
		}
	}
	if content.String() != "Hello" {
		t.Errorf("streamed content=%q want Hello", content.String())
	}
	// Must end with [DONE].
	if frames[len(frames)-1] != "[DONE]" {
		t.Errorf("last frame=%q want [DONE]", frames[len(frames)-1])
	}
}

// ---- Scenario 3: Anthropic inbound → OpenAI Chat upstream (non-stream) ----

func TestConversion_AnthropicToChat_NonStream(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "chat_1",
			"model": "test-model",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "yo"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 4, "completion_tokens": 1},
		})
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "openai", `["openai-chat"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/messages",
		[]byte(`{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Upstream received an OpenAI Chat request.
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path=%q want /v1/chat/completions", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("Authorization=%q want Bearer ...", gotAuth)
	}
	if gotBody["model"] != "test-model" {
		t.Errorf("upstream model=%v", gotBody["model"])
	}

	// Client received an Anthropic Messages response.
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal client response: %v body=%s", err, w.Body.String())
	}
	if resp["type"] != "message" {
		t.Errorf("type=%v want message", resp["type"])
	}
	if resp["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v want end_turn", resp["stop_reason"])
	}
	blocks := resp["content"].([]any)
	if blocks[0].(map[string]any)["text"] != "yo" {
		t.Errorf("content=%v want yo", blocks)
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(1) {
		t.Errorf("usage=%v", usage)
	}
}

// ---- Scenario 4: Anthropic inbound → OpenAI Chat upstream (stream) ----

func TestConversion_AnthropicToChat_Stream(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeData := func(data string) {
			fmt.Fprintf(w, "data: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeData(`{"id":"chat_1","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
		writeData(`{"id":"chat_1","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`)
		writeData(`{"id":"chat_1","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`)
		writeData(`{"id":"chat_1","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		writeData(`[DONE]`)
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "openai", `["openai-chat"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/messages",
		[]byte(`{"model":"test-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path=%q want /v1/chat/completions", gotPath)
	}
	if gotBody["stream"] != true {
		t.Errorf("upstream stream=%v want true", gotBody["stream"])
	}

	// Client received Anthropic SSE: message_start, content_block_delta(s),
	// message_delta, message_stop.
	var eventTypes []string
	for _, frame := range bytes.Split(w.Body.Bytes(), []byte("\n\n")) {
		var et string
		for _, line := range bytes.Split(frame, []byte("\n")) {
			ln := strings.TrimRight(string(line), "\r")
			if strings.HasPrefix(ln, "event:") {
				et = strings.TrimSpace(strings.TrimPrefix(ln, "event:"))
			}
		}
		if et != "" {
			eventTypes = append(eventTypes, et)
		}
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if !equalSlices(eventTypes, want) {
		t.Errorf("anthropic SSE events=%v\nwant %v", eventTypes, want)
	}

	// Concatenated text deltas should be "Hello".
	var content strings.Builder
	for _, frame := range bytes.Split(w.Body.Bytes(), []byte("\n\n")) {
		var data strings.Builder
		for _, line := range bytes.Split(frame, []byte("\n")) {
			ln := strings.TrimRight(string(line), "\r")
			if strings.HasPrefix(ln, "data:") {
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(ln, "data:")))
			}
		}
		if data.Len() == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(data.String()), &m) != nil {
			continue
		}
		if m["type"] == "content_block_delta" {
			if d, ok := m["delta"].(map[string]any); ok {
				if t, ok := d["text"].(string); ok {
					content.WriteString(t)
				}
			}
		}
	}
	if content.String() != "Hello" {
		t.Errorf("streamed content=%q want Hello", content.String())
	}
}

// ---- Scenario 5: Smart passthrough (inbound format already upstream-supported) ----

func TestConversion_SmartPassthrough(t *testing.T) {
	var gotPath string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"raw"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	// openai-chat is in the list → inbound chat is passed through unchanged.
	env := newTestEnv(t, upstream.URL, "openai", `["openai-chat","anthropic"]`)
	defer env.cleanup()

	inbound := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	w := env.post(t, "/v1/chat/completions", inbound)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path=%q want /v1/chat/completions (passthrough)", gotPath)
	}
	// Body forwarded byte-for-byte (no translation).
	if string(gotBody) != string(inbound) {
		t.Errorf("upstream body differs from inbound:\n got: %s\n want: %s", gotBody, inbound)
	}
}

// ---- Scenario 6: disabled → pure passthrough regardless of path ----

func TestConversion_DisabledPassthrough(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	// Build an env then flip ProtocolConversion off by recreating the group.
	memStore := store.NewMemoryStore()
	encSvc, _ := encryption.NewService("")
	gormDB, err := gorm.Open(sqlite.Open(sqliteDSN(t)), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	gormDB.AutoMigrate(&models.Group{}, &models.GroupSubGroup{}, &models.APIKey{})
	upstreams, _ := json.Marshal([]map[string]any{{"url": upstream.URL, "weight": 1}})
	group := &models.Group{
		Name: "conv-test", GroupType: "standard", ChannelType: "openai",
		Upstreams: datatypes.JSON(upstreams), TestModel: "test-model",
		ProtocolConversion: false, UpstreamFormats: datatypes.JSON(`["anthropic"]`),
	}
	gormDB.Create(group)
	settingsMgr := config.NewSystemSettingsManager()
	subGroupMgr := services.NewSubGroupManager(memStore)
	groupMgr := services.NewGroupManager(gormDB, memStore, settingsMgr, subGroupMgr)
	groupMgr.Initialize()
	defer groupMgr.Stop(context.Background())

	// Seed one active key in the store.
	memStore.HSet(fmt.Sprintf("key:%d", 1), map[string]any{
		"id": "1", "key_string": "secret-upstream-key", "status": models.KeyStatusActive,
		"failure_count": 0, "group_id": group.ID, "created_at": time.Now().Unix(),
	})
	memStore.LPush(fmt.Sprintf("group:%d:active_keys", group.ID), 1)

	keyProvider := keypool.NewProvider(nil, memStore, nil, encSvc)
	clientMgr := httpclient.NewHTTPClientManager()
	factory := channel.NewFactory(settingsMgr, clientMgr)
	affMgr := affinity.NewManager(memStore)
	ps, _ := NewProxyServer(keyProvider, groupMgr, subGroupMgr, settingsMgr, factory, nil, encSvc, affMgr)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Any("/proxy/:group_name/*path", ps.HandleProxy)

	// Inbound chat request with a chat-incompatible upstream format list, but
	// conversion is OFF → must passthrough to /v1/chat/completions unchanged.
	req := httptest.NewRequest(http.MethodPost, "/proxy/conv-test/v1/chat/completions",
		io.NopCloser(bytes.NewReader([]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path=%q want /v1/chat/completions (passthrough)", gotPath)
	}
}

// ---- Scenario 7: OpenAI Chat inbound → Gemini upstream (non-stream) ----

func TestConversion_ChatToGemini_NonStream(t *testing.T) {
	var gotPath, gotKey string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.URL.Query().Get("key")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content":      map[string]any{"role": "model", "parts": []map[string]any{{"text": "hello"}}},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{"promptTokenCount": 5, "candidatesTokenCount": 1, "totalTokenCount": 6},
			"modelVersion":  "test-model",
		})
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "gemini", `["gemini"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/chat/completions",
		[]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Upstream path embeds the model; auth via ?key=.
	if gotPath != "/v1beta/models/test-model:generateContent" {
		t.Errorf("upstream path=%q want /v1beta/models/test-model:generateContent", gotPath)
	}
	if gotKey != "secret-upstream-key" {
		t.Errorf("key=%q want secret-upstream-key", gotKey)
	}
	// Upstream received a Gemini contents body.
	if gotBody["contents"] == nil {
		t.Errorf("upstream body missing contents: %v", gotBody)
	}

	// Client received an OpenAI Chat response.
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "hello" {
		t.Errorf("content=%v", choice["message"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v want stop", choice["finish_reason"])
	}
}

// ---- Scenario 8: OpenAI Chat inbound → Gemini upstream (stream) ----

func TestConversion_ChatToGemini_Stream(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeData := func(data string) {
			fmt.Fprintf(w, "data: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]},"finishReason":null}]}`)
		writeData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":null}]}`)
		writeData(`{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}`)
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "gemini", `["gemini"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/chat/completions",
		[]byte(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/test-model:streamGenerateContent" {
		t.Errorf("upstream path=%q want :streamGenerateContent", gotPath)
	}

	// Client received OpenAI Chat SSE; concatenated content = "Hello", ends [DONE].
	frames := parseSSEData(w.Body.Bytes())
	var content strings.Builder
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(f), &m) != nil {
			continue
		}
		if chs, ok := m["choices"].([]any); ok && len(chs) > 0 {
			if d, ok := chs[0].(map[string]any)["delta"].(map[string]any); ok {
				if c, ok := d["content"].(string); ok {
					content.WriteString(c)
				}
			}
		}
	}
	if content.String() != "Hello" {
		t.Errorf("content=%q want Hello", content.String())
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Errorf("last frame=%q want [DONE]", frames[len(frames)-1])
	}
}

// ---- Scenario 9: Gemini inbound → OpenAI Chat upstream (non-stream) ----

func TestConversion_GeminiToChat_NonStream(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "chat_1",
			"model": "test-model",
			"choices": []map[string]any{{
				"index": 0, "message": map[string]any{"role": "assistant", "content": "yo"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 4, "completion_tokens": 1},
		})
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "openai", `["openai-chat"]`)
	defer env.cleanup()

	// Gemini inbound request: model in path, contents in body.
	w := env.post(t, "/v1beta/models/test-model:generateContent",
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":50}}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path=%q want /v1/chat/completions", gotPath)
	}
	// Upstream received OpenAI Chat body with the model extracted from the path.
	if gotBody["model"] != "test-model" {
		t.Errorf("upstream model=%v want test-model", gotBody["model"])
	}

	// Client received a Gemini generateContent response.
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	cands := resp["candidates"].([]any)
	c := cands[0].(map[string]any)
	if c["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] != "yo" {
		t.Errorf("text=%v", c["content"])
	}
	if c["finishReason"] != "STOP" {
		t.Errorf("finishReason=%v want STOP", c["finishReason"])
	}
	usage := resp["usageMetadata"].(map[string]any)
	if usage["promptTokenCount"] != float64(4) || usage["candidatesTokenCount"] != float64(1) {
		t.Errorf("usage=%v", usage)
	}
}

// ---- Scenario 10: Gemini inbound → OpenAI Chat upstream (stream) ----

func TestConversion_GeminiToChat_Stream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeData := func(data string) {
			fmt.Fprintf(w, "data: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeData(`{"id":"c","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
		writeData(`{"id":"c","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`)
		writeData(`{"id":"c","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`)
		writeData(`{"id":"c","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		writeData(`[DONE]`)
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "openai", `["openai-chat"]`)
	defer env.cleanup()

	// Gemini inbound streaming path.
	w := env.post(t, "/v1beta/models/test-model:streamGenerateContent",
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	// Client received Gemini SSE chunks: each data: is a GenerateContentResponse.
	frames := parseSSEData(w.Body.Bytes())
	var content strings.Builder
	for _, f := range frames {
		var m map[string]any
		if json.Unmarshal([]byte(f), &m) != nil {
			continue
		}
		if cands, ok := m["candidates"].([]any); ok && len(cands) > 0 {
			c := cands[0].(map[string]any)
			if contentObj, ok := c["content"].(map[string]any); ok {
				for _, part := range contentObj["parts"].([]any) {
					if t, ok := part.(map[string]any)["text"].(string); ok {
						content.WriteString(t)
					}
				}
			}
		}
	}
	if content.String() != "Hello" {
		t.Errorf("content=%q want Hello", content.String())
	}
}

// ---- Scenario 11: OpenAI Chat inbound → Responses upstream (non-stream) ----

func TestConversion_ChatToResponses_NonStream(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_1",
			"object": "response",
			"status": "completed",
			"model":  "test-model",
			"output": []map[string]any{{
				"type": "message", "role": "assistant", "status": "completed",
				"content": []map[string]any{{"type": "output_text", "text": "hello"}},
			}},
			"usage": map[string]any{"input_tokens": 5, "output_tokens": 1, "total_tokens": 6},
		})
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "openai-response", `["openai-response"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/chat/completions",
		[]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/responses" {
		t.Errorf("upstream path=%q want /v1/responses", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("Authorization=%q want Bearer ...", gotAuth)
	}
	// Upstream received a Responses body: input array + instructions.
	if gotBody["input"] == nil {
		t.Errorf("upstream missing input: %v", gotBody)
	}

	// Client received an OpenAI Chat response.
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "hello" {
		t.Errorf("content=%v", choice["message"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v want stop", choice["finish_reason"])
	}
}

// ---- Scenario 12: OpenAI Chat inbound → Responses upstream (stream) ----

func TestConversion_ChatToResponses_Stream(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeFrame := func(event, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeFrame("response.created", `{"type":"response.created","response":{"id":"resp_1","model":"test-model"}}`)
		writeFrame("response.output_text.delta", `{"type":"response.output_text.delta","delta":"Hel","output_index":0,"content_index":0}`)
		writeFrame("response.output_text.delta", `{"type":"response.output_text.delta","delta":"lo","output_index":0,"content_index":0}`)
		writeFrame("response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"test-model","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "openai-response", `["openai-response"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/chat/completions",
		[]byte(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/responses" {
		t.Errorf("upstream path=%q want /v1/responses", gotPath)
	}

	// Client received OpenAI Chat SSE; content = "Hello", ends [DONE].
	frames := parseSSEData(w.Body.Bytes())
	var content strings.Builder
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(f), &m) != nil {
			continue
		}
		if chs, ok := m["choices"].([]any); ok && len(chs) > 0 {
			if d, ok := chs[0].(map[string]any)["delta"].(map[string]any); ok {
				if c, ok := d["content"].(string); ok {
					content.WriteString(c)
				}
			}
		}
	}
	if content.String() != "Hello" {
		t.Errorf("content=%q want Hello", content.String())
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Errorf("last frame=%q want [DONE]", frames[len(frames)-1])
	}
}

// ---- Scenario 13: Responses inbound → OpenAI Chat upstream (non-stream) ----

func TestConversion_ResponsesToChat_NonStream(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "chat_1",
			"model": "test-model",
			"choices": []map[string]any{{
				"index": 0, "message": map[string]any{"role": "assistant", "content": "yo"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 4, "completion_tokens": 1},
		})
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "openai", `["openai-chat"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/responses",
		[]byte(`{"model":"test-model","input":"hi","max_output_tokens":50}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path=%q want /v1/chat/completions", gotPath)
	}
	// Upstream received an OpenAI Chat body.
	if gotBody["model"] != "test-model" {
		t.Errorf("upstream model=%v", gotBody["model"])
	}

	// Client received a Responses-format response.
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("object/status=%v/%v", resp["object"], resp["status"])
	}
	item := resp["output"].([]any)[0].(map[string]any)
	if item["content"].([]any)[0].(map[string]any)["text"] != "yo" {
		t.Errorf("text=%v", item["content"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["total_tokens"] != float64(5) {
		t.Errorf("usage=%v", usage)
	}
}

// ---- Scenario 14: Responses inbound → OpenAI Chat upstream (stream) ----

func TestConversion_ResponsesToChat_Stream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeData := func(data string) {
			fmt.Fprintf(w, "data: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeData(`{"id":"c","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
		writeData(`{"id":"c","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`)
		writeData(`{"id":"c","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`)
		writeData(`{"id":"c","object":"chat.completion.chunk","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		writeData(`[DONE]`)
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "openai", `["openai-chat"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/responses",
		[]byte(`{"model":"test-model","input":"hi","stream":true}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	// Client received Responses SSE: response.created, response.output_text.delta(s),
	// ..., response.completed.
	var eventTypes []string
	var content strings.Builder
	for _, frame := range bytes.Split(w.Body.Bytes(), []byte("\n\n")) {
		var et string
		var data strings.Builder
		for _, line := range bytes.Split(frame, []byte("\n")) {
			ln := strings.TrimRight(string(line), "\r")
			if strings.HasPrefix(ln, "event:") {
				et = strings.TrimSpace(strings.TrimPrefix(ln, "event:"))
			} else if strings.HasPrefix(ln, "data:") {
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(ln, "data:")))
			}
		}
		if et != "" {
			eventTypes = append(eventTypes, et)
		}
		if data.Len() == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(data.String()), &m) != nil {
			continue
		}
		if et == "response.output_text.delta" {
			if d, ok := m["delta"].(string); ok {
				content.WriteString(d)
			}
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
	if !equalSlices(eventTypes, want) {
		t.Errorf("responses SSE events=%v\nwant %v", eventTypes, want)
	}
	if content.String() != "Hello" {
		t.Errorf("content=%q want Hello", content.String())
	}
}

// ---- Scenario 15: Chat → Anthropic with tools (non-stream) ----

func TestConversion_ChatToAnthropic_Tools_NonStream(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		// Upstream returns a tool_use response.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "test-model",
			"content": []map[string]any{{"type": "tool_use", "id": "u1", "name": "get_weather", "input": map[string]any{"q": "sf"}}},
			"stop_reason": "tool_use",
			"usage": map[string]any{"input_tokens": 5, "output_tokens": 2},
		})
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "anthropic", `["anthropic"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/chat/completions", []byte(`{
		"model":"test-model",
		"messages":[{"role":"user","content":"weather?"}],
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Upstream received Anthropic tools (input_schema).
	if gotBody["tools"] == nil {
		t.Errorf("upstream missing tools: %v", gotBody)
	}
	// Client received OpenAI Chat tool_calls.
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish=%v want tool_calls", choice["finish_reason"])
	}
	tcs := choice["message"].(map[string]any)["tool_calls"].([]any)
	if tcs[0].(map[string]any)["function"].(map[string]any)["name"] != "get_weather" {
		t.Errorf("tool_call=%v", tcs[0])
	}
}

// ---- Scenario 16: Chat → Anthropic with tools (stream, input_json_delta) ----

func TestConversion_ChatToAnthropic_Tools_Stream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		wf := func(event, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		wf("message_start", `{"type":"message_start","message":{"id":"m","model":"test-model","usage":{"input_tokens":5,"output_tokens":1}}}`)
		wf("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"u1","name":"get_weather","input":{}}}`)
		wf("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"sf\"}"}}`)
		wf("content_block_stop", `{"type":"content_block_stop","index":0}`)
		wf("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`)
		wf("message_stop", `{"type":"message_stop"}`)
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "anthropic", `["anthropic"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/chat/completions", []byte(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"weather?"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Client received OpenAI Chat SSE with tool_calls deltas; args = {"q":"sf"}.
	var args strings.Builder
	for _, f := range parseSSEData(w.Body.Bytes()) {
		if f == "[DONE]" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(f), &m) != nil {
			continue
		}
		if chs, ok := m["choices"].([]any); ok && len(chs) > 0 {
			if d, ok := chs[0].(map[string]any)["delta"].(map[string]any); ok {
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					if a, ok := tcs[0].(map[string]any)["function"].(map[string]any)["arguments"].(string); ok {
						args.WriteString(a)
					}
				}
			}
		}
	}
	if args.String() != `{"q":"sf"}` {
		t.Errorf("streamed tool args=%q want {\"q\":\"sf\"}", args.String())
	}
}

// ---- Scenario 17: Anthropic → Chat with tool_result round-trip (non-stream) ----

func TestConversion_AnthropicToChat_ToolResult_NonStream(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "openai", `["openai-chat"]`)
	defer env.cleanup()

	// Anthropic inbound: user asks, assistant tool_use, user tool_result.
	w := env.post(t, "/v1/messages", []byte(`{"model":"test-model","max_tokens":100,"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":[{"type":"tool_use","id":"u1","name":"get_weather","input":{"q":"sf"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"u1","content":"sunny"}]}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Upstream (chat) received a role="tool" message carrying tool_call_id.
	msgs := gotBody["messages"].([]any)
	var toolMsg map[string]any
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "tool" {
			toolMsg = mm
		}
	}
	if toolMsg == nil || toolMsg["tool_call_id"] != "u1" || toolMsg["content"] != "sunny" {
		t.Errorf("tool message=%v", toolMsg)
	}
}

// ---- Scenario 18: Chat → Gemini with base64 image (non-stream) ----

func TestConversion_ChatToGemini_Image_NonStream(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"a cat"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`))
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "gemini", `["gemini"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/chat/completions", []byte(`{"model":"test-model","messages":[{"role":"user","content":[
		{"type":"text","text":"what?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD="}}]}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Upstream received Gemini inline_data.
	contents := gotBody["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	var hasInlineData bool
	for _, p := range parts {
		if p.(map[string]any)["inline_data"] != nil {
			hasInlineData = true
		}
	}
	if !hasInlineData {
		t.Errorf("upstream missing inline_data: %v", parts)
	}
	// Client got OpenAI Chat response with text.
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "a cat" {
		t.Errorf("content=%v", resp["choices"])
	}
}

// ---- Scenario 19: Chat → Gemini with tools (functionCall response → tool_calls) ----

func TestConversion_ChatToGemini_Tools_NonStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"q":"sf"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}`))
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "gemini", `["gemini"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/chat/completions", []byte(`{"model":"test-model","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish=%v want tool_calls", choice["finish_reason"])
	}
	tcs := choice["message"].(map[string]any)["tool_calls"].([]any)
	if tcs[0].(map[string]any)["function"].(map[string]any)["name"] != "get_weather" {
		t.Errorf("tool_call=%v", tcs[0])
	}
}

// ---- Scenario 20: Gemini → Chat with functionResponse inbound ----

func TestConversion_GeminiToChat_FunctionResponse_NonStream(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "openai", `["openai-chat"]`)
	defer env.cleanup()

	w := env.post(t, "/v1beta/models/test-model:generateContent", []byte(`{"contents":[
		{"role":"user","parts":[{"text":"weather?"}]},
		{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"q":"sf"}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"get_weather","response":{"output":"sunny"}}}]}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Upstream (chat) received a role="tool" message.
	msgs := gotBody["messages"].([]any)
	var toolMsg map[string]any
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "tool" {
			toolMsg = mm
		}
	}
	if toolMsg == nil || !strings.Contains(toolMsg["content"].(string), "sunny") {
		t.Errorf("tool message=%v", toolMsg)
	}
}

// ---- Scenario 21: Chat → Responses with tools (function_call output) ----

func TestConversion_ChatToResponses_Tools_NonStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"r","object":"response","status":"completed","model":"test-model","output":[{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"q\":\"sf\"}"}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`))
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "openai-response", `["openai-response"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/chat/completions", []byte(`{"model":"test-model","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish=%v want tool_calls", choice["finish_reason"])
	}
	tcs := choice["message"].(map[string]any)["tool_calls"].([]any)
	if tcs[0].(map[string]any)["function"].(map[string]any)["name"] != "get_weather" {
		t.Errorf("tool_call=%v", tcs[0])
	}
}

// ---- Scenario 22: Responses → Chat with function_call_output inbound ----

func TestConversion_ResponsesToChat_FunctionCallOutput_NonStream(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "openai", `["openai-chat"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/responses", []byte(`{"model":"test-model","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
		{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"q\":\"sf\"}"},
		{"type":"function_call_output","call_id":"c1","output":"sunny"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// Upstream (chat) received a role="tool" message with tool_call_id c1.
	msgs := gotBody["messages"].([]any)
	var toolMsg map[string]any
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "tool" {
			toolMsg = mm
		}
	}
	if toolMsg == nil || toolMsg["tool_call_id"] != "c1" || toolMsg["content"] != "sunny" {
		t.Errorf("tool message=%v", toolMsg)
	}
}

// ---- Scenario 23: Chat → Gemini URL image → 400 ----

func TestConversion_ChatToGemini_URLImage_Rejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for URL image to gemini")
	}))
	defer upstream.Close()

	env := newTestEnv(t, upstream.URL, "gemini", `["gemini"]`)
	defer env.cleanup()

	w := env.post(t, "/v1/chat/completions", []byte(`{"model":"test-model","messages":[{"role":"user","content":[
		{"type":"text","text":"look"},
		{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]}]}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400 (gemini URL image rejected)", w.Code, w.Body.String())
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ensure protocol package symbols referenced are wired (compile guard).
var _ = protocol.FormatAnthropic
