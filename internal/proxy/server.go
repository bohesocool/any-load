// Package proxy provides high-performance OpenAI multi-key proxy server
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"any-load/internal/affinity"
	"any-load/internal/channel"
	"any-load/internal/config"
	"any-load/internal/encryption"
	app_errors "any-load/internal/errors"
	"any-load/internal/keypool"
	"any-load/internal/models"
	"any-load/internal/protocol"
	"any-load/internal/response"
	"any-load/internal/services"
	"any-load/internal/utils"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// ProxyServer represents the proxy server
type ProxyServer struct {
	keyProvider       *keypool.KeyProvider
	groupManager      *services.GroupManager
	subGroupManager   *services.SubGroupManager
	settingsManager   *config.SystemSettingsManager
	channelFactory    *channel.Factory
	requestLogService *services.RequestLogService
	encryptionSvc     encryption.Service
	affinityMgr       *affinity.Manager
}

// NewProxyServer creates a new proxy server
func NewProxyServer(
	keyProvider *keypool.KeyProvider,
	groupManager *services.GroupManager,
	subGroupManager *services.SubGroupManager,
	settingsManager *config.SystemSettingsManager,
	channelFactory *channel.Factory,
	requestLogService *services.RequestLogService,
	encryptionSvc encryption.Service,
	affinityMgr *affinity.Manager,
) (*ProxyServer, error) {
	return &ProxyServer{
		keyProvider:       keyProvider,
		groupManager:      groupManager,
		subGroupManager:   subGroupManager,
		settingsManager:   settingsManager,
		channelFactory:    channelFactory,
		requestLogService: requestLogService,
		encryptionSvc:     encryptionSvc,
		affinityMgr:       affinityMgr,
	}, nil
}

// affinityCtx carries channel-affinity state from HandleProxy into the retry loop.
type affinityCtx struct {
	enabled      bool          // affinity enabled and a key was derived
	affinityKey  string        // derived affinity key ("" when disabled)
	entryGroupID uint          // the entry (original) group the binding is keyed under
	ttl          time.Duration // binding TTL from effective config
	bound        *affinity.Binding // resolved binding to replay on attempt 0 (nil = cold)
}

// HandleProxy is the main entry point for proxy requests, refactored based on the stable .bak logic.
func (ps *ProxyServer) HandleProxy(c *gin.Context) {
	startTime := time.Now()
	groupName := c.Param("group_name")

	originalGroup, err := ps.groupManager.GetGroupByName(groupName)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	// Read the body up front: it is needed both to derive the channel-affinity
	// key (before sub-group selection) and downstream for param overrides.
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logrus.Errorf("Failed to read request body: %v", err)
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "Failed to read request body"))
		return
	}
	c.Request.Body.Close()

	// Channel affinity: derive the affinity key and try to replay an existing
	// binding so the same session stays on the same (upstream, key) pair.
	affCtx := ps.resolveAffinity(c, originalGroup, bodyBytes)

	group := originalGroup
	var channelHandler channel.ChannelProxy

	if affCtx.bound != nil {
		// Affinity hit: use the bound sub-group/channel directly. For standard
		// groups the binding stores an empty SubGroup (i.e. the entry group).
		boundName := affCtx.bound.SubGroup
		if boundName == "" {
			boundName = originalGroup.Name
		}
		boundGroup, err := ps.groupManager.GetGroupByName(boundName)
		if err != nil {
			// Bound sub-group no longer exists: invalidate and fall back to cold.
			ps.affinityMgr.DeleteBinding(affCtx.entryGroupID, affCtx.affinityKey)
			affCtx.bound = nil
		} else {
			group = boundGroup
		}
	}

	if affCtx.bound == nil {
		// Cold path: select sub-group (for aggregates) then resolve the group.
		subGroupName, err := ps.subGroupManager.SelectSubGroup(originalGroup)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"aggregate_group": originalGroup.Name,
				"error":           err,
			}).Error("Failed to select sub-group from aggregate")
			response.Error(c, app_errors.NewAPIError(app_errors.ErrNoKeysAvailable, "No available sub-groups"))
			return
		}
		group = originalGroup
		if subGroupName != "" {
			group, err = ps.groupManager.GetGroupByName(subGroupName)
			if err != nil {
				response.Error(c, app_errors.ParseDBError(err))
				return
			}
		}
	}

	channelHandler, err = ps.channelFactory.GetChannel(group)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to get channel for group '%s': %v", groupName, err)))
		return
	}

	// Validate a bound affinity hit against the live channel (upstream must
	// still exist at the bound index with the same base URL, and the bound key
	// must still be active). On mismatch, drop the binding and go cold.
	if affCtx.bound != nil && !ps.validateBoundBinding(affCtx, channelHandler) {
		ps.affinityMgr.DeleteBinding(affCtx.entryGroupID, affCtx.affinityKey)
		affCtx.bound = nil
	}

	finalBodyBytes, err := ps.applyParamOverrides(bodyBytes, group)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to apply parameter overrides: %v", err)))
		return
	}

	// Resolve protocol conversion mode. The inbound format is detected from
	// the request path; if it is already supported by the upstream (smart
	// passthrough) or conversion is disabled, the request takes the normal
	// passthrough path unchanged.
	conv, convErr := ps.resolveConversion(c, group)
	if convErr != nil {
		response.Error(c, convErr)
		return
	}

	var isStream bool
	if conv != nil {
		// Stream intent is format-specific: chat/anthropic use the body `stream`
		// field; gemini uses the path suffix :streamGenerateContent.
		isStream = conv.inHandler.IsInboundStream(c.Request.URL.Path, bodyBytes)
	} else {
		isStream = channelHandler.IsStreamRequest(c, bodyBytes)
	}

	ps.executeRequestWithRetry(c, channelHandler, originalGroup, group, finalBodyBytes, isStream, startTime, 0, affCtx, conv)
}

