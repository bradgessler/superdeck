-- +goose Up
-- proven: this instance has run properly at least once. One that has is kept
-- and retried however often it fails; one that never worked is dropped after
-- its first failure rather than retried for ever.
ALTER TABLE instances ADD COLUMN proven INTEGER NOT NULL DEFAULT 0;

-- hpid: the process id of the holder keeping this run alive, so that a
-- restarted Superdeck can tell a holder that is busy from one that is gone.
ALTER TABLE runs ADD COLUMN hpid INTEGER NOT NULL DEFAULT 0;

-- Instances saved by earlier versions were running, so they count as proven.
UPDATE instances SET proven = 1;

-- +goose Down
ALTER TABLE runs DROP COLUMN hpid;
ALTER TABLE instances DROP COLUMN proven;
