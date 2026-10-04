package graph

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/founderstack/api/internal/pkg/safego"
)

const (
	eventChannelPrefix = "founderstack:run-events:"
	cancelChannel      = "founderstack:run-cancel"
	relayQueueSize     = 1024
)

// relay carries bus events between API instances over Redis pub/sub. It is
// best effort by design: a live stream that misses an event is a cosmetic
// glitch (the run's real state is in Postgres), so nothing here may slow a run
// down or fail it.
type relay struct {
	rdb      *redis.Client
	id       string // this instance, so it can ignore its own echoes
	out      chan relayMessage
	lastWarn atomic.Int64
}

type relayMessage struct {
	channel string
	payload []byte
}

type relayEnvelope struct {
	Instance string `json:"i"`
	Event    Event  `json:"e"`
}

// EnableRedis turns on cross-instance relaying. Call once at startup, before
// the bus is used; ctx ends the publisher.
func (b *EventBus) EnableRedis(ctx context.Context, rdb *redis.Client) {
	r := &relay{rdb: rdb, id: uuid.NewString(), out: make(chan relayMessage, relayQueueSize)}
	b.relay = r
	safego.Go("graph: event relay publisher", func() { r.publishLoop(ctx) })
}

func (r *relay) publishLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-r.out:
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := r.rdb.Publish(pctx, m.channel, m.payload).Err()
			cancel()
			if err != nil {
				r.warn("event relay publish failed", err)
			}
		}
	}
}

// warn logs at most once every 30 seconds, so a Redis outage doesn't flood the log.
func (r *relay) warn(msg string, err error) {
	now := time.Now().Unix()
	if last := r.lastWarn.Load(); now-last >= 30 && r.lastWarn.CompareAndSwap(last, now) {
		slog.Warn("graph: "+msg, "error", err)
	}
}

// relayOut queues ev for other instances. It never blocks: a full queue drops the event.
func (b *EventBus) relayOut(ev Event) {
	r := b.relay
	if r == nil {
		return
	}
	payload, err := json.Marshal(relayEnvelope{Instance: r.id, Event: ev})
	if err != nil {
		return
	}
	select {
	case r.out <- relayMessage{channel: eventChannelPrefix + ev.RunID.String(), payload: payload}:
	default:
	}
}

// subscribeRemote feeds deliver with runID's events published by *other*
// instances, until the returned stop func is called (which waits for any
// in-flight delivery to finish, so the caller may close its channel after).
// Without a relay it does nothing.
func (b *EventBus) subscribeRemote(runID uuid.UUID, deliver func(Event)) (stop func()) {
	r := b.relay
	if r == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	ps := r.rdb.Subscribe(ctx, eventChannelPrefix+runID.String())
	// Wait for the subscription to be live, so events published right after
	// this returns aren't lost to the race.
	cctx, ccancel := context.WithTimeout(ctx, time.Second)
	_, _ = ps.Receive(cctx)
	ccancel()

	done := make(chan struct{})
	safego.Go("graph: event relay subscriber", func() {
		defer close(done)
		defer ps.Close()
		msgs := ps.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-msgs:
				if !ok {
					return
				}
				var env relayEnvelope
				if err := json.Unmarshal([]byte(m.Payload), &env); err != nil || env.Instance == r.id {
					continue
				}
				deliver(env.Event)
			}
		}
	})
	return func() {
		cancel()
		<-done
	}
}

// PublishCancel asks whichever instance is running runID to cancel it.
func (e *Engine) PublishCancel(ctx context.Context, runID uuid.UUID) bool {
	r := e.Bus.relay
	if r == nil {
		return false
	}
	return r.rdb.Publish(ctx, cancelChannel, runID.String()).Err() == nil
}

// ListenForCancels cancels, on this instance, any run another instance asked
// to cancel. Blocks until ctx ends; run it on its own goroutine.
func (e *Engine) ListenForCancels(ctx context.Context) {
	r := e.Bus.relay
	if r == nil {
		return
	}
	ps := r.rdb.Subscribe(ctx, cancelChannel)
	defer ps.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-ps.Channel():
			if !ok {
				return
			}
			if id, err := uuid.Parse(m.Payload); err == nil {
				e.Cancel(id) // a no-op on every instance that isn't running it
			}
		}
	}
}
