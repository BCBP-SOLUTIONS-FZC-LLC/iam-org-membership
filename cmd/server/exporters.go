// Metric exporter goroutines started from main.go (§11.2). Each polls the
// BYPASSRLS sysPool on an interval and refreshes one DB-state gauge.
// Follows the sibling iam-realm-provisioner cmd/server/exporters.go pattern:
// exporters run as goroutines in the server pod, not as CronJobs, so the
// pod that serves /metrics is also the one that populates them. The SQL
// itself lives in the postgres adapter (pgadapter.GaugeRepository) — no
// SQL and no pgxpool.WithConn in this file.
package main

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
)

const exporterInterval = 5 * time.Minute

// runBusinessExporters starts 4 goroutines that keep the T-13, T-15,
// SEAT-5, and invitation-stale gauges fresh. gauges MUST be constructed
// over the BYPASSRLS sysPool (§4.4) since these queries are cross-tenant.
//
// Every exporter emits once at startup (populates the gauge before the
// first Prometheus scrape), then on a 5-minute ticker. Errors are logged
// at Warn — a scrape returning stale data is preferable to a panic.
func runBusinessExporters(ctx context.Context, gauges *pgadapter.GaugeRepository, log port.Logger) {
	go tick(ctx, "tenant_ownerless", log, func() error {
		n, err := gauges.CountOwnerlessTenants(ctx)
		if err != nil {
			return err
		}
		metrics.TenantOwnerless.Set(float64(n))
		return nil
	})
	go tick(ctx, "realm_sync_pending", log, func() error {
		n, err := gauges.CountRealmSyncPending(ctx)
		if err != nil {
			return err
		}
		metrics.RealmSyncPending.Set(float64(n))
		return nil
	})
	go tick(ctx, "seat_overage_active", log, func() error {
		n, err := gauges.CountSeatOverageActive(ctx)
		if err != nil {
			return err
		}
		metrics.SeatOverageActive.Set(float64(n))
		return nil
	})
	go tick(ctx, "pending_invitations_stale", log, func() error {
		n, err := gauges.CountPendingInvitationsStale(ctx)
		if err != nil {
			return err
		}
		metrics.PendingInvitationsStale.Set(float64(n))
		return nil
	})
	// iam_rls_violations_total was registered but never incremented in
	// production — this closes that gap, mirroring iam-user-profile's
	// runRLSViolationExporter. Counter is monotone (never decreases), so
	// alerts use rate()/increase() over the same window and stay stable
	// across pod restarts; rls_violation_log is the source of truth, not
	// this counter's in-memory value.
	go tick(ctx, "rls_violations", log, func() error {
		counts, err := gauges.RLSViolationCounts(ctx, exporterInterval)
		if err != nil {
			return err
		}
		for vType, n := range counts {
			if n > 0 {
				metrics.RLSViolations.WithLabelValues(vType).Add(float64(n))
			}
		}
		return nil
	})
}

// tick runs emit immediately (populating the gauge before the first
// Prometheus scrape), then again on every exporterInterval tick, until ctx
// is canceled. emit errors are logged at Warn — a scrape returning stale
// data is preferable to a panic — and never stop the loop.
func tick(ctx context.Context, name string, log port.Logger, emit func() error) {
	run := func() {
		if err := emit(); err != nil {
			log.Warn("gauge exporter query failed", map[string]any{"gauge": name, "error": err.Error()})
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
