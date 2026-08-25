package tools

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/stretchr/testify/require"
)

func TestMCPImageMediaResponseVisionFallback(t *testing.T) {
	t.Parallel()

	result := mcp.ToolResult{
		Type:      "image",
		Data:      []byte("png-bytes"),
		MediaType: "image/png",
		Content:   "tool says hi",
	}

	t.Run("describes image when model cannot see", func(t *testing.T) {
		t.Parallel()

		ctx := context.WithValue(context.Background(), SupportsImagesContextKey, false)
		ctx = context.WithValue(ctx, VisionDescribeFuncContextKey,
			DescribeImageFunc(func(_ context.Context, data []byte, mediaType string) (string, error) {
				require.Equal(t, result.Data, data)
				require.Equal(t, "image/png", mediaType)
				return "A chart of monthly sales.", nil
			}))

		resp, err := imageMediaResponse(ctx, result)
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Contains(t, resp.Content, "tool says hi")
		require.Contains(t, resp.Content, "[Image description]")
		require.Contains(t, resp.Content, "A chart of monthly sales.")
	})

	t.Run("errors without describe func", func(t *testing.T) {
		t.Parallel()

		ctx := context.WithValue(context.Background(), SupportsImagesContextKey, false)
		resp, err := imageMediaResponse(ctx, result)
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.Contains(t, resp.Content, "does not support image data")
	})

	t.Run("describe failure falls back to error", func(t *testing.T) {
		t.Parallel()

		ctx := context.WithValue(context.Background(), SupportsImagesContextKey, false)
		ctx = context.WithValue(ctx, VisionDescribeFuncContextKey,
			DescribeImageFunc(func(context.Context, []byte, string) (string, error) {
				return "", context.DeadlineExceeded
			}))

		resp, err := imageMediaResponse(ctx, result)
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.Contains(t, resp.Content, "does not support image data")
	})

	t.Run("non-image media type errors without describing", func(t *testing.T) {
		t.Parallel()

		mediaResult := mcp.ToolResult{
			Type:      "media",
			Data:      []byte("audio-bytes"),
			MediaType: "audio/wav",
		}
		ctx := context.WithValue(context.Background(), SupportsImagesContextKey, false)
		ctx = context.WithValue(ctx, VisionDescribeFuncContextKey,
			DescribeImageFunc(func(context.Context, []byte, string) (string, error) {
				t.Fatal("describe must not be called for non-image media")
				return "", nil
			}))

		resp, err := imageMediaResponse(ctx, mediaResult)
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.Contains(t, resp.Content, "does not support image data")
	})

	t.Run("vision model gets the raw image response", func(t *testing.T) {
		t.Parallel()

		ctx := context.WithValue(context.Background(), SupportsImagesContextKey, true)
		resp, err := imageMediaResponse(ctx, result)
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Equal(t, "tool says hi", resp.Content)
	})
}

// findRefs collects every $ref value left anywhere in a schema.
func findRefs(node any, out *[]string) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if k == "$ref" {
				if s, ok := v.(string); ok {
					*out = append(*out, s)
				}
				continue
			}
			findRefs(v, out)
		}
	case []any:
		for _, v := range n {
			findRefs(v, out)
		}
	}
}

func TestInlineSchemaRefsResolvesAgainstDefs(t *testing.T) {
	t.Parallel()

	// Shape of browser39_fill: $defs at the top level, $ref under
	// properties. Info drops the former and keeps the latter.
	input := map[string]any{
		"$defs": map[string]any{
			"FillFieldParam": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"selector": map[string]any{"type": "string"},
					"value":    map[string]any{"type": "string"},
				},
				"required": []any{"selector", "value"},
			},
		},
		"type": "object",
		"properties": map[string]any{
			"fields": map[string]any{
				"type":  []any{"array", "null"},
				"items": map[string]any{"$ref": "#/$defs/FillFieldParam"},
			},
		},
	}

	props := input["properties"].(map[string]any)
	got := inlineSchemaRefs(props, schemaDefs(input), nil).(map[string]any)

	var refs []string
	findRefs(got, &refs)
	require.Empty(t, refs, "no $ref may survive: $defs is not sent to the provider")

	items := got["fields"].(map[string]any)["items"].(map[string]any)
	require.Equal(t, "object", items["type"])
	require.Contains(t, items["properties"], "selector")
	require.Contains(t, items["properties"], "value")
}

