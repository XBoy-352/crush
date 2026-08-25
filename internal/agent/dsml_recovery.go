package agent

import (
	"strings"

	"charm.land/fantasy"
	"github.com/google/uuid"
)

// dsmlRecovery buffers streamed text so leaked DSML tool-call markup can
// be intercepted at the model boundary. Text deltas are held back; every
// other part type passes through untouched. If the provider finishes
// with "stop" and emitted zero native tool calls, the buffered text is
// scanned for markup referencing tools from the current call, and any
// recovered calls are synthesized as native tool call parts with a
// FinishReasonToolCalls finish. Otherwise the buffered text is flushed
// exactly as it arrived.
type dsmlRecovery struct {
	call              fantasy.Call
	bufferedText      []fantasy.StreamPart
	hasNativeToolCall bool
	passthrough       bool
}

func newDSMLRecovery(call fantasy.Call) *dsmlRecovery {
	return &dsmlRecovery{call: call}
}

// handlePart processes one stream part, yielding zero or more output
// parts downstream. It returns false if the consumer stopped the stream.
func (r *dsmlRecovery) handlePart(part fantasy.StreamPart, yield func(fantasy.StreamPart) bool) bool {
	if r.passthrough {
		return yield(part)
	}
	switch part.Type {
	case fantasy.StreamPartTypeTextStart, fantasy.StreamPartTypeTextDelta,
		fantasy.StreamPartTypeTextEnd:
		// Buffer the whole text segment so start/delta/end stay ordered
		// if they must be replayed after a recovery decision.
		r.bufferedText = append(r.bufferedText, part)
		return true
	case fantasy.StreamPartTypeToolCall, fantasy.StreamPartTypeToolInputStart:
		// Native tool calls mean recovery can never trigger: flush the
		// buffered text and pass everything straight through so the
		// original part ordering is preserved exactly.
		r.hasNativeToolCall = true
		r.passthrough = true
		if !r.flush(yield, fantasy.StreamPart{}) {
			return false
		}
		return yield(part)
	case fantasy.StreamPartTypeFinish:
		if r.shouldRecover(part) {
			return r.recover(yield, part)
		}
		return r.flush(yield, part)
	default:
		return yield(part)
	}
}

// shouldRecover reports whether the finish part qualifies for DSML
// recovery: the model stopped without native tool calls.
func (r *dsmlRecovery) shouldRecover(part fantasy.StreamPart) bool {
	return !r.hasNativeToolCall &&
		part.FinishReason == fantasy.FinishReasonStop &&
		len(r.call.Tools) > 0
}

// flush emits all buffered text parts, optionally followed by a final
// part such as the finish part.
func (r *dsmlRecovery) flush(yield func(fantasy.StreamPart) bool, final fantasy.StreamPart) bool {
	for _, part := range r.bufferedText {
		if !yield(part) {
			return false
		}
	}
	r.bufferedText = nil
	if final.Type != "" {
		if !yield(final) {
			return false
		}
	}
	return true
}

// recover strips leaked markup from the buffered text, emits the cleaned
// text, then synthesizes tool call parts for each recovered call before
// rewriting the finish reason to tool_calls. Usage and provider metadata
// on the original finish part are preserved.
func (r *dsmlRecovery) recover(yield func(fantasy.StreamPart) bool, finish fantasy.StreamPart) bool {
	var full strings.Builder
	for _, part := range r.bufferedText {
		full.WriteString(part.Delta)
	}

	names := make([]string, len(r.call.Tools))
	for i, tool := range r.call.Tools {
		names[i] = tool.GetName()
	}
	calls, cleaned := ExtractLeakedToolCalls(full.String(), names)
	if len(calls) == 0 {
		return r.flush(yield, finish)
	}

	for i, part := range r.bufferedText {
		if part.Type == fantasy.StreamPartTypeTextDelta {
			r.bufferedText[i].Delta = ""
		}
	}
	var placed bool
	for i, part := range r.bufferedText {
		if part.Type != fantasy.StreamPartTypeTextDelta || placed {
			continue
		}
		r.bufferedText[i].Delta = cleaned
		placed = true
		break
	}
	if !placed && cleaned != "" {
		// No delta part carried text (edge case); emit the cleaned text
		// as its own delta before the closing parts.
		r.bufferedText = append(r.bufferedText, fantasy.StreamPart{
			Type:  fantasy.StreamPartTypeTextDelta,
			ID:    finish.ID,
			Delta: cleaned,
		})
	}
	for _, part := range r.bufferedText {
		if !yield(part) {
			return false
		}
	}

	for _, call := range calls {
		input, err := MarshalLeakedToolCall(call)
		if err != nil {
			continue
		}
		id := uuid.NewString()
		parts := []fantasy.StreamPart{
			{
				Type:         fantasy.StreamPartTypeToolInputStart,
				ID:           id,
				ToolCallName: call.Name,
			},
			{
				Type:          fantasy.StreamPartTypeToolCall,
				ID:            id,
				ToolCallName:  call.Name,
				ToolCallInput: input,
			},
		}
		for _, part := range parts {
			if !yield(part) {
				return false
			}
		}
	}

	finish.FinishReason = fantasy.FinishReasonToolCalls
	return yield(finish)
}
