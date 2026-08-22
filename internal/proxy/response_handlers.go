package proxy

import (
	"bytes"
	"io"
	"net/http"

	"any-load/internal/utils"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// maxLoggedResponseBodySize caps how much of the upstream response is kept in
// memory for logging. It is far above any normal LLM response size and only
// guards against pathological cases (e.g. proxying huge binary downloads).
const maxLoggedResponseBodySize = 10 << 20 // 10MB

// responseCapture accumulates a copy of the upstream response while it is
// being forwarded to the client, so it can be stored in the request log.
// A nil *responseCapture disables capturing entirely.
type responseCapture struct {
	buf       bytes.Buffer
	truncated bool
	// decoded is set when the captured bytes are already decompressed
	// (conversion / model-list paths read and decode the body themselves).
	decoded bool
}

// Write implements io.Writer so the capture can be used with io.TeeReader.
// It always reports the full length as written to keep the forwarding path
// intact even after the capture limit is reached.
func (rc *responseCapture) Write(p []byte) (int, error) {
	remain := maxLoggedResponseBodySize - rc.buf.Len()
	if remain > 0 {
		if len(p) > remain {
			rc.buf.Write(p[:remain])
			rc.truncated = true
		} else {
			rc.buf.Write(p)
		}
	} else if len(p) > 0 {
		rc.truncated = true
	}
	return len(p), nil
}

// bodyForLog returns the captured response as a string for logging,
// decompressing it based on the upstream Content-Encoding when needed.
func (rc *responseCapture) bodyForLog(resp *http.Response) string {
	if rc == nil || rc.buf.Len() == 0 {
		return ""
	}
	data := rc.buf.Bytes()
	if !rc.decoded && !rc.truncated && resp != nil {
		decompressed, err := utils.DecompressResponse(resp.Header.Get("Content-Encoding"), data)
		if err == nil {
			data = decompressed
		}
	}
	body := string(data)
	if rc.truncated {
		body += "\n...[response body truncated]"
	}
	return body
}

func (ps *ProxyServer) handleStreamingResponse(c *gin.Context, resp *http.Response, capture *responseCapture) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logrus.Error("Streaming unsupported by the writer, falling back to normal response")
		ps.handleNormalResponse(c, resp, capture)
		return
	}

	buf := make([]byte, 4*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if capture != nil {
				capture.Write(buf[:n])
			}
			if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
				logUpstreamError("writing stream to client", writeErr)
				return
			}
			flusher.Flush()
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			logUpstreamError("reading from upstream", err)
			return
		}
	}
}

func (ps *ProxyServer) handleNormalResponse(c *gin.Context, resp *http.Response, capture *responseCapture) {
	var reader io.Reader = resp.Body
	if capture != nil {
		reader = io.TeeReader(resp.Body, capture)
	}
	if _, err := io.Copy(c.Writer, reader); err != nil {
		logUpstreamError("copying response body", err)
	}
}
