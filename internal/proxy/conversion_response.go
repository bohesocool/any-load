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
// writes it to the client. The upstream and client stream modes are decoupled:
//
//   - upstreamStream == clientStream: the response is forwarded in matching
//     form — streaming SSE chunk-by-chunk, or a whole translated JSON body.
//   - upstreamStream && !clientStream ("fake non-stream"): the upstream was
//     asked to stream; the SSE is accumulated into one ChatResponse IR and
//     emitted to the client as a single non-stream response body.
//   - !upstreamStream && clientStream ("fake stream"): the upstream was asked
//     for a single response; it is parsed into the IR and expanded into an SSE
//     stream for the client.
//
// model is the (redirected) IR model, threaded to the inbound stream emitter
// so the client's streamed chunks carry a correct model field. The raw
// upstream body (decompressed when applicable) is copied into capture for
// request logging when set; the converted client-facing body is copied into
// clientCapture.
func (ps *ProxyServer) dispatchConversionResponse(c *gin.Context, resp *http.Response, conv *convCtx, upstreamStream, clientStream bool, model string, capture *responseCapture, clientCapture *responseCapture) {
	switch {
	case upstreamStream && !clientStream:
		ps.handleFakeNonStreamConversionResponse(c, resp, conv, model, capture, clientCapture)
	case !upstreamStream && clientStream:
		ps.handleFakeStreamConversionResponse(c, resp, conv, model, capture, clientCapture)
	case clientStream:
		// upstreamStream == clientStream == true
		ps.handleStreamingConversionResponse(c, resp, conv, model, capture, clientCapture)
	default:
		// upstreamStream == clientStream == false
		ps.handleNonStreamConversionResponse(c, resp, conv, capture, clientCapture)
	}
}

// handleNonStreamConversionResponse translates a whole non-stream upstream
// response body into the client's inbound format and writes it as one JSON
// body. (Extracted from the old dispatchConversionResponse non-stream branch.)
func (ps *ProxyServer) handleNonStreamConversionResponse(c *gin.Context, resp *http.Response, conv *convCtx, capture *responseCapture, clientCapture *responseCapture) {
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		logrus.WithError(err).Error("Failed to read conversion response body")
		c.Status(http.StatusBadGateway)
		return
	}

	bodyBytes = handleGzipCompression(resp, bodyBytes)
	if capture != nil {
		capture.decoded = true
		capture.Write(bodyBytes)
	}

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
	if clientCapture != nil {
		clientCapture.Write(out)
	}
	if _, err := c.Writer.Write(out); err != nil {
		logrus.WithError(err).Error("Failed to write conversion response")
	}
}

// handleStreamingConversionResponse reads upstream SSE frames (in the upstream
// handler's format), converts them to IR stream events via the upstream parser,
// then to the client's inbound SSE format via the inbound emitter, and streams
// them to the client. The emitter's Done() frames terminate the stream. model
// is set on the inbound emitter so the client's chunks carry a correct model.
func (ps *ProxyServer) handleStreamingConversionResponse(c *gin.Context, resp *http.Response, conv *convCtx, model string, capture *responseCapture, clientCapture *responseCapture) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logrus.Error("Streaming unsupported by the writer, falling back to normal response")
		// Fall back to non-stream conversion of whatever the upstream sent.
		// (If the upstream was streaming this is imperfect — the body is SSE
		// bytes — but that is a pre-existing limitation of the fallback path.)
		ps.handleNonStreamConversionResponse(c, resp, conv, capture, clientCapture)
		return
	}

	parser := conv.upHandler.NewUpstreamStreamParser()
	emitter := conv.inHandler.NewInboundStreamEmitter(model)
	var upstreamBody io.Reader = resp.Body
	if capture != nil {
		// Capture the raw upstream SSE stream (before conversion) so the log
		// reflects exactly what the upstream returned.
		upstreamBody = io.TeeReader(resp.Body, capture)
	}
	br := bufio.NewReaderSize(upstreamBody, 64*1024)

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
			if !writeConvertedChunks(c, clientCapture, flusher, chunks) {
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
	writeConvertedChunks(c, clientCapture, flusher, emitter.Done())
}

