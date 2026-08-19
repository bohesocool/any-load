package protocol

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// ReadSSEFrame reads one SSE frame from br and returns the event type (from an
// `event:` line, or "" if absent) and the concatenated `data:` payload (without
// the `data:` prefixes; multiple `data:` lines are joined with "\n"). Returns
// io.EOF when the stream has ended. Leading blank lines and non-data/event
// lines (id:, retry:, comments) are skipped.
func ReadSSEFrame(br *bufio.Reader) (eventType string, data []byte, err error) {
	var dataBuf bytes.Buffer
	hasData := false

	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) == 0 {
			if rerr == io.EOF {
				if hasData {
					return eventType, dataBuf.Bytes(), nil
				}
				return "", nil, io.EOF
			}
			if rerr != nil {
				return "", nil, rerr
			}
			continue
		}

		trimmed := strings.TrimRight(string(line), "\r\n")
		if trimmed == "" {
			// blank line: frame delimiter
			if hasData {
				return eventType, dataBuf.Bytes(), nil
			}
			continue
		}

		switch {
		case strings.HasPrefix(trimmed, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		case strings.HasPrefix(trimmed, "data:"):
			payload := strings.TrimPrefix(trimmed, "data:")
			payload = strings.TrimPrefix(payload, " ") // one optional leading space
			if hasData {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(payload)
			hasData = true
		}
		// other lines ignored

		if rerr == io.EOF {
			// last line without trailing newline
			if hasData {
				return eventType, dataBuf.Bytes(), nil
			}
			return "", nil, io.EOF
		}
	}
}