func TestInlineSchemaRefsDropsUnresolvableRefs(t *testing.T) {
	t.Parallel()

	// No $defs block at all, plus a ref that points outside the document.
	// Both must be dropped rather than forwarded to the provider.
	input := map[string]any{
		"properties": map[string]any{
			"a": map[string]any{"$ref": "#/$defs/Missing"},
			"b": map[string]any{"$ref": "https://example.com/x.json"},
		},
	}

	got := inlineSchemaRefs(input["properties"], schemaDefs(input), nil)

	var refs []string
	findRefs(got, &refs)
	require.Empty(t, refs)
}

func TestInlineSchemaRefsKeepsSiblingKeys(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"$defs": map[string]any{
			"Node": map[string]any{"type": "object", "description": "from defs"},
		},
		"properties": map[string]any{
			"a": map[string]any{
				"$ref":        "#/$defs/Node",
				"description": "from sibling",
			},
		},
	}

	got := inlineSchemaRefs(input["properties"], schemaDefs(input), nil).(map[string]any)
	a := got["a"].(map[string]any)
	require.Equal(t, "object", a["type"], "target keys are inlined")
	require.Equal(t, "from sibling", a["description"], "sibling keys win over the target's")
}

func TestInlineSchemaRefsTerminatesOnCycle(t *testing.T) {
	t.Parallel()

	// A self-referential $def must not recurse forever.
	input := map[string]any{
		"$defs": map[string]any{
			"Node": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"child": map[string]any{"$ref": "#/$defs/Node"},
				},
			},
		},
		"properties": map[string]any{
			"root": map[string]any{"$ref": "#/$defs/Node"},
		},
	}

	done := make(chan map[string]any, 1)
	go func() {
		done <- inlineSchemaRefs(input["properties"], schemaDefs(input), nil).(map[string]any)
	}()

	select {
	case got := <-done:
		var refs []string
		findRefs(got, &refs)
		require.Empty(t, refs)
	case <-t.Context().Done():
		t.Fatal("inlineSchemaRefs did not terminate on a cyclic $ref")
	}
}

func TestSchemaDefsSupportsDraft07(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"definitions": map[string]any{
			"Node": map[string]any{"type": "string"},
		},
		"properties": map[string]any{
			"a": map[string]any{"$ref": "#/definitions/Node"},
		},
	}

	got := inlineSchemaRefs(input["properties"], schemaDefs(input), nil).(map[string]any)
	require.Equal(t, "string", got["a"].(map[string]any)["type"])
}

// TestInfoEmitsNoDanglingRefs guards the wiring, not just the helper: Info
// drops the $defs block, so it must resolve $refs before handing the schema
// to the provider. A dangling pointer makes strict validators reject the
// entire request, taking every other tool down with it.
func TestInfoEmitsNoDanglingRefs(t *testing.T) {
	t.Parallel()

	tool := &Tool{
		mcpName: "browser39",
		tool: &mcp.Tool{
			Name: "browser39_fill",
			InputSchema: map[string]any{
				"$defs": map[string]any{
					"FillFieldParam": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"selector": map[string]any{"type": "string"},
						},
					},
				},
				"type": "object",
				"properties": map[string]any{
					"fields": map[string]any{
						"type":  "array",
						"items": map[string]any{"$ref": "#/$defs/FillFieldParam"},
					},
				},
				"required": []any{"fields"},
			},
		},
	}

	info := tool.Info()

	var refs []string
	findRefs(info.Parameters, &refs)
	require.Empty(t, refs, "Info must not emit a $ref it did not also send a $defs for")

	require.Equal(t, []string{"fields"}, info.Required)
	items := info.Parameters["fields"].(map[string]any)["items"].(map[string]any)
	require.Contains(t, items["properties"], "selector")
}
