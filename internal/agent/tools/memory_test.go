package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func runMemoryTool(t *testing.T, dataDir string, params MemoryWriteParams) fantasy.ToolResponse {
	t.Helper()
	tool := NewMemoryTool(dataDir)
	input, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "test-call",
		Name:  MemoryWriteToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	return resp
}

func TestMemoryWriteSaveAndIndex(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	resp := runMemoryTool(t, dataDir, MemoryWriteParams{
		Action:      "save",
		Name:        "build-commands",
		Description: "How to run tests",
		Content:     "Use `task test` for unit tests.",
	})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, `Saved memory "build-commands"`)
	require.Contains(t, resp.Content, "next crush launch")

	path := filepath.Join(dataDir, "memory", "build-commands.md")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "---\ndescription: How to run tests\n---\n\nUse `task test` for unit tests.", string(b))

	index, err := os.ReadFile(filepath.Join(dataDir, "memory", "MEMORY.md"))
	require.NoError(t, err)
	require.Equal(t, "# Memory index\n\n- build-commands: How to run tests\n", string(index))
}

func TestMemoryWriteOverwrite(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	resp := runMemoryTool(t, dataDir, MemoryWriteParams{
		Action:      "save",
		Name:        "pref",
		Description: "old",
		Content:     "old content",
	})
	require.False(t, resp.IsError)

	resp = runMemoryTool(t, dataDir, MemoryWriteParams{
		Action:      "save",
		Name:        "pref",
		Description: "new",
		Content:     "new content",
	})
	require.False(t, resp.IsError)

	b, err := os.ReadFile(filepath.Join(dataDir, "memory", "pref.md"))
	require.NoError(t, err)
	require.Contains(t, string(b), "description: new")
	require.Contains(t, string(b), "new content")

	index, err := os.ReadFile(filepath.Join(dataDir, "memory", "MEMORY.md"))
	require.NoError(t, err)
	require.Contains(t, string(index), "- pref: new")
	require.NotContains(t, string(index), "old")
}

func TestMemoryWriteDelete(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	resp := runMemoryTool(t, dataDir, MemoryWriteParams{
		Action:      "save",
		Name:        "temp-fact",
		Description: "temporary",
		Content:     "will be deleted",
	})
	require.False(t, resp.IsError)

	resp = runMemoryTool(t, dataDir, MemoryWriteParams{
		Action: "delete",
		Name:   "temp-fact",
	})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, `Deleted memory "temp-fact"`)

	_, err := os.Stat(filepath.Join(dataDir, "memory", "temp-fact.md"))
	require.True(t, os.IsNotExist(err))

	index, err := os.ReadFile(filepath.Join(dataDir, "memory", "MEMORY.md"))
	require.NoError(t, err)
	require.Equal(t, "# Memory index\n\n", string(index))
}

func TestMemoryWriteDeleteMissing(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	resp := runMemoryTool(t, dataDir, MemoryWriteParams{
		Action: "delete",
		Name:   "does-not-exist",
	})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "no memory named does-not-exist")
}

func TestMemoryWriteSlugValidation(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	cases := []string{
		"../x",
		"a/b",
		"UPPER",
		"",
		"has space",
		"under_score",
		"-leading-hyphen",
		strings.Repeat("a", 65),
	}
	for _, name := range cases {
		resp := runMemoryTool(t, dataDir, MemoryWriteParams{
			Action:      "save",
			Name:        name,
			Description: "desc",
			Content:     "body",
		})
		require.True(t, resp.IsError, "expected rejection for name %q", name)
	}
}

func TestMemoryWriteCaps(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	resp := runMemoryTool(t, dataDir, MemoryWriteParams{
		Action:      "save",
		Name:        "ok",
		Description: strings.Repeat("d", maxMemoryDescription+1),
		Content:     "body",
	})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "description must be at most")

	resp = runMemoryTool(t, dataDir, MemoryWriteParams{
		Action:      "save",
		Name:        "ok",
		Description: "desc",
		Content:     strings.Repeat("x", maxMemoryContentBytes+1),
	})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "content must be at most")

	resp = runMemoryTool(t, dataDir, MemoryWriteParams{
		Action: "save",
		Name:   "ok",
		// empty description
		Content: "body",
	})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "description is required")

	resp = runMemoryTool(t, dataDir, MemoryWriteParams{
		Action:      "save",
		Name:        "ok",
		Description: "desc",
	})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "content is required")

	resp = runMemoryTool(t, dataDir, MemoryWriteParams{
		Action:      "save",
		Name:        "ok",
		Description: "line1\nline2",
		Content:     "body",
	})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "description must be a single line")
}

func TestMemoryWriteFileCountCap(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	memoryDir := filepath.Join(dataDir, "memory")
	require.NoError(t, os.MkdirAll(memoryDir, 0o755))

	// Pre-fill to the limit without going through the tool so the test
	// stays fast.
	for i := range maxMemoryFiles {
		name := filepath.Join(memoryDir, "m"+strings.Repeat("a", 1)+string(rune('a'+i%26))+".md")
		// Use unique names via index.
		name = filepath.Join(memoryDir, "m"+itoa(i)+".md")
		require.NoError(t, os.WriteFile(name, []byte("---\ndescription: x\n---\n\nbody"), 0o644))
	}

	resp := runMemoryTool(t, dataDir, MemoryWriteParams{
		Action:      "save",
		Name:        "one-more",
		Description: "overflow",
		Content:     "should fail",
	})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "memory limit")

	// Overwriting an existing name must still succeed.
	resp = runMemoryTool(t, dataDir, MemoryWriteParams{
		Action:      "save",
		Name:        "m0",
		Description: "updated",
		Content:     "ok to overwrite at limit",
	})
	require.False(t, resp.IsError)
}

func TestMemoryWriteIndexSortedAndSlugFallback(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	memoryDir := filepath.Join(dataDir, "memory")
	require.NoError(t, os.MkdirAll(memoryDir, 0o755))

	// Hand-written file without frontmatter → description falls back to slug.
	require.NoError(t, os.WriteFile(filepath.Join(memoryDir, "zebra.md"), []byte("no frontmatter"), 0o644))

	resp := runMemoryTool(t, dataDir, MemoryWriteParams{
		Action:      "save",
		Name:        "alpha",
		Description: "first",
		Content:     "alpha body",
	})
	require.False(t, resp.IsError)

	index, err := os.ReadFile(filepath.Join(memoryDir, "MEMORY.md"))
	require.NoError(t, err)
	require.Equal(t, "# Memory index\n\n- alpha: first\n- zebra: zebra\n", string(index))
}

func TestMemoryWriteInvalidAction(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	resp := runMemoryTool(t, dataDir, MemoryWriteParams{
		Action: "update",
		Name:   "x",
	})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "action must be save or delete")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