// convCtx carries the conversion handlers resolved for a request. A nil
// convCtx means passthrough (no conversion). Inbound handler parses the
// client's format; upstream handler emits/speaks the target format.
type convCtx struct {
	inHandler protocol.FormatHandler
	upHandler protocol.FormatHandler
}

// resolveConversion determines whether this request should run through the
// protocol-conversion path. Returns a non-nil convCtx when conversion applies,
// nil when passthrough (disabled, unknown inbound format, no target, or the
// inbound format is already an upstream-supported format → smart passthrough).
func (ps *ProxyServer) resolveConversion(c *gin.Context, group *models.Group) (*convCtx, *app_errors.APIError) {
	if !group.ProtocolConversion {
		return nil, nil
	}
	if c.Request.Method != http.MethodPost {
		return nil, nil
	}
	inboundFormat := protocol.DetectInboundFormat(c.Request.URL.Path)
	if inboundFormat == "" {
		return nil, nil
	}
	targetFormat := protocol.PickTarget(group.UpstreamFormatList, inboundFormat)
	if targetFormat == "" || targetFormat == inboundFormat {
		// No upstream formats configured, or the inbound format is already
		// supported upstream → passthrough.
		return nil, nil
	}
	inHandler, ok := protocol.GetHandler(inboundFormat)
	if !ok {
		// Inbound format detected but no handler (e.g. gemini in Phase A):
		// fall back to passthrough rather than failing.
		return nil, nil
	}
	upHandler, ok := protocol.GetHandler(targetFormat)
	if !ok {
		return nil, app_errors.NewAPIError(app_errors.ErrInternalServer,
			fmt.Sprintf("protocol conversion target %q is not supported", targetFormat))
	}
	return &convCtx{inHandler: inHandler, upHandler: upHandler}, nil
}

// resolveAffinity derives the affinity key for the request and, if affinity is
// enabled, attempts to load an existing binding. Returns a ctx with bound=nil
// when affinity is disabled, no key could be derived, or no binding exists.
func (ps *ProxyServer) resolveAffinity(c *gin.Context, originalGroup *models.Group, bodyBytes []byte) *affinityCtx {
	ctx := &affinityCtx{entryGroupID: originalGroup.ID}

	if !originalGroup.EffectiveConfig.ChannelAffinity {
		return ctx
	}

	affinityKey := affinity.DeriveAffinityKey(c.Request.Header, bodyBytes)
	if affinityKey == "" {
		return ctx
	}

	ctx.enabled = true
	ctx.affinityKey = affinityKey
	if originalGroup.EffectiveConfig.ChannelAffinityTTL > 0 {
		ctx.ttl = time.Duration(originalGroup.EffectiveConfig.ChannelAffinityTTL) * time.Second
	} else {
		ctx.ttl = 0 // no expiry
	}

	bound, err := ps.affinityMgr.GetBinding(originalGroup.ID, affinityKey)
	if err != nil {
		logrus.WithError(err).Warn("Failed to load affinity binding")
		return ctx
	}
	ctx.bound = bound
	return ctx
}

