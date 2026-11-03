// Package pgtest starts a throwaway Postgres 16 in Docker for tests and
// applies Ferry's migrations to it. Import it only from _test.go files.
package pgtest

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver used by goose
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/darkodi/ferry/migrations"
)

// Image is the Postgres image used in tests; it matches docker-compose.yml.
const Image = "postgres:16-alpine"

// DB is a running test database.
type DB struct {
	// DSN connects as the database owner (a superuser in the container).
	// Tests that check least privilege switch to ferry_app with SET ROLE.
	DSN       string
	container *postgres.PostgresContainer
}

// Start launches Postgres and applies all migrations. Call Close when done.
func Start(ctx context.Context) (*DB, error) {
	c, err := postgres.Run(ctx, Image,
		postgres.WithDatabase("ferry"),
		postgres.WithUsername("ferry"),
		postgres.WithPassword("ferry"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres container: %w", err)
	}
	db := &DB{container: c}

	db.DSN, err = c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("connection string: %w", err)
	}
	if err := Migrate(ctx, db.DSN); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Close stops and removes the container.
func (d *DB) Close() {
	if d.container != nil {
		_ = testcontainers.TerminateContainer(d.container)
	}
}

// Migrate applies every migration in migrations.FS to the database at dsn.
func Migrate(ctx context.Context, dsn string) error {
	p, closeDB, err := Provider(dsn)
	if err != nil {
		return err
	}
	defer closeDB()
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Provider returns a goose provider for dsn and a func that closes its connection.
func Provider(dsn string) (*goose.Provider, func(), error) {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("goose provider: %w", err)
	}
	return p, func() { _ = sqlDB.Close() }, nil
}
