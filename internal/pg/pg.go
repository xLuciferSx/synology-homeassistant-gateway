// Package pg connects to PostgreSQL and applies each service's migrations.
// Every microservice owns one schema (auth, hubs, ...) in the shared server.
package pg

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 5 // small: free/basic database tiers have few connections
	cfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return pool, nil
}

// Migrate applies files named NNN_description.sql from dir, in order, once.
// Applied versions are kept in <schema>.schema_migrations; an advisory lock
// stops two replicas migrating at the same time.
func Migrate(ctx context.Context, pool *pgxpool.Pool, schema string, files fs.FS) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	lock := "select pg_advisory_lock(hashtext($1))"
	if _, err := conn.Exec(ctx, lock, "migrate:"+schema); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), "select pg_advisory_unlock(hashtext($1))", "migrate:"+schema)

	if _, err := conn.Exec(ctx, fmt.Sprintf(`create schema if not exists %[1]s;
		create table if not exists %[1]s.schema_migrations (version int primary key, applied_at timestamptz not null default now())`, schema)); err != nil {
		return err
	}
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", name)
		}
		var done bool
		if err := conn.QueryRow(ctx, fmt.Sprintf("select exists(select 1 from %s.schema_migrations where version=$1)", schema), v).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("insert into %s.schema_migrations(version) values($1)", schema), v); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
