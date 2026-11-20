-- +goose Up
-- An account is a Claude login. Its id is the name of its config directory,
-- which never changes because Claude keys the Keychain login to that path.
-- Its name is what people see and can be changed freely.
CREATE TABLE accounts (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- An instance someone launched and has not stopped. Superdeck brings these
-- back when it starts.
CREATE TABLE instances (
    id         INTEGER PRIMARY KEY,
    account_id TEXT NOT NULL,
    folder     TEXT NOT NULL,
    spawn      TEXT NOT NULL,
    capacity   INTEGER NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (account_id, folder)
);

-- +goose Down
DROP TABLE instances;
DROP TABLE accounts;
