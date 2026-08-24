package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
)

// describeImageTimeout bounds each vision side-call so a slow vision
// model cannot stall a turn indefinitely.
const describeImageTimeout = 30 * time.Second

// describeImagePrompt is the system prompt sent to the vision model.
const describeImagePrompt = "Describe everything visible in this image in " +
	"thorough detail. Include any text, code, data, objects, people, layout, " +
	"colors, UI elements, and any other notable visual information. " +
	"Preserve exact text where readable."

// DescribeImage runs a one-shot vision call on the given model and returns a
// text description of the image bytes.
func DescribeImage(ctx context.Context, m fantasy.LanguageModel, data []byte, mediaType string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, describeImageTimeout)
	defer cancel()

	agent := fantasy.NewAgent(
		wrapRetryableModel(m),
		fantasy.WithSystemPrompt(describeImagePrompt),
		fantasy.WithUserAgent(userAgent),
	)
	res, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Prompt:  "Describe this image:",
		Files:   []fantasy.FilePart{{Filename: "image", Data: data, MediaType: mediaType}},
		Headers: sessionHeaders(""),
	})
	if err != nil {
		return "", fmt.Errorf("vision description failed: %w", err)
	}
	description := strings.TrimSpace(res.Response.Content.Text())
	if description == "" {
		return "", fmt.Errorf("vision model returned an empty description")
	}
	return description, nil
}

// isImageMediaType reports whether the MIME type identifies an image.
func isImageMediaType(mediaType string) bool {
	return strings.HasPrefix(mediaType, "image/")
}

// transcribeImageParts replaces image content with text descriptions from the
// small (vision-capable) model for runs on a non-vision large model.
//
// Image attachments destined for `files` are dropped and their descriptions
// are appended as a leading user message. Historical image FileParts inside
// user messages — which preparePrompt would otherwise strip — are replaced
// in place with TextParts. When no images were present, or the small model
// cannot see either, the inputs are returned unchanged so the run falls back
// to today's silent-drop behavior.
func transcribeImageParts(
	ctx context.Context,
	history []fantasy.Message,
	files []fantasy.FilePart,
	largeModel Model,
	smallModel Model,
	sessionID string,
) ([]fantasy.Message, []fantasy.FilePart) {
	if largeModel.CatwalkCfg.SupportsImages || !smallModel.CatwalkCfg.SupportsImages {
		return history, files
	}

	hasImages := false
	for _, f := range files {
		if isImageMediaType(f.MediaType) {
			hasImages = true
			break
		}
	}
	if !hasImages {
		for _, msg := range history {
			if msg.Role != fantasy.MessageRoleUser {
				continue
			}
			for _, part := range msg.Content {
				if _, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
					hasImages = true
					break
				}
			}
			if hasImages {
				break
			}
		}
	}
	if !hasImages {
		return history, files
	}

	describe := func(f fantasy.FilePart) (string, bool) {
		desc, err := DescribeImage(ctx, smallModel.Model, f.Data, f.MediaType)
		if err != nil {
			slog.Warn("Failed to describe image with vision model; dropping image", "error", err)
			return "", false
		}
		return desc, true
	}

	// Replace historical image FileParts with text descriptions.
	for i := range history {
		if history[i].Role != fantasy.MessageRoleUser {
			continue
		}
		parts := history[i].Content
		replaced := false
		for j, part := range parts {
			filePart, ok := fantasy.AsMessagePart[fantasy.FilePart](part)
			if !ok || !isImageMediaType(filePart.MediaType) {
				continue
			}
			desc, described := describe(filePart)
			if described {
				parts[j] = fantasy.TextPart{Text: fmt.Sprintf("<image filename=%q>\n%s\n</image>", filePart.Filename, desc)}
				replaced = true
			} else {
				parts = append(parts[:j], parts[j+1:]...)
				j--
			}
		}
		history[i].Content = parts
		if replaced {
			history[i].ProviderOptions = nil
		}
	}

	// Transcribe current-turn attachments and drop them from files. The
	// descriptions ride along as a synthetic user message prepended to
	// the history.
	var descriptions []string
	filteredFiles := files[:0]
	for _, f := range files {
		if !isImageMediaType(f.MediaType) {
			filteredFiles = append(filteredFiles, f)
			continue
		}
		if desc, ok := describe(f); ok {
			descriptions = append(descriptions, fmt.Sprintf("<image filename=%q>\n%s\n</image>", f.Filename, desc))
		}
	}
	files = filteredFiles
	if len(descriptions) > 0 {
		msg := fantasy.NewUserMessage(strings.Join(descriptions, "\n\n"))
		history = append(history, msg)
	}

	return history, files
}
