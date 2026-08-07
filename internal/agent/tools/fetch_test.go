package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestFetchTool_UnreachableURLReturnsToolError(t *testing.T) {
	t.Parallel()

	perms := permission.NewPermissionService(t.TempDir(), true, nil)
	tool := NewFetchTool(perms, t.TempDir(), http.DefaultClient)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	params := FetchParams{
		URL:    "http://127.0.0.1:59999/nonexistent",
		Format: "text",
	}

	paramsJSON, err := json.Marshal(params)
	require.NoError(t, err)

	call := fantasy.ToolCall{
		ID:    "call-1",
		Name:  FetchToolName,
		Input: string(paramsJSON),
	}

	resp, err := tool.Run(ctx, call)
	require.NoError(t, err, "fetching an unreachable URL should not return a Go system error")
	require.True(t, resp.IsError, "response should be marked as an error tool response")
	require.Contains(t, resp.Content, "failed to fetch URL")
}
