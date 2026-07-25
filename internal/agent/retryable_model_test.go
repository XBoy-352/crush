package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/stretchr/testify/require"
)

func TestMapRetryableStreamErr_RateLimitSSE(t *testing.T) {
	t.Parallel()
	err := &ssestream.StreamError{
		Message: `received error while streaming: {"message":"Please try again in a few minutes.","type":"rate_limit_error","code":null}`,
		Event: ssestream.Event{
			Type: "error",
			Data: []byte(`{"message":"Please try again in a few minutes.","type":"rate_limit_error","code":null}`),
		},
	}
	mapped := mapRetryableStreamErr(err)
	var pe *fantasy.ProviderError
	require.ErrorAs(t, mapped, &pe)
	require.True(t, pe.IsRetryable(), "mid-stream rate limit must be retryable")
	require.Equal(t, http.StatusTooManyRequests, pe.StatusCode)
	require.Contains(t, pe.Message, "few minutes")
}

func TestMapRetryableStreamErr_NonRetryable(t *testing.T) {
	t.Parallel()
	err := &ssestream.StreamError{
		Message: `received error while streaming: {"message":"bad request","type":"invalid_request_error"}`,
		Event: ssestream.Event{
			Data: []byte(`{"message":"bad request","type":"invalid_request_error"}`),
		},
	}
	mapped := mapRetryableStreamErr(err)
	var pe *fantasy.ProviderError
	require.ErrorAs(t, mapped, &pe)
	require.False(t, pe.IsRetryable())
}

func TestMapRetryableStreamErr_PassthroughProviderError(t *testing.T) {
	t.Parallel()
	orig := &fantasy.ProviderError{
		Title:      "rate limit",
		Message:    "slow down",
		StatusCode: http.StatusTooManyRequests,
	}
	require.Same(t, orig, mapRetryableStreamErr(orig))
}

func TestMapRetryableStreamErr_Nil(t *testing.T) {
	t.Parallel()
	require.NoError(t, mapRetryableStreamErr(nil))
}

func TestMapRetryableStreamErr_StringRateLimit(t *testing.T) {
	t.Parallel()
	mapped := mapRetryableStreamErr(errors.New("received error while streaming: rate_limit_error"))
	var pe *fantasy.ProviderError
	require.ErrorAs(t, mapped, &pe)
	require.True(t, pe.IsRetryable())
}

func TestIdleStreamTimeoutError_IsRetryable(t *testing.T) {
	t.Parallel()
	err := idleStreamTimeoutError()
	var pe *fantasy.ProviderError
	require.ErrorAs(t, err, &pe)
	require.True(t, pe.IsRetryable())
	require.True(t, errors.Is(err, io.ErrUnexpectedEOF))
}

type stallModel struct {
	// hang forever when Stream is consumed.
}

func (stallModel) Provider() string { return "test" }
func (stallModel) Model() string    { return "stall" }
func (stallModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("unused")
}

func (stallModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("unused")
}

func (stallModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("unused")
}

func (stallModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		// Block until the wrapper cancels the idle context on timeout.
		<-ctx.Done()
	}, nil
}

func TestRetryableModel_StreamIdleTimeout(t *testing.T) {
	// Shorten the idle timeout for the test.
	old := streamIdleTimeout
	streamIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { streamIdleTimeout = old })

	m := wrapRetryableModel(stallModel{})
	stream, err := m.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)

	var got error
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			got = part.Error
			break
		}
	}
	require.Error(t, got)
	var pe *fantasy.ProviderError
	require.ErrorAs(t, got, &pe)
	require.True(t, pe.IsRetryable())
	require.Contains(t, pe.Title, "idle")
}

func TestWrapRetryableModel_Idempotent(t *testing.T) {
	t.Parallel()
	inner := stallModel{}
	once := wrapRetryableModel(inner)
	twice := wrapRetryableModel(once)
	require.Same(t, once, twice)
}
