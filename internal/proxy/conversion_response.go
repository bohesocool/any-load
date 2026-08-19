package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net/http"

	"any-load/internal/protocol"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// dispatchConversionResponse translates an upstream response (in the upstream
// handler's format) back to the client's inbound format (via the chat IR) and
// writes it to the client. Non-stream responses are translated as a whole;
// streaming responses are translated chunk-by-chunk as SSE.
func (ps *ProxyServer) dispatchConversionResponse(c *gin.Context, resp *http.Response, conv *convCtx, isStream bool) {
	if isStream {
		ps.handleStreamingConversionResponse(c, resp, conv)
		return
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		logrus.WithError(err).Error("Failed to read conversion response body")
		c.Status(http.StatusBadGateway)
		return
	}

	bodyBytes = handleGzipCompression(resp, bodyBytes)

	ir, err := conv.upHandler.ParseUpstreamResponse(bodyBytes)
	if err != nil {
		logrus.WithError(err).Error("Failed to parse upstream conversion response")
		c.Status(http.StatusBadGateway)
		return
	}

	out, err := conv.inHandler.EmitInboundResponse(ir)
	if err != nil {
		logrus.WithError(err).Error("Failed to emit inbound conversion response")
		c.Status(http.StatusBadGateway)
		return
	}

	c.Header("Content-Type", conv.inHandler.InboundContentType())
	c.Status(resp.StatusCode)
	if _, err := c.Writer.Write(out); err != nil {
		logrus.WithError(err).Error("Failed to write conversion response")
	}
}

// handleStreamingConversionResponse reads upstream SSE frames (in the upstream
// handler's format), converts them to IR stream events via the upstream parser,
// then to the client's inbound SSE format via the inbound emitter, and streams
// them to the client. The emitter's Done() frames terminate the stream.
func (ps *ProxyServer) handleStreamingConversionResponse(c *gin.Context, resp *http.Response, conv *convCtx) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logrus.Error("Streaming unsupported by the writer, falling back to normal response")
		ps.dispatchConversionResponse(c, resp, conv, false)
		return
	}

	parser := conv.upHandler.NewUpstreamStreamParser()
	emitter := conv.inHandler.NewInboundStreamEmitter("")
	br := bufio.NewReaderSize(resp.Body, 64*1024)

	writeChunks := func(chunks []protocol.StreamChunk) bool {
		for _, ch := range chunks {
			if ch.Event != "" {
				if _, werr := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", ch.Event, ch.Data); werr != nil {
					logrus.WithError(werr).Error("Failed to write converted SSE chunk")
					return false
				}
			} else {
				if _, werr := fmt.Fprintf(c.Writer, "data: %s\n\n", ch.Data); werr != nil {
					logrus.WithError(werr).Error("Failed to write converted SSE chunk")
					return false
				}
			}
		}
		flusher.Flush()
		return true
	}

	for {
		eventType, data, err := protocol.ReadSSEFrame(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			logrus.WithError(err).Error("Failed to read upstream SSE frame")
			break
		}

		events, perr := parser.Parse(eventType, data)
		for _, ev := range events {
			chunks, eerr := emitter.Emit(ev)
			if eerr != nil {
				logrus.WithError(eerr).Error("Inbound stream emitter error")
				return
			}
			if !writeChunks(chunks) {
				return
			}
		}
		if perr == io.EOF {
			break
		}
		if perr != nil {
			logrus.WithError(perr).Error("Upstream stream parser error")
			break
		}
	}

	// Emit the emitter's terminal frames (e.g. [DONE] for openai-chat, or
	// message_stop for anthropic) so the client sees a clean end-of-stream.
	if !writeChunks(emitter.Done()) {
		return
	}
}
