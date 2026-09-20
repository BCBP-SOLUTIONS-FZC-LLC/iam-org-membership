package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLogger is a minimal port.Logger test double recording Warn calls —
// tick's only observable side effect on emit failure.
type fakeLogger struct {
	warnCalls int32
	mu        sync.Mutex
	lastWarn  map[string]any
}

func (f *fakeLogger) Debug(string, map[string]any) {}
func (f *fakeLogger) Info(string, map[string]any)  {}
func (f *fakeLogger) Warn(msg string, fields map[string]any) {
	f.mu.Lock()
	f.lastWarn = fields
	f.mu.Unlock()
	atomic.AddInt32(&f.warnCalls, 1)
}
func (f *fakeLogger) Error(string, map[string]any) {}
func (f *fakeLogger) getLastWarn() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastWarn
}

func TestTick_EmitsOnceBeforeFirstTickerFire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int32
	done := make(chan struct{})
	go func() {
		tick(ctx, "test_gauge", &fakeLogger{}, func() error {
			atomic.AddInt32(&calls, 1)
			return nil
		})
		close(done)
	}()

	require.Eventually(t, func() bool { return atomic.LoadInt32(&calls) >= 1 }, time.Second, time.Millisecond,
		"emit must run once immediately, without waiting for the first ticker interval")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tick did not return after ctx was canceled")
	}
}

func TestTick_ReturnsPromptlyOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tick(ctx, "test_gauge", &fakeLogger{}, func() error { return nil })
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tick blocked well past context cancellation — it must return on <-ctx.Done(), not wait for the next ticker fire")
	}
}

func TestTick_QueryFailureLogsWarnAndKeepsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &fakeLogger{}
	done := make(chan struct{})

	go func() {
		tick(ctx, "test_gauge", log, func() error { return errors.New("boom") })
		close(done)
	}()

	require.Eventually(t, func() bool { return atomic.LoadInt32(&log.warnCalls) >= 1 }, time.Second, time.Millisecond)
	lastWarn := log.getLastWarn()
	assert.Equal(t, "test_gauge", lastWarn["gauge"])
	assert.Equal(t, "boom", lastWarn["error"])

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tick did not return after ctx was canceled, even after an emit failure")
	}
}
