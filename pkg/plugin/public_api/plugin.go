package public_api

import (
	"fmt"
	"maps"
	"net/http"
	"sync"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

const (
	priority = 501
	name     = "public-api"
)

const schema = `
{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string"
    }
  }
}
`

type Config struct {
	URI string `json:"uri,omitempty"`
}

type registryKey struct {
	method string
	uri    string
}

// Registry owns the public APIs for one route generation. A registry must be
// captured by every public-api handler in that generation so route rebuilds
// cannot change an already-published handler's dispatch table.
type Registry struct {
	sync.RWMutex
	handlers      map[registryKey]http.Handler
	ownerIdentity map[string]string
}

// RegistryCheckpoint captures a registry generation's mutable public API
// state so a route build can be rolled back without affecting other routes.
type RegistryCheckpoint struct {
	handlers      map[registryKey]http.Handler
	ownerIdentity map[string]string
}

func NewRegistry() *Registry {
	return &Registry{
		handlers:      make(map[registryKey]http.Handler),
		ownerIdentity: make(map[string]string),
	}
}

// Checkpoint returns a consistent snapshot of the registry's handlers and
// owner claims. Handler values are immutable references; only the maps are
// copied so a rollback cannot retain later registrations.
func (r *Registry) Checkpoint() RegistryCheckpoint {
	if r == nil {
		return RegistryCheckpoint{}
	}
	r.RLock()
	defer r.RUnlock()
	return RegistryCheckpoint{
		handlers:      cloneHandlers(r.handlers),
		ownerIdentity: cloneOwnerIdentity(r.ownerIdentity),
	}
}

// Rollback restores a previously captured registry checkpoint.
func (r *Registry) Rollback(checkpoint RegistryCheckpoint) {
	if r == nil {
		return
	}
	r.Lock()
	defer r.Unlock()
	r.handlers = cloneHandlers(checkpoint.handlers)
	r.ownerIdentity = cloneOwnerIdentity(checkpoint.ownerIdentity)
}

func cloneHandlers(source map[registryKey]http.Handler) map[registryKey]http.Handler {
	if source == nil {
		return nil
	}
	clone := make(map[registryKey]http.Handler, len(source))
	maps.Copy(clone, source)
	return clone
}

func cloneOwnerIdentity(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	clone := make(map[string]string, len(source))
	maps.Copy(clone, source)
	return clone
}

// ClaimOwner rejects conflicting registrations from instances that own the
// same generation-level public APIs. Repeating an identical identity is safe.
func (r *Registry) ClaimOwner(owner string, identity string) error {
	if r == nil {
		return fmt.Errorf("public API registry is nil")
	}
	r.Lock()
	defer r.Unlock()
	if r.ownerIdentity == nil {
		r.ownerIdentity = make(map[string]string)
	}
	if existing, ok := r.ownerIdentity[owner]; ok && existing != identity {
		return fmt.Errorf("public API owner %q has conflicting configurations", owner)
	}
	r.ownerIdentity[owner] = identity
	return nil
}

func (r *Registry) Register(method string, uri string, handler http.Handler) {
	if r == nil {
		return
	}
	r.Lock()
	defer r.Unlock()
	if r.handlers == nil {
		r.handlers = make(map[registryKey]http.Handler)
	}
	r.handlers[registryKey{method: method, uri: uri}] = handler
}

func (r *Registry) Lookup(method string, uri string) http.Handler {
	if r == nil {
		return nil
	}
	r.RLock()
	defer r.RUnlock()
	return r.handlers[registryKey{method: method, uri: uri}]
}

type Plugin struct {
	base.BasePlugin
	config   Config
	registry *Registry
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if p.registry == nil {
		p.registry = NewRegistry()
	}
	return nil
}

// SetPublicAPIRegistry injects the registry owned by the route generation
// before PostInit registers any public endpoint.
func (p *Plugin) SetPublicAPIRegistry(registry *Registry) {
	p.registry = registry
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apisixctx.GetRequestLifecycle(r) != nil {
			base.AdaptRequestPhase(p, next).ServeHTTP(w, r)
			return
		}
		p.serve(w, r)
	})
}

// RunRequestPhase owns the local public-api gateway response. The gateway is
// an APISIX response even when a registered handler produces the body.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceAPISIX)
	p.serve(w, r)
	return base.StopRequestWithSource(r, apisixctx.ResponseSourceAPISIX)
}

func (p *Plugin) serve(w http.ResponseWriter, r *http.Request) {
	uri := p.config.URI
	if uri == "" {
		uri = r.URL.Path
	}
	handler := p.registry.Lookup(r.Method, uri)
	if handler == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	req := r.Clone(r.Context())
	req.URL.Path = uri
	req.URL.RawPath = ""
	handler.ServeHTTP(w, req)
}
