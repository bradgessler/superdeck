-- +goose Up
-- pid: the process id of the Claude process itself. A restarted Superdeck
-- that cannot reach a run's holder still knows which process the run is, so
-- it can tell whether it is alive and make sure it is never left running
-- beside a replacement.
ALTER TABLE runs ADD COLUMN pid INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE runs DROP COLUMN pid;
