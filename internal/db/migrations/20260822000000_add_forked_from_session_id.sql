-- +goose Up
-- forked_from_session_id links a user-created fork to the session it was
-- copied from. Forks stay root sessions (parent_session_id stays NULL) so
-- they keep appearing in the session picker and remain continuable;
-- parent_session_id is reserved for agent-tool sub-sessions.
ALTER TABLE sessions ADD COLUMN forked_from_session_id TEXT;

CREATE INDEX idx_sessions_forked_from ON sessions (forked_from_session_id);

-- +goose Down
DROP INDEX idx_sessions_forked_from;
ALTER TABLE sessions DROP COLUMN forked_from_session_id;
