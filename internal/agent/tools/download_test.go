package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestDownloadTool_UnreachableURLReturnsToolError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	perms := permission.NewPermissionService(tmpDir, true, nil)
	tool := NewDownloadTool(perms, tmpDir, http.DefaultClient)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	params := DownloadParams{
		URL:      "http://127.0.0.1:59999/nonexistent",
		FilePath: filepath.Join(tmpDir, "out.txt"),
	}

	paramsJSON, err := json.Marshal(params)
	require.NoError(t, err)

	call := fantasy.ToolCall{
		ID:    "call-1",
		Name:  DownloadToolName,
		Input: string(paramsJSON),
	}

	resp, err := tool.Run(ctx, call)
	require.NoError(t, err, "downloading from an unreachable URL should not return a Go system error")
	require.True(t, resp.IsError, "response should be marked as an error tool response")
	require.Contains(t, resp.Content, "failed to download from URL")
}