// handleFakeNonStreamConversionResponse serves "fake non-stream": the upstream
// was asked to stream, and the proxy buffers the whole SSE into one
// ChatResponse IR, then emits it to the client as a single non-stream body in
// the client's inbound format.
func (ps *ProxyServer) handleFakeNonStreamConversionResponse(c *gin.Context, resp *http.Response, conv *convCtx, model string, capture *responseCapture, clientCapture *responseCapture) {
	var upstreamBody io.Reader = resp.Body
	if capture != nil {
		// Capture the raw upstream SSE (decoded stays false: the bytes are the
		// raw SSE text, no Content-Encoding decompression applies to a stream).
		upstreamBody = io.TeeReader(resp.Body, capture)
	}

	ir, err := protocol.AccumulateStreamToResponse(conv.upHandler, upstreamBody, model)
	if err != nil {
		logrus.WithError(err).Error("Failed to accumulate upstream stream into a response")
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
	if clientCapture != nil {
		clientCapture.Write(out)
	}
	if _, err := c.Writer.Write(out); err != nil {
		logrus.WithError(err).Error("Failed to write fake non-stream conversion response")
	}
}

// handleFakeStreamConversionResponse serves "fake stream": the upstream was
// asked for a single non-stream response, and the proxy parses it into the IR
// and expands it into an SSE stream in the client's inbound format.
func (ps *ProxyServer) handleFakeStreamConversionResponse(c *gin.Context, resp *http.Response, conv *convCtx, model string, capture *responseCapture, clientCapture *responseCapture) {
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		logrus.WithError(err).Error("Failed to read fake-stream upstream body")
		c.Status(http.StatusBadGateway)
		return
	}
	bodyBytes = handleGzipCompression(resp, bodyBytes)
	if capture != nil {
		capture.decoded = true
		capture.Write(bodyBytes)
	}

	ir, err := conv.upHandler.ParseUpstreamResponse(bodyBytes)
	if err != nil {
		logrus.WithError(err).Error("Failed to parse upstream conversion response for fake stream")
		c.Status(http.StatusBadGateway)
		return
	}

	// If the writer can't stream, fall back to emitting a single non-stream
	// body (we already hold the parsed IR, so this is clean and correct).
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		out, err := conv.inHandler.EmitInboundResponse(ir)
		if err != nil {
			logrus.WithError(err).Error("Failed to emit inbound conversion response")
			c.Status(http.StatusBadGateway)
			return
		}
		c.Header("Content-Type", conv.inHandler.InboundContentType())
		c.Status(resp.StatusCode)
		if clientCapture != nil {
			clientCapture.Write(out)
		}
		c.Writer.Write(out)
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(resp.StatusCode)

	emitter := conv.inHandler.NewInboundStreamEmitter(ir.Model)
	for _, ev := range protocol.ExpandResponseToEvents(ir) {
		chunks, eerr := emitter.Emit(ev)
		if eerr != nil {
			logrus.WithError(eerr).Error("Inbound stream emitter error (fake stream)")
			return
		}
		if !writeConvertedChunks(c, clientCapture, flusher, chunks) {
			return
		}
	}
	writeConvertedChunks(c, clientCapture, flusher, emitter.Done())
}

// writeConvertedChunks writes a batch of converted SSE chunks to the client
// (and the client capture, when set) and flushes. Returns false if writing
// failed so the caller can abort the stream. Shared by the stream→stream and
// non-stream→stream (fake stream) paths.
func writeConvertedChunks(c *gin.Context, clientCapture *responseCapture, flusher http.Flusher, chunks []protocol.StreamChunk) bool {
	for _, ch := range chunks {
		var frame string
		if ch.Event != "" {
			frame = fmt.Sprintf("event: %s\ndata: %s\n\n", ch.Event, ch.Data)
		} else {
			frame = fmt.Sprintf("data: %s\n\n", ch.Data)
		}
		if clientCapture != nil {
			clientCapture.Write([]byte(frame))
		}
		if _, werr := c.Writer.Write([]byte(frame)); werr != nil {
			logrus.WithError(werr).Error("Failed to write converted SSE chunk")
			return false
		}
	}
	flusher.Flush()
	return true
}
