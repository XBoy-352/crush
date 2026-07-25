package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/openai/openai-go/v3/packages/ssestream"
)

// streamIdleTimeout is how long a provider stream may sit with no
// parts before Crush aborts the attempt. Without this, a half-open
// TCP connection (common after rate limits or proxy stalls) leaves
// the UI spinning indefinitely with no retry and no error.
// Mutable for tests only.
var streamIdleTimeout = 2 * time.Minute

// retryableLanguageModel wraps a fantasy.LanguageModel so stream
// failures that fantasy leaves as raw SDK errors (notably mid-stream
// rate limits delivered as *ssestream.StreamError) become retryable
// ProviderErrors, and stalled streams time out into a retryable
// incomplete-stream error.
type retryableLanguageModel struct {
	inner fantasy.LanguageModel
}

func wrapRetryableModel(m fantasy.LanguageModel) fantasy.LanguageModel {
	if m == nil {
		return nil
	}
	if _, ok := m.(*retryableLanguageModel); ok {
		return m
	}
	return &retryableLanguageModel{inner: m}
}

func (m *retryableLanguageModel) Provider() string { return m.inner.Provider() }
func (m *retryableLanguageModel) Model() string    { return m.inner.Model() }

func (m *retryableLanguageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	resp, err := m.inner.Generate(ctx, call)
	return resp, mapRetryableStreamErr(err)
}

func (m *retryableLanguageModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	resp, err := m.inner.GenerateObject(ctx, call)
	return resp, mapRetryableStreamErr(err)
}

func (m *retryableLanguageModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	stream, err := m.inner.StreamObject(ctx, call)
	if err != nil {
		return nil, mapRetryableStreamErr(err)
	}
	return stream, nil
}

func (m *retryableLanguageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	// Derive a cancelable context so an idle timeout can unblock the
	// provider stream (providers that honor ctx) and the pull goroutine
	// can exit cleanly instead of leaking.
	idleCtx, cancelIdle := context.WithCancel(ctx)
	stream, err := m.inner.Stream(idleCtx, call)
	if err != nil {
		cancelIdle()
		return nil, mapRetryableStreamErr(err)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		defer cancelIdle()
		next, stop := iter.Pull(stream)
		defer stop()

		type pullResult struct {
			part fantasy.StreamPart
			ok   bool
		}

		for {
			ch := make(chan pullResult, 1)
			go func() {
				part, ok := next()
				ch <- pullResult{part: part, ok: ok}
			}()

			timer := time.NewTimer(streamIdleTimeout)
			var res pullResult
			select {
			case <-ctx.Done():
				timer.Stop()
				cancelIdle()
				// Drain the in-flight pull so stop() does not hang.
				<-ch
				return
			case <-timer.C:
				timer.Stop()
				cancelIdle()
				// Drain so stop() can return after the provider unblocks.
				<-ch
				_ = yield(fantasy.StreamPart{
					Type:  fantasy.StreamPartTypeError,
					Error: idleStreamTimeoutError(),
				})
				return
			case res = <-ch:
				timer.Stop()
			}

			if !res.ok {
				return
			}
			if res.part.Type == fantasy.StreamPartTypeError && res.part.Error != nil {
				res.part.Error = mapRetryableStreamErr(res.part.Error)
			}
			if !yield(res.part) {
				return
			}
		}
	}, nil
}

func idleStreamTimeoutError() error {
	return &fantasy.ProviderError{
		Title:   "stream idle timeout",
		Message: "provider stream produced no data for " + streamIdleTimeout.String(),
		// io.ErrUnexpectedEOF makes ProviderError.IsRetryable() true so
		// the existing retry middleware re-runs the step.
		Cause: io.ErrUnexpectedEOF,
	}
}

// mapRetryableStreamErr promotes mid-stream provider failures that
// fantasy's openai adapter leaves unwrapped into retryable
// ProviderErrors. OpenAI-compatible gateways (Hyper, xAI, …) often
// emit rate limits as SSE error events (*ssestream.StreamError)
// rather than HTTP 429 on the initial response; without this those
// errors skip the retry budget and kill the turn immediately.
func mapRetryableStreamErr(err error) error {
	if err == nil {
		return nil
	}
	// Already a ProviderError: leave classification to fantasy.
	var pe *fantasy.ProviderError
	if errors.As(err, &pe) {
		return err
	}
	if fantasy.IsTransportError(err) {
		return fantasy.NewTransportError(err)
	}

	var streamErr *ssestream.StreamError
	if errors.As(err, &streamErr) {
		if status, retryable, title, msg := classifyStreamError(streamErr); retryable {
			return &fantasy.ProviderError{
				Title:      title,
				Message:    msg,
				Cause:      err,
				StatusCode: status,
			}
		}
		// Non-retryable stream error still gets a clean ProviderError
		// so the UI shows a title instead of the raw SDK string.
		title, msg := streamErrorDisplay(streamErr)
		return &fantasy.ProviderError{
			Title:   title,
			Message: msg,
			Cause:   err,
		}
	}

	// Fallback for wrappers that string-ify the stream error.
	if isRateLimitMessage(err.Error()) {
		return &fantasy.ProviderError{
			Title:      "rate limit",
			Message:    err.Error(),
			Cause:      err,
			StatusCode: http.StatusTooManyRequests,
		}
	}
	return err
}

func classifyStreamError(err *ssestream.StreamError) (status int, retryable bool, title, msg string) {
	title, msg = streamErrorDisplay(err)
	raw := err.Error()
	if len(err.Event.Data) > 0 {
		raw = raw + " " + string(err.Event.Data)
	}
	if isRateLimitMessage(raw) {
		return http.StatusTooManyRequests, true, "rate limit", msg
	}
	// Overload / temporary server failures sometimes arrive as SSE
	// error events with 5xx-shaped payloads.
	if strings.Contains(strings.ToLower(raw), "overloaded") ||
		strings.Contains(strings.ToLower(raw), "server_error") ||
		strings.Contains(strings.ToLower(raw), "internal_error") {
		return http.StatusInternalServerError, true, "provider overloaded", msg
	}
	return 0, false, title, msg
}

func streamErrorDisplay(err *ssestream.StreamError) (title, msg string) {
	msg = strings.TrimSpace(err.Error())
	title = "stream error"
	if len(err.Event.Data) == 0 {
		return title, msg
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(err.Event.Data, &payload) != nil {
		return title, msg
	}
	if payload.Error.Type != "" {
		title = strings.ReplaceAll(payload.Error.Type, "_", " ")
	} else if payload.Type != "" {
		title = strings.ReplaceAll(payload.Type, "_", " ")
	}
	if payload.Error.Message != "" {
		msg = payload.Error.Message
	} else if payload.Message != "" {
		msg = payload.Message
	}
	return title, msg
}

func isRateLimitMessage(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests")
}
