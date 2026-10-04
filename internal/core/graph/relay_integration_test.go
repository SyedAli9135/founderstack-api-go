//go:build integration

package graph

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func testRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skip("redis not reachable: ", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// twoInstances builds two buses sharing one Redis, standing in for two API
// processes behind a load balancer.
func twoInstances(t *testing.T) (a, b *EventBus) {
	t.Helper()
	rdb := testRedisClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a, b = NewEventBus(), NewEventBus()
	a.EnableRedis(ctx, rdb)
	b.EnableRedis(ctx, rdb)
	return a, b
}

func recvWithin(ch <-chan Event, d time.Duration) (Event, bool) {
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(d):
		return Event{}, false
	}
}

func TestEventBusRelay_ARunOnOneInstanceCanBeWatchedFromAnother(t *testing.T) {
	a, b := twoInstances(t)
	runID := uuid.New()

	onB, unsubB := b.Subscribe(runID)
	defer unsubB()
	onA, unsubA := a.Subscribe(runID)
	defer unsubA()

	a.Publish(Event{Type: EventReasoning, RunID: runID, Data: "thinking"})

	if ev, ok := recvWithin(onB, 2*time.Second); !ok || ev.Type != EventReasoning || ev.RunID != runID {
		t.Fatalf("subscriber on the other instance got (%+v, %v), want the reasoning event", ev, ok)
	}
	if ev, ok := recvWithin(onA, time.Second); !ok || ev.Type != EventReasoning {
		t.Fatalf("subscriber on the publishing instance got (%+v, %v)", ev, ok)
	}
	// Neither side should see the event twice: the publisher ignores its own echo.
	if ev, ok := recvWithin(onA, 300*time.Millisecond); ok {
		t.Fatalf("publishing instance's subscriber received a duplicate: %+v", ev)
	}
	if ev, ok := recvWithin(onB, 300*time.Millisecond); ok {
		t.Fatalf("remote subscriber received a duplicate: %+v", ev)
	}
}

func TestEventBusRelay_OnlyTheMatchingRunCrossesInstances(t *testing.T) {
	a, b := twoInstances(t)
	watched, other := uuid.New(), uuid.New()
	ch, unsub := b.Subscribe(watched)
	defer unsub()

	a.Publish(Event{Type: EventToolCall, RunID: other})
	if ev, ok := recvWithin(ch, 400*time.Millisecond); ok {
		t.Fatalf("got another run's event: %+v", ev)
	}
}

// The orchestrator and its specialist can land on different instances (the
// dispatch goes back through the load balancer). The orchestrator's viewers,
// on any instance, must still see the specialist's live events.
func TestEventBusRelay_ASpecialistOnAnotherInstanceIsMirroredToEveryViewer(t *testing.T) {
	rdb := testRedisClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	orchestrator, specialist, viewer := NewEventBus(), NewEventBus(), NewEventBus()
	for _, b := range []*EventBus{orchestrator, specialist, viewer} {
		b.EnableRedis(ctx, rdb)
	}
	parent, child := uuid.New(), uuid.New()

	onOrchestrator, u1 := orchestrator.Subscribe(parent)
	defer u1()
	onViewer, u2 := viewer.Subscribe(parent) // a browser on a third instance
	defer u2()

	orchestrator.LinkChild(child, parent, "finance")
	defer orchestrator.UnlinkChild(child)

	specialist.Publish(Event{Type: EventToolResult, RunID: child, Data: "result"}) // runs on another instance

	for name, ch := range map[string]<-chan Event{"orchestrator instance": onOrchestrator, "third instance": onViewer} {
		ev, ok := recvWithin(ch, 2*time.Second)
		if !ok {
			t.Fatalf("%s never saw the specialist's event", name)
		}
		if ev.RunID != parent || ev.SubRunID == nil || *ev.SubRunID != child || ev.AgentRole != "finance" {
			t.Fatalf("%s got %+v, want a copy mirrored onto the parent run, tagged with the child and its role", name, ev)
		}
	}
}

func TestEventBus_CloseAllEndsEveryStreamAndUnsubscribeStaysSafe(t *testing.T) {
	bus := NewEventBus()
	ch1, unsub1 := bus.Subscribe(uuid.New())
	ch2, unsub2 := bus.Subscribe(uuid.New())

	bus.CloseAll()
	for i, ch := range []<-chan Event{ch1, ch2} {
		if _, ok := recvWithin(ch, time.Second); ok {
			t.Fatalf("stream %d still open after CloseAll", i+1)
		}
	}
	// A client disconnecting afterwards must not double-close.
	unsub1()
	unsub2()
}

func TestEngine_CancelReachesARunOnAnotherInstance(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	rdb := testRedisClient(t)
	ctx, cancelCtx := context.WithCancel(context.Background())
	t.Cleanup(cancelCtx)
	orgID, agentID, runID := testWorkflowRun(t, systemPool)

	running, requester := NewEngine(appPool), NewEngine(appPool)
	running.Bus.EnableRedis(ctx, rdb)
	requester.Bus.EnableRedis(ctx, rdb)
	go running.ListenForCancels(ctx)
	time.Sleep(200 * time.Millisecond) // let the listener subscribe

	started := make(chan struct{})
	nodes := Nodes{"executor": func(ctx context.Context, s *RunState) (NodeName, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}}
	done := make(chan error, 1)
	go func() {
		done <- running.Run(context.Background(), nodes, &RunState{OrgID: orgID, AgentID: agentID, WorkflowRunID: runID}, "executor")
	}()
	<-started

	if requester.Cancel(runID) {
		t.Fatal("the requesting instance isn't running it, so a local Cancel must say no")
	}
	if !requester.PublishCancel(context.Background(), runID) {
		t.Fatal("PublishCancel failed with Redis available")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run ended with %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the run on the other instance was never cancelled")
	}
	if status, _, _ := fetchRunRow(t, systemPool, runID); status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", status)
	}
}

func TestEngine_CancelAllStopsEveryRunAndInFlightCountsThem(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	engine := NewEngine(appPool)

	const n = 3
	started := make(chan struct{}, n)
	done := make(chan error, n)
	nodes := Nodes{"executor": func(ctx context.Context, s *RunState) (NodeName, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}}
	var runIDs []uuid.UUID
	for i := 0; i < n; i++ {
		orgID, agentID, runID := testWorkflowRun(t, systemPool)
		runIDs = append(runIDs, runID)
		go func() {
			done <- engine.Run(context.Background(), nodes, &RunState{OrgID: orgID, AgentID: agentID, WorkflowRunID: runID}, "executor")
		}()
	}
	for i := 0; i < n; i++ {
		<-started
	}
	if got := engine.InFlight(); got != n {
		t.Fatalf("InFlight = %d, want %d", got, n)
	}
	if got := engine.CancelAll(); got != n {
		t.Fatalf("CancelAll cancelled %d, want %d", got, n)
	}
	for i := 0; i < n; i++ {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("run ended with %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a run outlived CancelAll")
		}
	}
	for _, id := range runIDs {
		if status, _, _ := fetchRunRow(t, systemPool, id); status != "cancelled" {
			t.Fatalf("run %s status = %q, want cancelled (not left 'running')", id, status)
		}
	}
	if engine.InFlight() != 0 {
		t.Fatalf("InFlight = %d after all runs ended, want 0", engine.InFlight())
	}
}