// validateBoundBinding checks that the bound (upstream, key) is still usable
// against the live channel: the upstream index is in range and its base URL is
// unchanged, and the bound key is still active.
func (ps *ProxyServer) validateBoundBinding(ctx *affinityCtx, ch channel.ChannelProxy) bool {
	if ctx.bound == nil {
		return false
	}
	if base := ch.UpstreamBaseURL(ctx.bound.UpstreamIdx); base == "" || base != ctx.bound.BaseURL {
		return false
	}
	key, err := ps.keyProvider.GetKeyByID(ctx.bound.KeyID)
	if err != nil || key == nil {
		return false
	}
	return true
}

// executeRequestWithRetry is the core recursive function for handling requests and retries.
func (ps *ProxyServer) executeRequestWithRetry(
	c *gin.Context,
	channelHandler channel.ChannelProxy,
	originalGroup *models.Group,
	group *models.Group,
	bodyBytes []byte,
	isStream bool,
	startTime time.Time,
	retryCount int,
	affCtx *affinityCtx,
	conv *convCtx,
) {
	cfg := group.EffectiveConfig
	maxConc := cfg.MaxConcurrencyPerKey

	// Build the source URL (for upstream selection) and the request body.
	// In conversion mode the inbound body is parsed to the chat IR, the model
	// is redirected, and the IR is emitted in the target upstream format with
	// the target's native path. In passthrough mode both are the inbound
	// values unchanged.
	var reqBody []byte
	srcURL := c.Request.URL
	if conv != nil {
		ir, err := conv.inHandler.ParseInboundRequest(c.Request.URL.Path, bodyBytes)
		if err != nil {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, err.Error()))
			ps.logRequest(c, originalGroup, group, nil, startTime, http.StatusBadRequest, err, isStream, "", channelHandler, bodyBytes, models.RequestTypeFinal)
			return
		}
		// Apply model redirect on the IR model.
		if redirected, rerr := channel.RedirectModel(ir.Model, group); rerr != nil {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, rerr.Error()))
			ps.logRequest(c, originalGroup, group, nil, startTime, http.StatusBadRequest, rerr, isStream, "", channelHandler, bodyBytes, models.RequestTypeFinal)
			return
		} else {
			ir.Model = redirected
		}
		ir.Stream = isStream

		path, rawQuery := conv.upHandler.UpstreamPath(ir.Model, isStream)
		srcURL = &url.URL{Path: path, RawQuery: rawQuery}
		reqBody, err = conv.upHandler.EmitUpstreamRequest(ir)
		if err != nil {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, err.Error()))
			ps.logRequest(c, originalGroup, group, nil, startTime, http.StatusBadRequest, err, isStream, "", channelHandler, bodyBytes, models.RequestTypeFinal)
			return
		}
	} else {
		reqBody = bodyBytes
	}

	apiKey, upstreamURL, err := ps.selectUpstreamAndKey(c, channelHandler, srcURL, originalGroup, group, retryCount, affCtx)
	if err != nil {
		logrus.Errorf("Failed to select a key for group %s on attempt %d: %v", group.Name, retryCount+1, err)
		response.Error(c, app_errors.NewAPIError(app_errors.ErrNoKeysAvailable, err.Error()))
		ps.logRequest(c, originalGroup, group, nil, startTime, http.StatusServiceUnavailable, err, isStream, "", channelHandler, bodyBytes, models.RequestTypeFinal)
		return
	}
	// Release the per-key concurrency slot when this attempt fully completes
	// (success, error, or after recursing into the next retry attempt).
	defer ps.keyProvider.ReleaseKey(group.ID, apiKey.ID, maxConc)

	var ctx context.Context
	var cancel context.CancelFunc
	if isStream {
		ctx, cancel = context.WithCancel(c.Request.Context())
	} else {
		timeout := time.Duration(cfg.RequestTimeout) * time.Second
		ctx, cancel = context.WithTimeout(c.Request.Context(), timeout)
	}
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, c.Request.Method, upstreamURL, bytes.NewReader(reqBody))
	if err != nil {
		logrus.Errorf("Failed to create upstream request: %v", err)
		response.Error(c, app_errors.ErrInternalServer)
		return
	}
	req.ContentLength = int64(len(reqBody))

	req.Header = c.Request.Header.Clone()

	// Clean up client auth key
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	req.Header.Del("X-Goog-Api-Key")

	if conv != nil {
		// Target-native auth (e.g. x-api-key for Anthropic, ?key= for Gemini).
		conv.upHandler.ApplyAuth(req, apiKey)
	} else {
		// Apply model redirection
		finalBodyBytes, err := channelHandler.ApplyModelRedirect(req, bodyBytes, group)
		if err != nil {
			response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, err.Error()))
			ps.logRequest(c, originalGroup, group, apiKey, startTime, http.StatusBadRequest, err, isStream, upstreamURL, channelHandler, bodyBytes, models.RequestTypeFinal)
			return
		}

		// Update request body if it was modified by redirection
		if !bytes.Equal(finalBodyBytes, bodyBytes) {
			req.Body = io.NopCloser(bytes.NewReader(finalBodyBytes))
			req.ContentLength = int64(len(finalBodyBytes))
		}

		channelHandler.ModifyRequest(req, apiKey, group)
	}

	// Apply custom header rules
	if len(group.HeaderRuleList) > 0 {
		headerCtx := utils.NewHeaderVariableContextFromGin(c, group, apiKey)
		utils.ApplyHeaderRules(req, group.HeaderRuleList, headerCtx)
	}

	var client *http.Client
	if isStream {
		client = channelHandler.GetStreamClient()
		req.Header.Set("X-Accel-Buffering", "no")
	} else {
		client = channelHandler.GetHTTPClient()
	}

	resp, err := client.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}

	// Unified error handling for retries.
	// Retry policy is fully defined by group.FailoverStatusCodeMatcher (derived from EffectiveConfig).
	shouldRetryByStatus := resp != nil && shouldFailoverOnStatusCode(resp.StatusCode, group)
	if err != nil || shouldRetryByStatus {
		if err != nil && app_errors.IsIgnorableError(err) {
			logrus.Debugf("Client-side ignorable error for key %s, aborting retries: %v", utils.MaskAPIKey(apiKey.KeyValue), err)
			ps.logRequest(c, originalGroup, group, apiKey, startTime, 499, err, isStream, upstreamURL, channelHandler, bodyBytes, models.RequestTypeFinal)
			return
		}

		var statusCode int
		var errorMessage string
		var parsedError string

		if err != nil {
			statusCode = 500
			errorMessage = err.Error()
			parsedError = errorMessage
			logrus.Debugf("Request failed (attempt %d/%d) for key %s: %v", retryCount+1, cfg.MaxRetries, utils.MaskAPIKey(apiKey.KeyValue), err)
		} else {
			// Retryable upstream response (HTTP status code matched failover policy)
			statusCode = resp.StatusCode
			errorBody, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				logrus.Errorf("Failed to read error body: %v", readErr)
				errorBody = []byte("Failed to read error body")
			}

			errorBody = handleGzipCompression(resp, errorBody)
			errorMessage = string(errorBody)
			parsedError = app_errors.ParseUpstreamError(errorBody)
			logrus.Debugf("Request failed with status %d (attempt %d/%d) for key %s. Parsed Error: %s", statusCode, retryCount+1, cfg.MaxRetries, utils.MaskAPIKey(apiKey.KeyValue), parsedError)
		}

		// 上游 key 可能出现在错误文本中（如 Gemini 通道将 key 放入 URL query，
		// 传输层错误会把完整 URL 带入 err.Error()），返回客户端和落库前先脱敏
		errorMessage = utils.RedactSecret(errorMessage, apiKey.KeyValue)
		parsedError = utils.RedactSecret(parsedError, apiKey.KeyValue)

		// 使用解析后的错误信息更新密钥状态
		ps.keyProvider.UpdateStatus(apiKey, group, false, parsedError)

		// 判断是否为最后一次尝试
		isLastAttempt := retryCount >= cfg.MaxRetries
		requestType := models.RequestTypeRetry
		if isLastAttempt {
			requestType = models.RequestTypeFinal
		}

		ps.logRequest(c, originalGroup, group, apiKey, startTime, statusCode, errors.New(parsedError), isStream, upstreamURL, channelHandler, bodyBytes, requestType)

		// 如果是最后一次尝试，直接返回错误，不再递归
		if isLastAttempt {
			var errorJSON map[string]any
			if err := json.Unmarshal([]byte(errorMessage), &errorJSON); err == nil {
				c.JSON(statusCode, errorJSON)
			} else {
				response.Error(c, app_errors.NewAPIErrorWithUpstream(statusCode, "UPSTREAM_ERROR", errorMessage))
			}
			return
		}

		ps.executeRequestWithRetry(c, channelHandler, originalGroup, group, bodyBytes, isStream, startTime, retryCount+1, affCtx, conv)
		return
	}

	// ps.keyProvider.UpdateStatus(apiKey, group, true) // 请求成功不再重置成功次数，减少IO消耗
	logrus.Debugf("Request for group %s succeeded on attempt %d with key %s", group.Name, retryCount+1, utils.MaskAPIKey(apiKey.KeyValue))

	// In conversion mode, translate the upstream response back to the client's
	// inbound format. Otherwise (passthrough, or smart-passthrough target)
	// forward the response as-is.
	if conv != nil {
		ps.dispatchConversionResponse(c, resp, conv, isStream)
	} else if shouldInterceptModelList(c.Request.URL.Path, c.Request.Method) {
		// Check if this is a model list request (needs special handling)
		ps.handleModelListResponse(c, resp, group, channelHandler)
	} else {
		for key, values := range resp.Header {
			for _, value := range values {
				c.Header(key, value)
			}
		}
		c.Status(resp.StatusCode)

		if isStream {
			ps.handleStreamingResponse(c, resp)
		} else {
			ps.handleNormalResponse(c, resp)
		}
	}

	ps.logRequest(c, originalGroup, group, apiKey, startTime, resp.StatusCode, nil, isStream, upstreamURL, channelHandler, bodyBytes, models.RequestTypeFinal)
}

