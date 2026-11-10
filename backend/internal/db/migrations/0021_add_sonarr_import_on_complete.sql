-- +goose Up
-- +goose StatementBegin
-- Per-Sonarr-instance opt-in: when a torrent Marauder delivered for a topic this
-- instance created finishes downloading, and it is not the torrent Sonarr
-- grabbed itself, ask Sonarr to import its files (ManualImport, copy mode).
-- Off by default: importing behind Sonarr's back is a behaviour change an
-- operator has to choose.
ALTER TABLE sonarr_instances
    ADD COLUMN import_on_complete BOOLEAN NOT NULL DEFAULT false;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sonarr_instances
    DROP COLUMN import_on_complete;
-- +goose StatementEnd
