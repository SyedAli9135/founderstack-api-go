// Package safego contains panics on background goroutines. HTTP handlers are
// covered by middleware.Recovery, but a panic on any other goroutine ends the
// whole process — and with it every tenant's requests and in-flight runs.
// It can't help with fatal runtime errors (out of memory, concurrent map
// writes), which aren't panics.
package safego

import (
	"context"
	"fmt"
	"github.com/founderstack/api/internal/pkg/errreport"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
	"time"
)

// inflight counts goroutines started by Go that haven't finished, so shutdown
// can wait for them (runs, document jobs, notifications) instead of cutting
// them off mid-work.
var inflight atomic.Int64

// PanicError is what a contained panic turns into.
type PanicError struct {
	Name  string
	Value any
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic in %s: %v", e.Name, e.Value) }

// Do runs fn and returns a *PanicError, after logging the stack, if it panics.
func Do(name string, fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("recovered panic", "where", name, "panic", r, "stack", string(debug.Stack()))
			errreport.Panic(name, r, nil)
			err = &PanicError{Name: name, Value: r}
		}
	}()
	fn()
	return nil
}

// DoErr is Do for a function that already returns an error (errgroup bodies).
func DoErr(name string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("recovered panic", "where", name, "panic", r, "stack", string(debug.Stack()))
			errreport.Panic(name, r, nil)
			err = &PanicError{Name: name, Value: r}
		}
	}()
	return fn()
}

// Go runs fn on a new goroutine with a panic contained.
func Go(name string, fn func()) {
	inflight.Add(1)
	go func() {
		defer inflight.Add(-1)
		_ = Do(name, fn)
	}()
}

// InFlight is how many Go goroutines are still running.
func InFlight() int { return int(inflight.Load()) }

// Wait blocks until every goroutine started by Go has finished, or ctx ends
// (returning its error). The always-running ones — job loops and listeners that
// end when the process context is cancelled — finish promptly once it is.
func Wait(ctx context.Context) error {
	for inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}
