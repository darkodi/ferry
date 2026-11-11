-- Reconciliation: runs, breaks and matched pairs.
-- See "Reconciliation" in docs/design.md.

-- +goose Up

-- One row per reconciliation run.
CREATE TABLE recon_runs (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    started_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz,
    window_from  timestamptz NOT NULL,
    window_to    timestamptz NOT NULL,
    checks       text[]      NOT NULL,  -- checks that completed
    failed       text[]      NOT NULL DEFAULT '{}',  -- checks that could not run (e.g. rail unreachable)
    breaks_seen  integer     NOT NULL DEFAULT 0,
    CONSTRAINT recon_runs_window_valid CHECK (window_from < window_to)
);

-- A disagreement between Ferry and an outside source (chain, rail) or
-- inside Ferry (an invariant). Identified by (check, kind, subject), so
-- each run updates the same row while the disagreement lasts:
--   open      cleared_at and resolved_at are NULL
--   resolved  a person explained it (resolved_reason); it stays resolved
--             while later runs see the same detail, and reopens if the
--             detail changes
--   cleared   a later run of the same check no longer saw it
-- Reconciliation never changes the ledger. A fix is a separate
-- correction entry whose reference names the break.
CREATE TABLE recon_breaks (
    id               uuid        PRIMARY KEY,
    check_name       text        NOT NULL,  -- internal, chain, rail
    kind             text        NOT NULL,  -- e.g. balance_mismatch, unknown_rail_tx
    subject          text        NOT NULL,  -- what it is about, e.g. an address or rail tx id
    detail           jsonb       NOT NULL,  -- expected and actual values
    first_seen       timestamptz NOT NULL DEFAULT now(),
    last_seen        timestamptz NOT NULL DEFAULT now(),
    seen_count       integer     NOT NULL DEFAULT 1,
    last_run_id      bigint      NOT NULL REFERENCES recon_runs (id),
    cleared_at       timestamptz,
    resolved_at      timestamptz,
    resolved_reason  text,

    CONSTRAINT recon_breaks_check_valid CHECK (check_name IN ('internal', 'chain', 'rail')),
    CONSTRAINT recon_breaks_kind_len CHECK (length(kind) BETWEEN 1 AND 64),
    CONSTRAINT recon_breaks_subject_len CHECK (length(subject) BETWEEN 1 AND 300),
    CONSTRAINT recon_breaks_resolution CHECK ((resolved_at IS NULL) = (resolved_reason IS NULL)),
    CONSTRAINT recon_breaks_reason_len CHECK (length(resolved_reason) BETWEEN 3 AND 2000)
);

-- At most one live (not cleared) break per disagreement.
CREATE UNIQUE INDEX recon_breaks_live_key ON recon_breaks (check_name, kind, subject) WHERE cleared_at IS NULL;
CREATE INDEX recon_breaks_open_idx ON recon_breaks (first_seen) WHERE cleared_at IS NULL AND resolved_at IS NULL;

-- Pairs that matched in a run: Ferry's record and the outside one.
CREATE TABLE recon_matches (
    run_id        bigint NOT NULL REFERENCES recon_runs (id),
    check_name    text   NOT NULL,
    ferry_ref     text   NOT NULL,
    external_ref  text   NOT NULL,
    PRIMARY KEY (run_id, check_name, external_ref)
);

GRANT SELECT, INSERT ON recon_runs, recon_breaks, recon_matches TO ferry_app;
GRANT UPDATE (finished_at, checks, failed, breaks_seen) ON recon_runs TO ferry_app;
GRANT UPDATE (detail, last_seen, seen_count, last_run_id, cleared_at, resolved_at, resolved_reason) ON recon_breaks TO ferry_app;

-- +goose Down
REVOKE ALL ON recon_matches, recon_breaks, recon_runs FROM ferry_app;
DROP TABLE recon_matches;
DROP TABLE recon_breaks;
DROP TABLE recon_runs;
