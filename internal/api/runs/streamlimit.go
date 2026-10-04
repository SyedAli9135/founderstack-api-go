package runs

import "sync"

// Each open stream holds a goroutine and a subscription for as long as its
// run lives — hours, for one waiting on an approval — so the number a single
// user or workspace can hold open is bounded. Counted per process.
var (
	maxStreamsPerUser = 10
	maxStreamsPerOrg  = 50
)

type streamLimiter struct {
	mu      sync.Mutex
	perUser map[string]int
	perOrg  map[string]int
}

func newStreamLimiter() *streamLimiter {
	return &streamLimiter{perUser: map[string]int{}, perOrg: map[string]int{}}
}

// acquire takes a slot for (user, org), or reports that one of the limits is reached.
func (l *streamLimiter) acquire(user, org string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perUser[user] >= maxStreamsPerUser || l.perOrg[org] >= maxStreamsPerOrg {
		return false
	}
	l.perUser[user]++
	l.perOrg[org]++
	return true
}

func (l *streamLimiter) release(user, org string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perUser[user]--; l.perUser[user] <= 0 {
		delete(l.perUser, user)
	}
	if l.perOrg[org]--; l.perOrg[org] <= 0 {
		delete(l.perOrg, org)
	}
}
