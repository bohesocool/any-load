package proxy

import (
	"bytes"
	"compress/gzip"
	"any-load/internal/channel"
	app_errors "any-load/internal/errors"
	"any-load/internal/models"
	"any-load/internal/paramoverride"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func (ps *ProxyServer) applyParamOverrides(c *gin.Context, bodyBytes []byte, group *models.Group, channelHandler channel.ChannelProxy) ([]byte, error) {
	if len(group.ParamOverrides) == 0 || len(bodyBytes) == 0 {
		return bodyBytes, nil
	}

	ctx := paramoverride.Context{
		Model:       channelHandler.ExtractModel(c, bodyBytes),
		RequestPath: c.Request.URL.Path,
		IsStream:    channelHandler.IsStreamRequest(c, bodyBytes),
	}
	return paramoverride.Apply(bodyBytes, group.ParamOverrides, ctx)
}

// logUpstreamError provides a centralized way to log errors from upstream interactions.
func logUpstreamError(context string, err error) {
	if err == nil {
		return
	}
	if app_errors.IsIgnorableError(err) {
		logrus.Debugf("Ignorable upstream error in %s: %v", context, err)
	} else {
		logrus.Errorf("Upstream error in %s: %v", context, err)
	}
}

// handleGzipCompression checks for gzip encoding and decompresses the body if necessary.
func handleGzipCompression(resp *http.Response, bodyBytes []byte) []byte {
	if resp.Header.Get("Content-Encoding") == "gzip" {
		reader, gzipErr := gzip.NewReader(bytes.NewReader(bodyBytes))
		if gzipErr != nil {
			logrus.Warnf("Failed to create gzip reader for error body: %v", gzipErr)
			return bodyBytes
		}
		defer reader.Close()

		decompressedBody, readAllErr := io.ReadAll(reader)
		if readAllErr != nil {
			logrus.Warnf("Failed to decompress gzip error body: %v", readAllErr)
			return bodyBytes
		}
		return decompressedBody
	}
	return bodyBytes
}
