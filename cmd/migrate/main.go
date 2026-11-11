// Command migrate applies Ferry's database migrations (the SQL files in
// migrations/, embedded in the binary) and exits. It runs as a one-off
// task before a deploy (an ECS run-task on AWS), as the schema owner:
// the application role ferry_app cannot change the schema.
//
//	DATABASE_URL  Postgres connection string of the schema owner (required);
//	              missing parts (host, user, password) come from PGHOST,
//	              PGUSER, PGPASSWORD, as libpq does
//	FERRY_APP_DB_PASSWORD  password for -app-login (letters and digits, 32+)
//
//	migrate                       apply every pending migration
//	migrate -status               list migrations and whether each is applied
//	migrate -app-login ferry_svc  also create the services' login role (a
//	                              member of ferry_app) or reset its password
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver used by goose
	"github.com/pressly/goose/v3"

	"github.com/darkodi/ferry/internal/platform/obs"
	"github.com/darkodi/ferry/migrations"
)

func main() {
	log := obs.NewLogger()
	if err := run(log); err != nil {
		log.Error("migrate failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	status := flag.Bool("status", false, "list migrations instead of applying them")
	appLogin := flag.String("app-login", "", "after migrating, create or update this login role as a member of ferry_app (password from FERRY_APP_DB_PASSWORD)")
	flag.Parse()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	if *status {
		ss, err := p.Status(ctx)
		if err != nil {
			return fmt.Errorf("status: %w", err)
		}
		for _, s := range ss {
			log.Info("migration", "version", s.Source.Version, "file", s.Source.Path, "state", string(s.State))
		}
		return nil
	}
	res, err := p.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	for _, r := range res {
		log.Info("applied", "version", r.Source.Version, "file", r.Source.Path, "duration", r.Duration.String())
	}
	v, err := p.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("database version: %w", err)
	}
	log.Info("database is up to date", "version", v, "applied_now", len(res))
	if *appLogin != "" {
		if err := ensureLogin(ctx, db, *appLogin, os.Getenv("FERRY_APP_DB_PASSWORD")); err != nil {
			return err
		}
		log.Info("app login ready", "role", *appLogin, "member_of", "ferry_app")
	}
	return nil
}
