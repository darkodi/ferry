-- Index for the resolver's scans of payouts waiting on the rail.
-- See "Failure and reversal" in docs/design.md.

-- +goose Up
CREATE INDEX payouts_in_flight_idx ON payouts (status, updated_at)
    WHERE status IN ('held', 'submitted');

-- +goose Down
DROP INDEX payouts_in_flight_idx;
