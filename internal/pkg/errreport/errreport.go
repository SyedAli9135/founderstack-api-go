// Package errreport sends unexpected server errors to Sentry. It does nothing
// until SENTRY_DSN is set, and never attaches request data: bodies, headers,
// cookies and query strings (which can carry tokens or customer content) are
// not collected, only the error, its stack, and a few safe tags.
package errreport

import (
	"fmt"
	"time"

	"github.com/getsentry/sentry-go"
)

// Init enables reporting when dsn is non-empty and returns a flush func to
// call at shutdown. An empty dsn leaves reporting off.
func Init(dsn, environment, release string) (flush func(), err error) {
	if dsn == "" {
		return func() {}, nil
	}
	err = sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Environment:      environment,
		Release:          release,
		AttachStacktrace: true,
		TracesSampleRate: 0,
		BeforeSend: func(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			e.Request = nil
			e.User = sentry.User{}
			return e
		},
	})
	if err != nil {
		return func() {}, fmt.Errorf("errreport: init sentry: %w", err)
	}
	return func() { sentry.Flush(3 * time.Second) }, nil
}

// Panic reports a recovered panic. where names the code path (a job, "http").
func Panic(where string, value any, tags map[string]string) {
	capture(where, tags, func() { sentry.CurrentHub().Recover(value) })
}

// Message reports a failure that has no Go error (e.g. a handler's 5xx).
func Message(where, msg string, tags map[string]string) {
	capture(where, tags, func() { sentry.CaptureMessage(msg) })
}

func capture(where string, tags map[string]string, send func()) {
	if sentry.CurrentHub().Client() == nil {
		return
	}
	sentry.WithScope(func(s *sentry.Scope) {
		s.SetTag("where", where)
		for k, v := range tags {
			s.SetTag(k, v)
		}
		send()
	})
}
