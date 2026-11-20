-- +goose Up
-- A run is one Claude process Superdeck started. Each lives in its own tmux
-- session, named by its id, so it keeps going while Superdeck restarts. The
-- row is how a restarted Superdeck knows what each session is.
CREATE TABLE runs (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    account_id TEXT NOT NULL DEFAULT '',
    folder     TEXT NOT NULL DEFAULT '',
    dir        TEXT NOT NULL,
    name       TEXT NOT NULL,
    spawn      TEXT NOT NULL DEFAULT '',
    capacity   INTEGER NOT NULL DEFAULT 0,
    version    TEXT NOT NULL DEFAULT '',
    restarts   INTEGER NOT NULL DEFAULT 0,
    started_at TEXT NOT NULL,
    stopped    INTEGER NOT NULL DEFAULT 0,
    ended_at   TEXT,
    exit_code  INTEGER
);

-- +goose Down
DROP TABLE runs;
