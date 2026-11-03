-- Least-privilege role for the Ferry binaries.
--
-- The role is created NOLOGIN: no password lives in the repo. A deployment
-- grants it to a login user, e.g.
--   CREATE ROLE ferry_api LOGIN PASSWORD '...' IN ROLE ferry_app;
-- Migrations run as the schema owner, never as ferry_app.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ferry_app') THEN
        CREATE ROLE ferry_app NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO ferry_app;

-- Entries and postings: read and append only.
GRANT SELECT, INSERT ON journal_entries, postings TO ferry_app;

-- Accounts: open new ones, and change only the balance. The column-level
-- UPDATE grant is also what allows SELECT ... FOR UPDATE row locks.
GRANT SELECT, INSERT ON accounts TO ferry_app;
GRANT UPDATE (posted, updated_at) ON accounts TO ferry_app;

-- +goose Down
REVOKE ALL ON accounts, journal_entries, postings FROM ferry_app;
REVOKE USAGE ON SCHEMA public FROM ferry_app;
