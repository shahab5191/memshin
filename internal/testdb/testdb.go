// Package testdb creates, lists and drops ephemeral test databases so the
// memtest harness can exercise the memory pipeline without ever touching the
// application's main tables. Each run gets a fresh database named with a
// timestamp and short hash; nothing is dropped automatically — a separate
// command removes them when the operator is done inspecting.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"

	"github.com/shahab5191/memshin/internal/db"
)

const (
	// Prefix distinguishes test databases from the application database so they
	// can be listed and dropped as a group without risk to real data.
	Prefix = "memshin_test_"

	// AdminDB is the always-present database used for CREATE/DROP DATABASE.
	AdminDB = "postgres"
)

// baseURL builds the connection URL from the environment, leaving the path
// (database name) unset. It mirrors cmd/api's buildDSN.
func baseURL() *url.URL {
	host := os.Getenv("POSTGRES_HOST")
	port := os.Getenv("POSTGRES_PORT")
	user := os.Getenv("POSTGRES_USER")
	password := os.Getenv("POSTGRES_PASSWORD")
	sslmode := os.Getenv("POSTGRES_SSLMODE")

	if host == "" || port == "" || user == "" || sslmode == "" {
		panic("database configuration is not set in environment variables or .env file")
	}

	return &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(host, port),
		RawQuery: "sslmode=" + sslmode,
	}
}

// DSNFor returns the DSN for a specific database on the same server.
func DSNFor(name string) string {
	u := baseURL()
	u.Path = name
	return u.String()
}

// adminDSN points at the admin database used for CREATE/DROP DATABASE.
func adminDSN() string {
	return DSNFor(AdminDB)
}

// adminPool connects to the admin database without the pgvector codec: the
// extension is not installed there, so db.Connect (which registers the codec on
// AfterConnect) would fail.
func adminPool(ctx context.Context) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, adminDSN())
	if err != nil {
		return nil, fmt.Errorf("open admin pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping admin database: %w", err)
	}
	return pool, nil
}

// Name returns a unique test database name: prefix + timestamp + short hash.
func Name() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s%s_%s", Prefix, time.Now().Format("20060102_150405"), hex.EncodeToString(b))
}

// Create makes a fresh test database, migrates it, and records the meta table.
// The returned pool is fully wired (pgvector codec registered via db.Connect).
func Create(ctx context.Context, name string) (*pgxpool.Pool, error) {
	if err := createDatabase(ctx, name); err != nil {
		return nil, err
	}

	dsn := DSNFor(name)

	// Migrate before connecting via db.Connect: the pgvector codec it registers
	// on AfterConnect needs the vector type, which migration 00004 creates.
	if err := migrate(ctx, dsn); err != nil {
		_ = dropDatabase(ctx, name)
		return nil, fmt.Errorf("migrate test database: %w", err)
	}

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		_ = dropDatabase(ctx, name)
		return nil, fmt.Errorf("connect test database: %w", err)
	}

	if err := createMetaTable(ctx, pool); err != nil {
		pool.Close()
		_ = dropDatabase(ctx, name)
		return nil, fmt.Errorf("create meta table: %w", err)
	}

	return pool, nil
}

// Drop drops a single test database, terminating any open connections first.
func Drop(ctx context.Context, name string) error {
	return dropDatabase(ctx, name)
}

// List returns the names of all test databases, newest first.
func List(ctx context.Context) ([]string, error) {
	pool, err := adminPool(ctx)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	rows, err := pool.Query(ctx,
		"SELECT datname FROM pg_database WHERE datname LIKE $1 ORDER BY datname DESC", Prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("list test databases: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan database name: %w", err)
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

func createDatabase(ctx context.Context, name string) error {
	pool, err := adminPool(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Identifiers cannot be parameterised; the name is internally generated
	// (prefix + timestamp + hex hash), so it is safe to interpolate.
	// TEMPLATE template0 sidesteps the "collation version mismatch" error that
	// template1 raises when the container's libc has drifted since init.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q TEMPLATE template0`, name)); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}

func dropDatabase(ctx context.Context, name string) error {
	pool, err := adminPool(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Terminate connections so DROP DATABASE cannot fail with "in use".
	if _, err := pool.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
		name); err != nil {
		return fmt.Errorf("terminate connections: %w", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP DATABASE %q`, name)); err != nil {
		return fmt.Errorf("drop database %s: %w", name, err)
	}
	return nil
}

// migrate applies the goose SQL migrations from the migrations directory.
func migrate(ctx context.Context, dsn string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open sql db: %w", err)
	}
	defer sqlDB.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS(migrationsDir()))
	if err != nil {
		return fmt.Errorf("new goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// migrationsDir returns the absolute path to the repository's migrations
// directory, resolved from this file's location rather than the process working
// directory, so the harness works no matter where it is invoked from.
func migrationsDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "migrations"
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "migrations")
}

// createMetaTable records which scenarios ran in a database so list/inspect can
// show what is inside without scanning the domain tables.
func createMetaTable(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS memtest_meta (
    run_id     text        NOT NULL,
    user_id    text        NOT NULL,
    title      text        NOT NULL,
    turns      int         NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, user_id)
)`)
	return err
}

// RecordScenario inserts one row describing a scenario that ran in the database.
func RecordScenario(ctx context.Context, pool *pgxpool.Pool, runID, userID, title string, turns int) error {
	_, err := pool.Exec(ctx, `
INSERT INTO memtest_meta (run_id, user_id, title, turns) VALUES ($1, $2, $3, $4)
ON CONFLICT (run_id, user_id) DO UPDATE SET title = EXCLUDED.title, turns = EXCLUDED.turns`,
		runID, userID, title, turns)
	return err
}

// Meta describes one scenario recorded in a test database.
type Meta struct {
	RunID     string
	UserID    string
	Title     string
	Turns     int
	CreatedAt time.Time
}

// ReadMeta returns the recorded scenarios for a database, or nil if the meta
// table is absent (e.g. an older test database created before meta existed).
func ReadMeta(ctx context.Context, pool *pgxpool.Pool) ([]Meta, error) {
	rows, err := pool.Query(ctx,
		`SELECT run_id, user_id, title, turns, created_at FROM memtest_meta ORDER BY user_id`)
	if err != nil {
		return nil, fmt.Errorf("read meta: %w", err)
	}
	defer rows.Close()

	var out []Meta
	for rows.Next() {
		var m Meta
		if err := rows.Scan(&m.RunID, &m.UserID, &m.Title, &m.Turns, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan meta: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
