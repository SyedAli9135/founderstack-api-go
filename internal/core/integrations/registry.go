package integrations

// Registry is the single lookup point every handler and the background
// refresh job uses to go from a catalog service name to its Provider —
type Registry struct {
	providers map[string]Provider
}

// NewRegistry builds a Registry from an explicit, caller-supplied list of
// providers — constructed once at startup in cmd/api/main.go, not via
// init()-based self-registration.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{providers: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		r.providers[p.Name()] = p
	}
	return r
}

// Get returns the Provider registered for service, or ok=false if
// service isn't in the registry (either an unknown service entirely, or
// a catalog entry whose provider wiring hasn't been added yet).
func (r *Registry) Get(service string) (Provider, bool) {
	p, ok := r.providers[service]
	return p, ok
}
