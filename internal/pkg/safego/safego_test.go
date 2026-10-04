package safego

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
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

func TestWait_ReturnsOnceTheGoroutinesFinishOrTheContextEnds(t *testing.T) {
	release := make(chan struct{})
	Go("slow", func() { <-release })
	if got := InFlight(); got < 1 {
		t.Fatalf("InFlight = %d, want at least 1", got)
	}

	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := Wait(short); err == nil {
		t.Fatal("Wait returned nil while a goroutine was still running")
	}

	close(release)
	long, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := Wait(long); err != nil {
		t.Fatalf("Wait = %v after the goroutine finished, want nil", err)
	}
	if InFlight() != 0 {
		t.Fatalf("InFlight = %d after everything finished", InFlight())
	}
}
