// Package safego contains panics on background goroutines. HTTP handlers are
// covered by middleware.Recovery, but a panic on any other goroutine ends the
// whole process — and with it every tenant's requests and in-flight runs.
// It can't help with fatal runtime errors (out of memory, concurrent map
// writes), which aren't panics.
package safego

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

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
			err = &PanicError{Name: name, Value: r}
		}
	}()
	return fn()
}

// Go runs fn on a new goroutine with a panic contained.
func Go(name string, fn func()) {
	go func() { _ = Do(name, fn) }()
}
