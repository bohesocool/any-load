package channel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"any-load/internal/models"
	"any-load/internal/types"
	"any-load/internal/utils"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
	"gorm.io/datatypes"
)

// UpstreamInfo holds the information for a single upstream server, including its weight.
type UpstreamInfo struct {
	URL           *url.URL
	Weight        int
	CurrentWeight int
}

// BaseChannel provides common functionality for channel proxies.
type BaseChannel struct {
	Name               string
	Upstreams          []UpstreamInfo
	HTTPClient         *http.Client
	StreamClient       *http.Client
	TestModel          string
	ValidationEndpoint string
	upstreamLock       sync.Mutex

	// Cached fields from the group for stale check
	channelType         string
	groupUpstreams      datatypes.JSON
	effectiveConfig     *types.SystemSettings
	modelRedirectRules  datatypes.JSONMap
	modelRedirectStrict bool
}

// getUpstreamURL selects an upstream URL using a smooth weighted round-robin
// algorithm, returning the chosen upstream's index alongside its URL so the
// caller (channel affinity) can replay the same upstream later.
func (b *BaseChannel) getUpstreamURL() (*url.URL, int) {
	b.upstreamLock.Lock()
	defer b.upstreamLock.Unlock()

	if len(b.Upstreams) == 0 {
		return nil, 0
	}
	if len(b.Upstreams) == 1 {
		return b.Upstreams[0].URL, 0
	}

	totalWeight := 0
	var best *UpstreamInfo
	bestIdx := 0

	for i := range b.Upstreams {
		up := &b.Upstreams[i]
		totalWeight += up.Weight
		up.CurrentWeight += up.Weight

		if best == nil || up.CurrentWeight > best.CurrentWeight {
			best = up
			bestIdx = i
		}
	}

	if best == nil {
		return b.Upstreams[0].URL, 0 // 降级到第一个可用的
	}

	best.CurrentWeight -= totalWeight
	return best.URL, bestIdx
}

// buildURL joins an upstream base URL with the inbound request path/query.
func (b *BaseChannel) buildURL(base *url.URL, originalURL *url.URL, groupName string) string {
	finalURL := *base
	proxyPrefix := "/proxy/" + groupName
	requestPath := originalURL.Path
	requestPath = strings.TrimPrefix(requestPath, proxyPrefix)

	finalURL.Path = strings.TrimRight(finalURL.Path, "/") + requestPath

	finalURL.RawQuery = originalURL.RawQuery

	return finalURL.String()
}

// BuildUpstreamURL constructs the target URL for the upstream service using
// weighted round-robin, and returns the chosen upstream index so affinity
// bindings can record it.
func (b *BaseChannel) BuildUpstreamURL(originalURL *url.URL, groupName string) (string, int, error) {
	base, idx := b.getUpstreamURL()
	if base == nil {
		return "", 0, fmt.Errorf("no upstream URL configured for channel %s", b.Name)
	}
	return b.buildURL(base, originalURL, groupName), idx, nil
}

// BuildUpstreamURLAt constructs the target URL using the upstream at the given
// index. It is used to replay the upstream chosen by a channel-affinity
// binding. Returns an error if idx is out of range (the group's upstream list
// changed since the binding was created); callers should then fall back to a
// fresh weighted selection.
func (b *BaseChannel) BuildUpstreamURLAt(originalURL *url.URL, groupName string, idx int) (string, error) {
	b.upstreamLock.Lock()
	defer b.upstreamLock.Unlock()

	if idx < 0 || idx >= len(b.Upstreams) {
		return "", fmt.Errorf("upstream index %d out of range for channel %s", idx, b.Name)
	}
	return b.buildURL(b.Upstreams[idx].URL, originalURL, groupName), nil
}

// UpstreamBaseURL returns the base URL string of the upstream at idx, for
// validating that an affinity binding's upstream is still the same. The caller
// holds no lock; this acquires upstreamLock for a consistent read.
func (b *BaseChannel) UpstreamBaseURL(idx int) string {
	b.upstreamLock.Lock()
	defer b.upstreamLock.Unlock()
	if idx < 0 || idx >= len(b.Upstreams) {
		return ""
	}
	return b.Upstreams[idx].URL.String()
}

// UpstreamCount returns the number of configured upstreams.
func (b *BaseChannel) UpstreamCount() int {
	b.upstreamLock.Lock()
	defer b.upstreamLock.Unlock()
	return len(b.Upstreams)
}

