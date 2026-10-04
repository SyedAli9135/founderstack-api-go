// Package db holds connection-pool setup shared by the API and its tools.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool opens a pool capped at maxConns. pgxpool's default is max(4, CPUs),
// which is both too small to serve a busy instance and unrelated to what the
// database can take — the cap has to be chosen against the server's own limit
// (all instances' pools summed must stay under max_connections).
func NewPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	cfg.HealthCheckPeriod = time.Minute
	return pgxpool.NewWithConfig(ctx, cfg)
}

// knownDevLogins are the role passwords the migrations create for local
// development. A deployment must replace them (ALTER ROLE ... PASSWORD ...).
var knownDevLogins = []struct{ User, Password string }{
	{"app_user", "app_password"},
	{"app_system", "app_system_password"},
}

// ProbeDevCredentials tries each local-development role password against the
// server dsn points at and returns the roles that still accept it. Production
// calls it at boot and refuses to start if any does: those passwords are in
// the repository, so a database that accepts them is open to anyone who can
// reach it.
func ProbeDevCredentials(ctx context.Context, dsn string) ([]string, error) {
	base, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	var open []string
	for _, l := range knownDevLogins {
		cfg := base.Copy()
		cfg.User, cfg.Password = l.User, l.Password
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := pgx.ConnectConfig(probeCtx, cfg)
		cancel()
		if err == nil {
			_ = conn.Close(ctx)
			open = append(open, l.User)
		}
	}
	return open, nil
}

// RefuseDevCredentials is ProbeDevCredentials as a boot check.
func RefuseDevCredentials(ctx context.Context, dsn string) error {
	open, err := ProbeDevCredentials(ctx, dsn)
	if err != nil {
		return fmt.Errorf("db: probe for local-dev credentials: %w", err)
	}
	if len(open) > 0 {
		return fmt.Errorf("db: roles %v still accept their local-dev passwords from the migrations — run ALTER ROLE <role> PASSWORD '<secret>' before starting in production", open)
	}
	return nil
}
