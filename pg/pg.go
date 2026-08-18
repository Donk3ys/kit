// Package pg builds a configured pgx connection pool and provides the
// transaction helpers that make rollback failures visible rather than silent.
//
// It is named for PostgreSQL, not for databases in general, because that is
// what it is: every exported symbol here either takes or returns a pgx type,
// and StatementTimeout, LockTimeout and IdleInTransactionSessionTimeout are
// PostgreSQL's own parameter names. A second database would be a sibling
// package with its own vocabulary, not another implementation behind a shared
// interface — Mongo has no savepoints, so Beginner and InTx could not mean the
// same thing there, and an interface spanning both would be the intersection
// of two databases, useful to neither.
package pg

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Donk3ys/kit/apperr"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config describes a pool. Only DSN is required; every duration left at zero
// keeps the driver or server default rather than having this package invent
// one.
type Config struct {
	// DSN is the PostgreSQL connection string.
	DSN string

	// ApplicationName appears in pg_stat_activity and in server logs. Setting
	// it is what makes "which service is running this query?" answerable
	// during an incident.
	ApplicationName string

	MaxConns int32
	MinConns int32
	// MaxConnLifetime bounds how long a connection is reused. A non-zero value
	// lets a pool drain onto a failed-over primary instead of holding
	// connections to the old one indefinitely.
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	// ConnectTimeout bounds establishing a single connection. Defaults to
	// DefaultConnectTimeout, because pgx's own default is no timeout at all
	// and a black-holed network then hangs startup forever.
	ConnectTimeout time.Duration

	// StatementTimeout caps any single statement, server-side.
	//
	// Strongly recommended — without it one runaway query pins a connection
	// until someone notices — but deliberately not defaulted, because the
	// right value is a product decision and a guess here would silently kill
	// a legitimate long-running report. 30s suits an interactive API.
	StatementTimeout time.Duration
	// LockTimeout caps how long a statement waits for a lock. Recommended:
	// noticeably shorter than StatementTimeout, so lock contention is
	// distinguishable from slow work.
	LockTimeout time.Duration
	// IdleInTransactionSessionTimeout kills sessions holding a transaction
	// open without doing anything. Recommended: a leaked transaction blocks
	// vacuum and can hold locks indefinitely.
	IdleInTransactionSessionTimeout time.Duration

	// QueryTracer, when set, is invoked around every query the pool runs.
	//
	// This package does not implement one: pgx already defines the interface,
	// and github.com/exaring/otelpgx implements it against OTel semantic
	// conventions, so writing another here would be rebuilding what the
	// ecosystem ships. The field exists because without it there is no way to
	// reach ConnConfig.Tracer through this constructor at all.
	//
	//	pool, err := pg.NewPool(ctx, pg.Config{
	//	        DSN:         cfg.DatabaseURL,
	//	        QueryTracer: otelpgx.NewTracer(),
	//	})
	//
	// Per-query spans are noisy at high volume; sample them, or instrument
	// units of work by hand instead.
	QueryTracer pgx.QueryTracer
}

// DefaultConnectTimeout applies when Config.ConnectTimeout is zero.
const DefaultConnectTimeout = 5 * time.Second

// PoolConfig translates a Config into a pgxpool.Config. It is exported so the
// translation can be tested, and inspected, without a database.
func PoolConfig(cfg Config) (*pgxpool.Config, error) {
	if cfg.DSN == "" {
		return nil, apperr.NewInternal("DATABASE_CONFIG_INVALID",
			"An unexpected error occurred.", "parse_dsn",
			fmt.Errorf("database DSN is empty"))
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		// The DSN carries the password; never let the driver's message, which
		// may quote it back, reach anything but a wrapped cause.
		return nil, apperr.NewInternal("DATABASE_CONFIG_INVALID",
			"An unexpected error occurred.", "parse_dsn", err)
	}

	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	if cfg.QueryTracer != nil {
		poolCfg.ConnConfig.Tracer = cfg.QueryTracer
	}

	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = DefaultConnectTimeout
	}
	poolCfg.ConnConfig.ConnectTimeout = connectTimeout

	// Server-side settings, applied per connection at startup. Setting them
	// here rather than in the DSN keeps them visible in code and means a
	// deployment cannot drop them by rewriting a connection string.
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if cfg.ApplicationName != "" {
		poolCfg.ConnConfig.RuntimeParams["application_name"] = cfg.ApplicationName
	}
	setMillis(poolCfg.ConnConfig.RuntimeParams, "statement_timeout", cfg.StatementTimeout)
	setMillis(poolCfg.ConnConfig.RuntimeParams, "lock_timeout", cfg.LockTimeout)
	setMillis(poolCfg.ConnConfig.RuntimeParams,
		"idle_in_transaction_session_timeout", cfg.IdleInTransactionSessionTimeout)

	return poolCfg, nil
}

// NewPool builds a pool and verifies it can reach the database.
//
// pgxpool connects lazily, so without the ping a bad DSN or an unreachable
// server is discovered by the first user request rather than at startup. The
// caller owns the returned pool and must Close it.
func NewPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	poolCfg, err := PoolConfig(cfg)
	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, apperr.NewExternal("DATABASE_UNAVAILABLE",
			"The service is temporarily unavailable.", "postgres", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, poolCfg.ConnConfig.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, apperr.NewExternal("DATABASE_UNAVAILABLE",
			"The service is temporarily unavailable.", "postgres", err)
	}

	return pool, nil
}

// setMillis writes a PostgreSQL duration parameter, which is expressed in
// milliseconds as a bare string.
func setMillis(params map[string]string, name string, d time.Duration) {
	if d <= 0 {
		return
	}
	params[name] = strconv.FormatInt(d.Milliseconds(), 10)
}
