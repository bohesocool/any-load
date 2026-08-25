package protocol

import "testing"

func TestEmitInboundError(t *testing.T) {
	tests := []struct {
		name       string
		format     string
		statusCode int
		message    string
		check      func(t *testing.T, body map[string]any)
	}{
		{
			name:       "openai-chat default envelope",
			format:     FormatOpenAIChat,
			statusCode: 500,
			message:    "boom",
			check: func(t *testing.T, body map[string]any) {
				errObj, ok := body["error"].(map[string]any)
				if !ok {
					t.Fatalf("expected error object, got %v", body)
				}
				if errObj["message"] != "boom" {
					t.Errorf("message = %v, want boom", errObj["message"])
				}
			},
		},
		{
			name:       "anthropic envelope",
			format:     FormatAnthropic,
			statusCode: 500,
			message:    "boom",
			check: func(t *testing.T, body map[string]any) {
				if body["type"] != "error" {
					t.Errorf("type = %v, want error", body["type"])
				}
				errObj, ok := body["error"].(map[string]any)
				if !ok {
					t.Fatalf("expected error object, got %v", body)
				}
				if errObj["message"] != "boom" {
					t.Errorf("message = %v, want boom", errObj["message"])
				}
			},
		},
		{
			name:       "gemini envelope carries status code",
			format:     FormatGemini,
			statusCode: 503,
			message:    "boom",
			check: func(t *testing.T, body map[string]any) {
				errObj, ok := body["error"].(map[string]any)
				if !ok {
					t.Fatalf("expected error object, got %v", body)
				}
				if errObj["code"] != float64(503) {
					t.Errorf("code = %v, want 503", errObj["code"])
				}
				if errObj["message"] != "boom" {
					t.Errorf("message = %v, want boom", errObj["message"])
				}
			},
		},
		{
			name:       "unknown format falls back to openai-style",
			format:     "",
			statusCode: 429,
			message:    "boom",
			check: func(t *testing.T, body map[string]any) {
				if _, ok := body["error"].(map[string]any); !ok {
					t.Fatalf("expected error object, got %v", body)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := mustDecode(t, EmitInboundError(tt.format, tt.statusCode, tt.message))
			tt.check(t, body)
		})
	}
}
