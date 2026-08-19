package channel

import (
	"encoding/json"
	"fmt"
	"strings"

	"any-load/internal/models"

	"github.com/gin-gonic/gin"
)

// RedirectModel applies a group's model redirect rules to a model name and
// returns the (possibly redirected) model. Used in protocol-conversion mode
// where the model is carried in the chat IR rather than a raw body. Returns an
// error only in strict mode when the model is not configured.
func RedirectModel(model string, group *models.Group) (string, error) {
	if len(group.ModelRedirectMap) == 0 || model == "" {
		return model, nil
	}
	if target, found := group.ModelRedirectMap[model]; found {
		return target, nil
	}
	if group.ModelRedirectStrict {
		return "", fmt.Errorf("model '%s' is not configured in redirect rules", model)
	}
	return model, nil
}

// IsOpenAIChatStream reports whether an inbound OpenAI Chat Completions
// request asks for streaming. Used in protocol-conversion mode where the
// inbound format is always OpenAI Chat (regardless of the group's channel
// type).
func IsOpenAIChatStream(c *gin.Context, bodyBytes []byte) bool {
	if strings.Contains(c.GetHeader("Accept"), "text/event-stream") {
		return true
	}
	if c.Query("stream") == "true" {
		return true
	}
	type streamPayload struct {
		Stream bool `json:"stream"`
	}
	var p streamPayload
	if err := json.Unmarshal(bodyBytes, &p); err == nil {
		return p.Stream
	}
	return false
}

// ExtractOpenAIChatModel returns the "model" field from an OpenAI Chat
// Completions request body.
func ExtractOpenAIChatModel(bodyBytes []byte) string {
	type modelPayload struct {
		Model string `json:"model"`
	}
	var p modelPayload
	if err := json.Unmarshal(bodyBytes, &p); err == nil {
		return p.Model
	}
	return ""
}

// ApplyOpenAIChatModelRedirect applies a group's model redirect rules to the
// "model" field of an OpenAI Chat Completions request body. It is a pure body
// operation and never modifies a URL path. Returns the original body unchanged
// when there are no redirect rules or no "model" field.
func ApplyOpenAIChatModelRedirect(bodyBytes []byte, group *models.Group) ([]byte, error) {
	if len(group.ModelRedirectMap) == 0 || len(bodyBytes) == 0 {
		return bodyBytes, nil
	}

	var requestData map[string]any
	if err := json.Unmarshal(bodyBytes, &requestData); err != nil {
		return bodyBytes, nil
	}

	modelValue, exists := requestData["model"]
	if !exists {
		return bodyBytes, nil
	}

	model, ok := modelValue.(string)
	if !ok {
		return bodyBytes, nil
	}

	if targetModel, found := group.ModelRedirectMap[model]; found {
		requestData["model"] = targetModel
		return json.Marshal(requestData)
	}

	if group.ModelRedirectStrict {
		return nil, fmt.Errorf("model '%s' is not configured in redirect rules", model)
	}

	return bodyBytes, nil
}
