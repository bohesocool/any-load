package protocol

import (
	"bufio"
	"encoding/json"
	"io"
)

// Stream mode constants for per-group stream-mode configuration. Stored on
// models.Group.StreamMode; only consulted on the protocol-conversion path.
const (
	StreamModePassthrough    = "passthrough"     // upstream stream = client stream (default, no-op)
	StreamModeFakeNonStream  = "fake_non_stream" // force upstream to stream; client still follows its own intent
	StreamModeFakeStream     = "fake_stream"     // force upstream to non-stream; client still follows its own intent
)

// AccumulateStreamToResponse reads an upstream SSE stream (in the handler's
// format), converts it to IR stream events via the handler's stream parser,
// and accumulates the events into a single non-stream ChatResponse IR. It is
// the streaming→non-stream bridge used by the "fake non-stream" mode: the
// upstream is asked to stream, and the proxy buffers the whole response into
// one JSON object for the client.
//
// model is set on the returned IR because StreamEvent carries no model field
// (none of the format parsers capture the chunk-level model into the IR); if
// left empty, the inbound emitter would write "model":"" to the client, which
// is worse than a real non-stream response. The caller has the (redirected)
// IR model available and should pass it.
//
// The loop terminates on either ReadSSEFrame io.EOF (the upstream connection
// closed) or the parser returning io.EOF (a terminal event such as [DONE] or
// message_stop), mirroring handleStreamingConversionResponse. Gemini's parser
// never returns io.EOF, so the ReadSSEFrame EOF is what ends Gemini streams.
func AccumulateStreamToResponse(h FormatHandler, r io.Reader, model string) (*ChatResponse, error) {
	parser := h.NewUpstreamStreamParser()
	br := bufio.NewReaderSize(r, 64*1024)

	acc := &streamAccumulator{}
	for {
		eventType, data, err := ReadSSEFrame(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		events, perr := parser.Parse(eventType, data)
		for _, ev := range events {
			acc.apply(ev)
		}
		if perr == io.EOF {
			break
		}
		if perr != nil {
			return nil, perr
		}
	}

	return acc.response(model), nil
}

// streamAccumulator folds IR stream events into a ChatResponse. It is
// re-start-aware for tool calls: Gemini's stream parser uses the parts-array
// index as ToolCallIndex and resets it to 0 on every SSE frame, so multiple
// tool calls across frames would overwrite each other in a naive
// map[index]ToolCall. Gemini emits Start+Delta+Finish atomically in one Parse
// call, so the previous slot at a reused index is always finished before the
// next Start; we append a fresh slot and rebind the index to it. The other
// three formats use stable, unique indices and are unaffected.
type streamAccumulator struct {
	role         string
	content      []byte
	toolCalls    []ToolCall
	idxToSlot    map[int]int // ToolCallIndex → position in toolCalls
	finished     []bool      // per-slot finished flag, parallel to toolCalls
	finishReason string
	prompt       int
	completion   int
}

func (a *streamAccumulator) apply(ev StreamEvent) {
	switch ev.Type {
	case EventRole:
		if ev.Delta != "" {
			a.role = ev.Delta
		}
	case EventContent:
		a.content = append(a.content, ev.Delta...)
	case EventToolCallStart:
		if a.idxToSlot == nil {
			a.idxToSlot = make(map[int]int)
		}
		if slot, ok := a.idxToSlot[ev.ToolCallIndex]; ok && (a.isFinished(slot) || len(a.toolCalls[slot].Arguments) > 0) {
			// Index reused after the previous slot finished (Gemini): start a
			// new tool call and rebind the index to it.
			a.appendSlot(ev.ToolCallIndex, ev.ToolCallID, ev.ToolCallName)
		} else if !ok {
			a.appendSlot(ev.ToolCallIndex, ev.ToolCallID, ev.ToolCallName)
		} else {
			// Same index, not finished, no args yet: fill in id/name (e.g. an
			// OpenAI start frame split from its deltas).
			a.toolCalls[slot].ID = ev.ToolCallID
			a.toolCalls[slot].Name = ev.ToolCallName
		}
	case EventToolCallDelta:
		if slot, ok := a.idxToSlot[ev.ToolCallIndex]; ok {
			a.toolCalls[slot].Arguments = append(a.toolCalls[slot].Arguments, ev.ArgumentsDelta...)
		}
	case EventToolCallFinish:
		if slot, ok := a.idxToSlot[ev.ToolCallIndex]; ok {
			a.setFinished(slot, true)
		}
	case EventFinish:
		a.finishReason = ev.FinishReason
	case EventUsage:
		a.prompt = ev.PromptTokens
		a.completion = ev.CompletionTokens
	}
}

func (a *streamAccumulator) appendSlot(idx int, id, name string) {
	a.toolCalls = append(a.toolCalls, ToolCall{
		ID:        id,
		Name:      name,
		Arguments: json.RawMessage{},
	})
	a.finished = append(a.finished, false)
	a.idxToSlot[idx] = len(a.toolCalls) - 1
}

func (a *streamAccumulator) isFinished(slot int) bool {
	return slot < len(a.finished) && a.finished[slot]
}

func (a *streamAccumulator) setFinished(slot int, v bool) {
	if slot < len(a.finished) {
		a.finished[slot] = v
	}
}

func (a *streamAccumulator) response(model string) *ChatResponse {
	role := a.role
	if role == "" {
		role = "assistant"
	}
	finish := a.finishReason
	if finish == "" {
		finish = "stop"
	}
	ir := &ChatResponse{
		Model:           model,
		Choices:         []ChatChoice{{Index: 0, Message: ChatMessage{Role: role, Content: string(a.content), ToolCalls: a.toolCalls}, FinishReason: finish}},
		PromptTokens:    a.prompt,
		CompletionTokens: a.completion,
	}
	return ir
}

// ExpandResponseToEvents turns a non-stream ChatResponse IR into a sequence of
// IR stream events, as if the response had arrived streamed. It is the
// non-stream→streaming bridge used by the "fake stream" mode: the upstream is
// asked for a single JSON object, and the proxy expands it into an SSE stream
// for the client.
//
// Tool calls use the slice index (0, 1, …) as ToolCallIndex. Every inbound
// stream emitter consumes this scheme: OpenAI Chat uses it as the chunk index;
// Anthropic uses index+1 as the content-block index; Responses correlates
// Start (nextToolIdx++) with Delta (index+1); Gemini accumulates args and
// emits the whole call at Finish. Start always precedes Delta for a given
// index, and Finish follows, which is all the emitters require.
func ExpandResponseToEvents(ir *ChatResponse) []StreamEvent {
	var out []StreamEvent
	out = append(out, StreamEvent{Type: EventRole, Delta: "assistant"})

	if len(ir.Choices) > 0 {
		ch := ir.Choices[0]
		if ch.Message.Content != "" {
			out = append(out, StreamEvent{Type: EventContent, Delta: ch.Message.Content})
		}
		for i, tc := range ch.Message.ToolCalls {
			out = append(out, StreamEvent{
				Type: EventToolCallStart, ToolCallIndex: i,
				ToolCallID: tc.ID, ToolCallName: tc.Name,
			})
			out = append(out, StreamEvent{
				Type: EventToolCallDelta, ToolCallIndex: i,
				ArgumentsDelta: argsToString(tc.Arguments),
			})
			out = append(out, StreamEvent{Type: EventToolCallFinish, ToolCallIndex: i})
		}
	}

	finish := "stop"
	if len(ir.Choices) > 0 && ir.Choices[0].FinishReason != "" {
		finish = ir.Choices[0].FinishReason
	}
	out = append(out, StreamEvent{Type: EventFinish, FinishReason: finish})
	out = append(out, StreamEvent{
		Type:             EventUsage,
		PromptTokens:     ir.PromptTokens,
		CompletionTokens: ir.CompletionTokens,
	})
	return out
}
