package session

import (
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSessionService(t *testing.T) (Service, message.Service) {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	q := db.New(conn)
	return NewService(q, conn), message.NewService(q)
}

// seedForkFixture creates an origin session with five messages and returns
// the session plus the message IDs in insertion order.
func seedForkFixture(t *testing.T, sessions Service, messages message.Service) (Session, []string) {
	t.Helper()
	origin, err := sessions.Create(t.Context(), "Origin")
	require.NoError(t, err)

	if messages == nil {
		return origin, nil
	}

	var ids []string
	for _, role := range []message.MessageRole{message.User, message.Assistant, message.User, message.Assistant, message.User} {
		msg, err := messages.Create(t.Context(), origin.ID, message.CreateMessageParams{Role: role})
		require.NoError(t, err)
		ids = append(ids, msg.ID)
	}
	return origin, ids
}

func TestForkSession(t *testing.T) {
	t.Run("copies messages before checkpoint into a root fork", func(t *testing.T) {
		sessions, messages := newTestSessionService(t)
		origin, ids := seedForkFixture(t, sessions, messages)

		// Fork at the third message: the first two are copied.
		fork, err := sessions.ForkSession(t.Context(), origin.ID, ids[2], "Fork")
		require.NoError(t, err)

		assert.NotEqual(t, origin.ID, fork.ID)
		assert.Empty(t, fork.ParentSessionID, "a fork must be a root session")
		assert.Equal(t, origin.ID, fork.ForkedFromSessionID)
		assert.Equal(t, "Fork", fork.Title)

		copied, err := messages.List(t.Context(), fork.ID)
		require.NoError(t, err)
		require.Len(t, copied, 2)
		// Copies get fresh IDs (messages.id is a global PK) in the
		// original insertion order.
		assert.NotEqual(t, ids[0], copied[0].ID)
		assert.Equal(t, message.User, copied[0].Role)
		assert.Equal(t, message.Assistant, copied[1].Role)
		assert.NotEqual(t, ids[1], copied[1].ID)
	})

	t.Run("origin is untouched", func(t *testing.T) {
		sessions, messages := newTestSessionService(t)
		origin, _ := seedForkFixture(t, sessions, messages)

		_, ids := seedForkFixture(t, sessions, messages) //nolint:dogsled
		_ = ids

		fresh, err := messages.List(t.Context(), origin.ID)
		require.NoError(t, err)
		require.Len(t, fresh, 5)

		fetched, err := sessions.Get(t.Context(), origin.ID)
		require.NoError(t, err)
		assert.Empty(t, fetched.ForkedFromSessionID)
	})

	t.Run("fork appears in ListSessions and ListForks but not as a child", func(t *testing.T) {
		sessions, messages := newTestSessionService(t)
		origin, ids := seedForkFixture(t, sessions, messages)

		fork, err := sessions.ForkSession(t.Context(), origin.ID, ids[1], "Branch A")
		require.NoError(t, err)
		fork2, err := sessions.ForkSession(t.Context(), origin.ID, ids[3], "Branch B")
		require.NoError(t, err)

		roots, err := sessions.List(t.Context())
		require.NoError(t, err)
		rootIDs := make(map[string]bool, len(roots))
		for _, s := range roots {
			rootIDs[s.ID] = true
		}
		assert.True(t, rootIDs[fork.ID], "fork must appear in the root picker")
		assert.True(t, rootIDs[fork2.ID])

		children, err := sessions.ListChildren(t.Context(), origin.ID)
		require.NoError(t, err)
		assert.Empty(t, children, "parent_session_id must stay reserved for sub-sessions")

		forks, err := sessions.ListForks(t.Context(), origin.ID)
		require.NoError(t, err)
		require.Len(t, forks, 2)
		assert.Equal(t, fork.ID, forks[0].ID, "ListForks must order by insertion")
		assert.Equal(t, fork2.ID, forks[1].ID)
	})

	t.Run("checkpoint from another session is rejected", func(t *testing.T) {
		sessions, messages := newTestSessionService(t)
		origin, ids := seedForkFixture(t, sessions, messages)

		other, err := sessions.Create(t.Context(), "Other")
		require.NoError(t, err)

		_, err = sessions.ForkSession(t.Context(), other.ID, ids[1], "Bad")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not belong")

		forks, err := sessions.ListForks(t.Context(), origin.ID)
		require.NoError(t, err)
		assert.Empty(t, forks, "failed fork must leave no session row behind")
	})

	t.Run("nonexistent checkpoint is rejected without side effects", func(t *testing.T) {
		sessions, messages := newTestSessionService(t)
		origin, _ := seedForkFixture(t, sessions, messages)

		_, err := sessions.ForkSession(t.Context(), origin.ID, "no-such-message", "Bad")
		require.Error(t, err)

		forks, err := sessions.ListForks(t.Context(), origin.ID)
		require.NoError(t, err)
		assert.Empty(t, forks)
	})
}

func TestListForks_EmptyOrigin(t *testing.T) {
	sessions, _ := newTestSessionService(t)
	forks, err := sessions.ListForks(t.Context(), "")
	require.NoError(t, err)
	assert.Empty(t, forks)
}
