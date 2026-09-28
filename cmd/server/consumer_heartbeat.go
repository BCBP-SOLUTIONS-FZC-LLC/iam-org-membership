package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// ConsumerHeartbeat is a readiness probe for a single SQS consumer goroutine.
// It satisfies httpadapter.Pinger (Health(ctx) error) and is passed to
// RouterConfig.Consumers so /readyz reports a 503 if any consumer goroutine
// has exited (panicked or returned a terminal error).
//
// Usage in main.go:
//
//	hb := newConsumerHeartbeat("tenant-orgm-q")
//	go func() {
//	    defer hb.MarkDead()
//	    // ... cons.Start(ctx) ...
//	}()
//
// The zero value is not useful; always construct via newConsumerHeartbeat.
type ConsumerHeartbeat struct {
	queue string
	alive atomic.Bool
	// startedAt is set once at construction so that Health can report a
	// meaningful age for a dead-on-arrival consumer.
	startedAt time.Time
}

// newConsumerHeartbeat returns a heartbeat already marked alive. Call
// MarkDead() in the goroutine's deferred cleanup so /readyz detects the exit.
func newConsumerHeartbeat(queue string) *ConsumerHeartbeat {
	h := &ConsumerHeartbeat{queue: queue, startedAt: time.Now()}
	h.alive.Store(true)
	return h
}

// MarkDead signals that the consumer goroutine has exited. It is idempotent.
func (h *ConsumerHeartbeat) MarkDead() { h.alive.Store(false) }

// Health implements httpadapter.Pinger. It returns a non-nil error when the
// consumer goroutine has exited, which causes /readyz to return 503 and remove
// the pod from rotation (OBS-1). The check is purely in-process (atomic load)
// and never blocks, so ctx is accepted for interface compliance only.
func (h *ConsumerHeartbeat) Health(_ context.Context) error {
	if !h.alive.Load() {
		return fmt.Errorf("consumer %q exited after %s",
			h.queue, time.Since(h.startedAt).Round(time.Second))
	}
	return nil
}
