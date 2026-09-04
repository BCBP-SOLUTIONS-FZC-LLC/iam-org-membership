// Metric exporter goroutines started from main.go (§11.2). Each polls
// the sysPool every 5 minutes and updates the corresponding gauge.
// Follows the sibling iam-user-profile2 pattern.
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5/pgxpool"
)

const exporterInterval = 5 * time.Minute

// runBusinessExporters starts 4 goroutines that keep the T-13, T-15,
// SEAT-5, and invitation-stale gauges fresh. The sysPool bypasses RLS
// (§4.4) since these queries are cross-tenant.
//
// Every exporter emits once at startup (populates the gauge before the
// first Prometheus scrape), then on a 5-minute ticker. Errors are logged
// at Warn — a scrape returning stale data is preferable to a panic.
func runBusinessExporters(ctx context.Context, sysPool *pgcommon.Pool, log Logger) {
	go runGaugeExporter(ctx, sysPool, log,
		"tenant_ownerless",
		`SELECT count(*) FROM tenants WHERE ownerless_since IS NOT NULL AND deleted_at IS NULL`,
		func(v int) { metrics.TenantOwnerless.Set(float64(v)) })
	go runGaugeExporter(ctx, sysPool, log,
		"realm_sync_pending",
		`SELECT count(*) FROM tenants WHERE realm_sync_pending = true AND deleted_at IS NULL`,
		func(v int) { metrics.RealmSyncPending.Set(float64(v)) })
	go runGaugeExporter(ctx, sysPool, log,
		"seat_overage_active",
		`SELECT count(*) FROM tenants WHERE overage_since IS NOT NULL AND deleted_at IS NULL`,
		func(v int) { metrics.SeatOverageActive.Set(float64(v)) })
	go runGaugeExporter(ctx, sysPool, log,
		"pending_invitations_stale",
		`SELECT count(*) FROM pending_invitations WHERE status = 'pending' AND expires_at < now()`,
		func(v int) { metrics.PendingInvitationsStale.Set(float64(v)) })
}

func runGaugeExporter(ctx context.Context, pool *pgcommon.Pool, log Logger, name, sql string, set func(int)) {
	tick(ctx, name, log, func() error {
		var n int
		err := pool.WithConn(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
			return conn.QueryRow(ctx, sql).Scan(&n)
		})
		if err != nil {
			return err
		}
		set(n)
		return nil
	})
}

// tick runs emit immediately (populating the gauge before the first
// Prometheus scrape), then again on every exporterInterval tick, until ctx
// is canceled. emit errors are logged at Warn — a scrape returning stale
// data is preferable to a panic — and never stop the loop.
func tick(ctx context.Context, name string, log Logger, emit func() error) {
	run := func() {
		if err := emit(); err != nil {
			log.Warn("gauge exporter query failed", map[string]interface{}{"gauge": name, "error": err.Error()})
		}
	}
	run()
	ticker := time.NewTicker(exporterInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// Logger is the narrow subset of the gincommon zap logger this file uses;
// declared here so we don't take a dependency on the full type.
type Logger interface {
	Warn(msg string, fields map[string]interface{})
	Info(msg string, fields map[string]interface{})
	Error(msg string, fields map[string]interface{})
}

// silence unused-import warning under -tags=integration
var _ = slog.Default
