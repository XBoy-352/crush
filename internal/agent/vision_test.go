package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// describeCaptureModel is a fantasy.LanguageModel stub that records the
// FileParts it was called with and returns a fixed text response.
type describeCaptureModel struct {
	capturedFiles []fantasy.FilePart
	response      string
	err           error
}

func (m *describeCaptureModel) Provider() string { return "test" }
func (m *describeCaptureModel) Model() string    { return "describe" }

func (m *describeCaptureModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	text := m.response
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: text})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd})
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
		})
	}, nil
}

func (m *describeCaptureModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	res, err := m.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	var response fantasy.Response
	for part := range res {
		if part.Type == fantasy.StreamPartTypeTextDelta {
			response.Content = append(response.Content, fantasy.TextContent{Text: part.Delta})
		}
	}
	return &response, nil
}

func (m *describeCaptureModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("unused")
}

func (m *describeCaptureModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("unused")
}

// captureFilesModel wraps Stream to snapshot the file parts embedded in the
// call's prompt messages before delegating to the inner model.
type captureFilesModel struct {
	inner  fantasy.LanguageModel
	onCall func([]fantasy.FilePart)
}

func (m *captureFilesModel) Provider() string { return m.inner.Provider() }
func (m *captureFilesModel) Model() string    { return m.inner.Model() }

func (m *captureFilesModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	var files []fantasy.FilePart
	for _, msg := range call.Prompt {
		for _, part := range msg.Content {
			if fp, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
				files = append(files, fp)
			}
		}
	}
	m.onCall(files)
	return m.inner.Stream(ctx, call)
}

func (m *captureFilesModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return m.inner.Generate(ctx, call)
}

func (m *captureFilesModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return m.inner.GenerateObject(ctx, call)
}

func (m *captureFilesModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return m.inner.StreamObject(ctx, call)
}

func TestDescribeImage(t *testing.T) {
	t.Parallel()

	model := &describeCaptureModel{response: "A red square on a blue background."}
	desc, err := DescribeImage(t.Context(), model, []byte("fakepng"), "image/png")
	require.NoError(t, err)
	require.Equal(t, "A red square on a blue background.", desc)
}

func TestDescribeImage_ErrorPropagates(t *testing.T) {
	t.Parallel()

	model := &describeCaptureModel{err: errors.New("provider down")}
	_, err := DescribeImage(t.Context(), model, []byte("fakepng"), "image/png")
	require.ErrorContains(t, err, "vision description failed")
}

func TestDescribeImage_EmptyResponseErrors(t *testing.T) {
	t.Parallel()

	model := &describeCaptureModel{response: ""}
	_, err := DescribeImage(t.Context(), model, []byte("fakepng"), "image/png")
	require.ErrorContains(t, err, "empty description")
}

func TestTranscribeImageParts_AttachmentsAndHistory(t *testing.T) {
	t.Parallel()

	largeModel := Model{CatwalkCfg: catwalkCfg(false)}
	smallModel := Model{
		Model:      &describeCaptureModel{response: "desc"},
		CatwalkCfg: catwalkCfg(true),
	}

	history := []fantasy.Message{
		fantasy.NewUserMessage("earlier", fantasy.FilePart{Filename: "old.png", MediaType: "image/png"}),
	}
	files := []fantasy.FilePart{{Filename: "new.png", Data: []byte("img"), MediaType: "image/png"}}

	newHistory, newFiles := transcribeImageParts(t.Context(), history, files, largeModel, smallModel, "sess")
	require.Empty(t, newFiles, "image attachments must be dropped from files")

	// The historical image part should be replaced by a text part.
	replaced := false
	for _, msg := range newHistory {
		for _, part := range msg.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				if contains := tp.Text; contains != "" && msg.Role == fantasy.MessageRoleUser {
					replaced = true
				}
			}
			if _, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
				t.Fatal("historical image FilePart must be replaced with a TextPart")
			}
		}
	}
	require.True(t, replaced, "expected a text replacement for the historical image")
}

func TestTranscribeImageParts_NoOpsForVisionLarge(t *testing.T) {
	t.Parallel()

	largeModel := Model{CatwalkCfg: catwalkCfg(true)}
	smallModel := Model{CatwalkCfg: catwalkCfg(false)}

	history := []fantasy.Message{fantasy.NewUserMessage("hi")}
	files := []fantasy.FilePart{{Filename: "a.png", MediaType: "image/png"}}

	newHistory, newFiles := transcribeImageParts(t.Context(), history, files, largeModel, smallModel, "sess")
	require.Equal(t, history, newHistory)
	require.Equal(t, files, newFiles)
}

func TestTranscribeImageParts_NoOpsForNonVisionSmall(t *testing.T) {
	t.Parallel()

	largeModel := Model{CatwalkCfg: catwalkCfg(false)}
	smallModel := Model{CatwalkCfg: catwalkCfg(false)}

	history := []fantasy.Message{fantasy.NewUserMessage("hi")}
	files := []fantasy.FilePart{{Filename: "a.png", MediaType: "image/png"}}

	newHistory, newFiles := transcribeImageParts(t.Context(), history, files, largeModel, smallModel, "sess")
	require.Equal(t, history, newHistory)
	require.Equal(t, files, newFiles)
}

func TestPreparePrompt_VisionTranscription(t *testing.T) {
	t.Parallel()

	describeModel := &describeCaptureModel{response: "described image"}
	smallVision := &captureFilesModel{
		inner: describeModel,
		onCall: func(files []fantasy.FilePart) {
			require.NotEmpty(t, files, "vision model should receive image parts")
		},
	}

	largeModel := Model{CatwalkCfg: catwalkCfg(false)}
	smallModel := Model{Model: smallVision, CatwalkCfg: catwalkCfg(true)}

	history, files := transcribeImageParts(
		t.Context(),
		nil,
		[]fantasy.FilePart{{Filename: "shot.png", Data: []byte("png-bytes"), MediaType: "image/png"}},
		largeModel,
		smallModel,
		"sess",
	)
	require.Empty(t, files)
	require.Len(t, history, 1)
	foundDesc := false
	for _, part := range history[0].Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			require.Contains(t, tp.Text, "described image")
			foundDesc = true
		}
	}
	require.True(t, foundDesc)
}

func TestViewTool_VisionFallbackContextKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	require.Nil(t, tools.GetDescribeImageFromContext(ctx), "no describe func should be set by default")

	ctx = context.WithValue(ctx, tools.VisionDescribeFuncContextKey,
		tools.DescribeImageFunc(func(context.Context, []byte, string) (string, error) {
			return "a description", nil
		}))
	require.NotNil(t, tools.GetDescribeImageFromContext(ctx))
	desc, err := tools.GetDescribeImageFromContext(ctx)(t.Context(), nil, "")
	require.NoError(t, err)
	require.Equal(t, "a description", desc)
}

// catwalkCfg builds a catwalk.Model with the given image capability.
func catwalkCfg(supportsImages bool) catwalk.Model {
	return catwalk.Model{SupportsImages: supportsImages}
}
