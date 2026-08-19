package channel

import (
	"context"
	"any-load/internal/models"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
)

// ChannelProxy defines the interface for different API channel proxies.
type ChannelProxy interface {
	// BuildUpstreamURL constructs the target URL for the upstream service via
	// weighted round-robin, returning the chosen upstream index too (used by
	// channel affinity to record the binding).
	BuildUpstreamURL(originalURL *url.URL, groupName string) (string, int, error)

	// BuildUpstreamURLAt constructs the target URL using the upstream at idx,
	// replaying an affinity binding. Returns an error if idx is out of range.
	BuildUpstreamURLAt(originalURL *url.URL, groupName string, idx int) (string, error)

	// UpstreamBaseURL returns the base URL string of the upstream at idx, or
	// "" if idx is out of range. Used to validate that a binding's upstream
	// has not changed.
	UpstreamBaseURL(idx int) string

	// IsConfigStale checks if the channel's configuration is stale compared to the provided group.
	IsConfigStale(group *models.Group) bool

	// GetHTTPClient returns the client for standard requests.
	GetHTTPClient() *http.Client

	// GetStreamClient returns the client for streaming requests.
	GetStreamClient() *http.Client

	// ModifyRequest allows the channel to add specific headers or modify the request
	ModifyRequest(req *http.Request, apiKey *models.APIKey, group *models.Group)

	// IsStreamRequest checks if the request is for a streaming response,
	IsStreamRequest(c *gin.Context, bodyBytes []byte) bool

	// ExtractModel extracts the model name from the request.
	ExtractModel(c *gin.Context, bodyBytes []byte) string

	// ValidateKey checks if the given API key is valid.
	ValidateKey(ctx context.Context, apiKey *models.APIKey, group *models.Group) (bool, error)

	// ApplyModelRedirect applies model redirection based on the group's redirect rules.
	ApplyModelRedirect(req *http.Request, bodyBytes []byte, group *models.Group) ([]byte, error)

	// TransformModelList transforms the model list response based on redirect rules.
	TransformModelList(req *http.Request, bodyBytes []byte, group *models.Group) (map[string]any, error)
}