func shouldFailoverOnStatusCode(statusCode int, group *models.Group) bool {
	if group == nil {
		return false
	}
	return group.FailoverStatusCodeMatcher.Match(statusCode)
}

// selectUpstreamAndKey picks an (upstream, key) pair for this attempt honoring
// channel affinity (replay a binding on attempt 0) and per-key concurrency. It
// acquires exactly one concurrency slot on success; the caller must ReleaseKey
// it. Affinity bindings are (re)written here: renewed on a bound replay,
// established on a first-attempt cold selection, and left untouched on retries
// or when a still-active bound key is transiently at capacity.
func (ps *ProxyServer) selectUpstreamAndKey(
	c *gin.Context,
	ch channel.ChannelProxy,
	srcURL *url.URL,
	originalGroup *models.Group,
	group *models.Group,
	retryCount int,
	affCtx *affinityCtx,
) (*models.APIKey, string, error) {
	maxConc := group.EffectiveConfig.MaxConcurrencyPerKey

	// Affinity hit on the first attempt: prefer the bound key.
	if retryCount == 0 && affCtx.bound != nil {
		apiKey, err := ps.keyProvider.SelectKeyWithConcurrency(group.ID, maxConc, affCtx.bound.KeyID)
		if err != nil {
			return nil, "", err
		}
		if apiKey.ID == affCtx.bound.KeyID {
			// Got the bound key: replay its upstream by index.
			upstreamURL, buildErr := ch.BuildUpstreamURLAt(srcURL, originalGroup.Name, affCtx.bound.UpstreamIdx)
			if buildErr == nil {
				ps.storeAffinityBinding(affCtx, ch, originalGroup, group, apiKey, affCtx.bound.UpstreamIdx)
				return apiKey, upstreamURL, nil
			}
			// Bound upstream is stale (group upstreams changed): release and
			// fall through to a fresh weighted selection below.
			ps.keyProvider.ReleaseKey(group.ID, apiKey.ID, maxConc)
		} else {
			// Bound key is at concurrency capacity (it was confirmed active in
			// HandleProxy): keep the existing binding so the next request retries
			// it, and just use the rotated key for this request.
			upstreamURL, _, buildErr := ch.BuildUpstreamURL(srcURL, originalGroup.Name)
			if buildErr != nil {
				ps.keyProvider.ReleaseKey(group.ID, apiKey.ID, maxConc)
				return nil, "", buildErr
			}
			return apiKey, upstreamURL, nil
		}
	}

	// Cold / retry selection.
	apiKey, err := ps.keyProvider.SelectKeyWithConcurrency(group.ID, maxConc, 0)
	if err != nil {
		return nil, "", err
	}
	upstreamURL, upstreamIdx, err := ch.BuildUpstreamURL(srcURL, originalGroup.Name)
	if err != nil {
		ps.keyProvider.ReleaseKey(group.ID, apiKey.ID, maxConc)
		return nil, "", err
	}

	// Establish a binding only on the first attempt's cold selection so retry
	// churn doesn't thrash the binding; a bound key that later gets blacklisted
	// is dropped by validateBoundBinding on the next request.
	if retryCount == 0 && affCtx.enabled {
		ps.storeAffinityBinding(affCtx, ch, originalGroup, group, apiKey, upstreamIdx)
	}
	return apiKey, upstreamURL, nil
}

