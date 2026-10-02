package sqspipeline

import (
	"sync"
	"sync/atomic"
)

// fakeLogger is a port.Logger that records Warn calls.
type fakeLogger struct {
	warnCalls int32
	mu        sync.Mutex
	lastWarn  map[string]any
}

func (f *fakeLogger) Debug(string, map[string]any) {}
func (f *fakeLogger) Info(string, map[string]any)  {}
func (f *fakeLogger) Warn(_ string, fields map[string]any) {
	f.mu.Lock()
	f.lastWarn = fields
	f.mu.Unlock()
	atomic.AddInt32(&f.warnCalls, 1)
}
func (f *fakeLogger) Error(string, map[string]any) {}