// IsConfigStale checks if the channel's configuration is stale compared to the provided group.
func (b *BaseChannel) IsConfigStale(group *models.Group) bool {
	if b.channelType != group.ChannelType {
		return true
	}
	if b.TestModel != group.TestModel {
		return true
	}
	if b.ValidationEndpoint != utils.GetValidationEndpoint(group) {
		return true
	}
	if !bytes.Equal(b.groupUpstreams, group.Upstreams) {
		return true
	}
	if !reflect.DeepEqual(b.effectiveConfig, &group.EffectiveConfig) {
		return true
	}
	// Check for model redirect rules changes
	if !reflect.DeepEqual(b.modelRedirectRules, group.ModelRedirectRules) {
		return true
	}
	if b.modelRedirectStrict != group.ModelRedirectStrict {
		return true
	}
	return false
}

// GetHTTPClient returns the client for standard requests.
func (b *BaseChannel) GetHTTPClient() *http.Client {
	return b.HTTPClient
}

// GetStreamClient returns the client for streaming requests.
func (b *BaseChannel) GetStreamClient() *http.Client {
	return b.StreamClient
}

// ApplyModelRedirect applies model redirection based on the group's redirect rules.
func (b *BaseChannel) ApplyModelRedirect(req *http.Request, bodyBytes []byte, group *models.Group) ([]byte, error) {
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

	// Direct match without any prefix processing
	if targetModel, found := group.ModelRedirectMap[model]; found {
		requestData["model"] = targetModel

		// Log the redirection for audit
		logrus.WithFields(logrus.Fields{
			"group":          group.Name,
			"original_model": model,
			"target_model":   targetModel,
			"channel":        "json_body",
		}).Debug("Model redirected")

		return json.Marshal(requestData)
	}

	if group.ModelRedirectStrict {
		return nil, fmt.Errorf("model '%s' is not configured in redirect rules", model)
	}

	return bodyBytes, nil
}

// TransformModelList transforms the model list response based on redirect rules.
func (b *BaseChannel) TransformModelList(req *http.Request, bodyBytes []byte, group *models.Group) (map[string]any, error) {
	var response map[string]any
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		logrus.WithError(err).Debug("Failed to parse model list response, returning empty")
		return nil, err
	}

	dataInterface, exists := response["data"]
	if !exists {
		return response, nil
	}

	upstreamModels, ok := dataInterface.([]any)
	if !ok {
		return response, nil
	}

	// Build configured source models list (common logic for both modes)
	configuredModels := buildConfiguredModels(group.ModelRedirectMap)

	// Strict mode: return only configured models (whitelist)
	if group.ModelRedirectStrict {
		response["data"] = configuredModels

		logrus.WithFields(logrus.Fields{
			"group":       group.Name,
			"model_count": len(configuredModels),
			"strict_mode": true,
		}).Debug("Model list returned (strict mode - configured models only)")

		return response, nil
	}

	// Non-strict mode: merge upstream + configured models (upstream priority)
	merged := mergeModelLists(upstreamModels, configuredModels)
	response["data"] = merged

	logrus.WithFields(logrus.Fields{
		"group":            group.Name,
		"upstream_count":   len(upstreamModels),
		"configured_count": len(configuredModels),
		"merged_count":     len(merged),
		"strict_mode":      false,
	}).Debug("Model list merged (non-strict mode)")

	return response, nil
}

// buildConfiguredModels builds a list of models from redirect rules
func buildConfiguredModels(redirectMap map[string]string) []any {
	if len(redirectMap) == 0 {
		return []any{}
	}

	models := make([]any, 0, len(redirectMap))
	for sourceModel := range redirectMap {
		models = append(models, map[string]any{
			"id":       sourceModel,
			"object":   "model",
			"created":  0,
			"owned_by": "system",
		})
	}
	return models
}

// mergeModelLists merges upstream and configured model lists
func mergeModelLists(upstream []any, configured []any) []any {
	// Create set of upstream model IDs
	upstreamIDs := make(map[string]bool)
	for _, item := range upstream {
		if modelObj, ok := item.(map[string]any); ok {
			if modelID, ok := modelObj["id"].(string); ok {
				upstreamIDs[modelID] = true
			}
		}
	}

	// Start with all upstream models
	result := make([]any, len(upstream))
	copy(result, upstream)

	// Add configured models that don't exist in upstream
	for _, item := range configured {
		if modelObj, ok := item.(map[string]any); ok {
			if modelID, ok := modelObj["id"].(string); ok {
				if !upstreamIDs[modelID] {
					result = append(result, item)
				}
			}
		}
	}

	return result
}