// storeAffinityBinding writes (or renews) the affinity binding for the selected
// (key, upstream) pair. SubGroup is empty for standard groups (the binding is
// keyed under the entry group itself) and set to the child name for aggregates.
func (ps *ProxyServer) storeAffinityBinding(
	affCtx *affinityCtx,
	ch channel.ChannelProxy,
	originalGroup *models.Group,
	group *models.Group,
	apiKey *models.APIKey,
	upstreamIdx int,
) {
	subGroup := ""
	if originalGroup.GroupType == "aggregate" && group.ID != originalGroup.ID {
		subGroup = group.Name
	}
	b := &affinity.Binding{
		GroupID:     affCtx.entryGroupID,
		KeyID:       apiKey.ID,
		UpstreamIdx: upstreamIdx,
		BaseURL:     ch.UpstreamBaseURL(upstreamIdx),
		SubGroup:    subGroup,
	}
	if err := ps.affinityMgr.SetBinding(affCtx.entryGroupID, affCtx.affinityKey, b, affCtx.ttl); err != nil {
		logrus.WithError(err).Warn("Failed to store affinity binding")
	}
}

// logRequest is a helper function to create and record a request log.
func (ps *ProxyServer) logRequest(
	c *gin.Context,
	originalGroup *models.Group,
	group *models.Group,
	apiKey *models.APIKey,
	startTime time.Time,
	statusCode int,
	finalError error,
	isStream bool,
	upstreamAddr string,
	channelHandler channel.ChannelProxy,
	bodyBytes []byte,
	requestType string,
) {
	if ps.requestLogService == nil {
		return
	}

	var requestBodyToLog, userAgent string

	if group.EffectiveConfig.EnableRequestBodyLogging {
		requestBodyToLog = utils.TruncateString(string(bodyBytes), 65000)
		userAgent = c.Request.UserAgent()
	}

	duration := time.Since(startTime).Milliseconds()

	logEntry := &models.RequestLog{
		GroupID:      group.ID,
		GroupName:    group.Name,
		IsSuccess:    finalError == nil && statusCode < 400,
		SourceIP:     c.ClientIP(),
		StatusCode:   statusCode,
		RequestPath:  utils.TruncateString(c.Request.URL.String(), 500),
		Duration:     duration,
		UserAgent:    userAgent,
		RequestType:  requestType,
		IsStream:     isStream,
		UpstreamAddr: utils.TruncateString(upstreamAddr, 500),
		RequestBody:  requestBodyToLog,
	}

	// Set parent group
	if originalGroup != nil && originalGroup.GroupType == "aggregate" && originalGroup.ID != group.ID {
		logEntry.ParentGroupID = originalGroup.ID
		logEntry.ParentGroupName = originalGroup.Name
	}

	if channelHandler != nil && bodyBytes != nil {
		logEntry.Model = channelHandler.ExtractModel(c, bodyBytes)
	}

	if apiKey != nil {
		// 加密密钥值用于日志存储
		encryptedKeyValue, err := ps.encryptionSvc.Encrypt(apiKey.KeyValue)
		if err != nil {
			logrus.WithError(err).Error("Failed to encrypt key value for logging")
			logEntry.KeyValue = "failed-to-encryption"
		} else {
			logEntry.KeyValue = encryptedKeyValue
		}
		// 添加 KeyHash 用于反查
		logEntry.KeyHash = ps.encryptionSvc.Hash(apiKey.KeyValue)
	}

	if finalError != nil {
		logEntry.ErrorMessage = finalError.Error()
	}

	if err := ps.requestLogService.Record(logEntry); err != nil {
		logrus.Errorf("Failed to record request log: %v", err)
	}
}
