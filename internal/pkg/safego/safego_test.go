package safego

import (
	"errors"
	"sync"
	"testing"
)

func TestDo_ReturnsAPanicAsAnError(t *testing.T) {
	err := Do("job", func() { panic("boom") })
	var pe *PanicError
	if !errors.As(err, &pe) || pe.Name != "job" || pe.Value != "boom" {
		t.Fatalf("err = %v, want a *PanicError for job/boom", err)
	}
	if err := Do("job", func() {}); err != nil {
		t.Fatalf("a clean run returned %v", err)
	}
}

func TestDoErr_PassesErrorsThroughAndContainsPanics(t *testing.T) {
	want := errors.New("plain failure")
	if got := DoErr("job", func() error { return want }); got != want {
		t.Fatalf("got %v, want the original error", got)
	}
	var pe *PanicError
	if err := DoErr("job", func() error { var m map[string]int; m["x"] = 1; return nil }); !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a *PanicError for a nil-map write", err)
	}
}

func TestGo_AGoroutinePanicDoesNotEndTheProcess(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	Go("job", func() {
		defer wg.Done()
		panic("boom")
	})
	wg.Wait() // reaching here at all is the assertion: an unrecovered panic would have killed the test binary
}
