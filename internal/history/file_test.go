package history

import (
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestHistory_CreateAndCreateVersion(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "test-session")
	require.NoError(t, err)

	hist := NewService(q, conn)

	// Create initial file
	file1, err := hist.Create(t.Context(), sess.ID, "main.go", "package main")
	require.NoError(t, err)
	require.Equal(t, int64(0), file1.Version)
	require.Equal(t, "package main", file1.Content)

	// Create version 1
	file2, err := hist.CreateVersion(t.Context(), sess.ID, "main.go", "package main\n\nfunc main() {}")
	require.NoError(t, err)
	require.Equal(t, int64(1), file2.Version)
	require.Equal(t, "package main\n\nfunc main() {}", file2.Content)

	// Create version 2
	file3, err := hist.CreateVersion(t.Context(), sess.ID, "main.go", "package main\n\nfunc main() { println(1) }")
	require.NoError(t, err)
	require.Equal(t, int64(2), file3.Version)
}
