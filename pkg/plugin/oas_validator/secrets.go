package oas_validator

import (
	"context"
	"crypto/sha256"
	"maps"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
)

type oasSecretState struct {
	lifecycleMu sync.Mutex
	activeWork  int
	workDone    chan struct{}
	retired     atomic.Bool

	preparationMu     sync.Mutex
	preparationCond   *sync.Cond
	preparationActive bool

	credentialMu  sync.Mutex
	legacyInline  *store.ResolvedSecret
	legacyHeaders map[string]*store.ResolvedSecret
	scopedInline  secret.Value
	scopedHeaders map[string]secret.Value
	headerNames   []string
	legacySet     bool
	scopedSet     bool
	activeUses    int
	usesDone      chan struct{}
}

type stagedOASSecrets struct {
	legacy            bool
	legacyInline      *store.ResolvedSecret
	legacyHeaders     map[string]*store.ResolvedSecret
	scopedInline      secret.Value
	scopedHeaders     map[string]secret.Value
	headerNames       []string
	inlineDescriptor  string
	headerDescriptors map[string]string
}

type oasSecretSnapshot struct {
	legacy        bool
	legacyInline  *store.ResolvedSecret
	legacyHeaders map[string]*store.ResolvedSecret
	scopedInline  secret.Value
	scopedHeaders map[string]secret.Value
	headerNames   []string
}

// MaterializeSecrets is the transitional Store-backed preparation path.
func (p *Plugin) MaterializeSecrets() error {
	p.beginOASPreparation()
	defer p.endOASPreparation()
	if prepared, err := p.oasPreparationState(); err != nil || prepared {
		return err
	}
	staged := stagedOASSecrets{
		legacy:            true,
		legacyHeaders:     make(map[string]*store.ResolvedSecret, len(p.config.SpecURLRequestHeaders)),
		headerDescriptors: make(map[string]string, len(p.config.SpecURLRequestHeaders)),
		headerNames:       sortedOASHeaderNames(p.config.SpecURLRequestHeaders),
	}
	var err error
	if p.config.Spec != "" {
		staged.legacyInline, err = store.MaterializeSecret(p.config.Spec)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		staged.inlineDescriptor, err = legacyOASDescriptor(staged.legacyInline)
		if err != nil {
			staged.destroyLegacy()
			return secret.ErrCredentialUnavailable
		}
	}
	for _, name := range staged.headerNames {
		staged.legacyHeaders[name], err = store.MaterializeSecret(p.config.SpecURLRequestHeaders[name])
		if err != nil {
			staged.destroyLegacy()
			return secret.ErrCredentialUnavailable
		}
		staged.headerDescriptors[name], err = legacyOASDescriptor(staged.legacyHeaders[name])
		if err != nil {
			staged.destroyLegacy()
			return secret.ErrCredentialUnavailable
		}
	}
	return p.installOASSecrets(staged)
}

// MaterializeScopedSecrets resolves one attempt's optional inline document and
// every request-header value through the exact terminal-container declaration.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	p.beginOASPreparation()
	defer p.endOASPreparation()
	if prepared, err := p.oasPreparationState(); err != nil || prepared {
		return err
	}
	staged := stagedOASSecrets{
		scopedHeaders:     make(map[string]secret.Value, len(p.config.SpecURLRequestHeaders)),
		headerDescriptors: make(map[string]string, len(p.config.SpecURLRequestHeaders)),
		headerNames:       sortedOASHeaderNames(p.config.SpecURLRequestHeaders),
	}
	var err error
	if p.config.Spec != "" {
		staged.scopedInline, err = access.Materialize(ctx, "spec", p.config.Spec)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		staged.inlineDescriptor, err = scopedOASDescriptor(staged.scopedInline)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
	}
	for _, name := range staged.headerNames {
		staged.scopedHeaders[name], err = access.Materialize(
			ctx, "spec_url_request_headers", p.config.SpecURLRequestHeaders[name],
		)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		staged.headerDescriptors[name], err = scopedOASDescriptor(staged.scopedHeaders[name])
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
	}
	return p.installOASSecrets(staged)
}

func sortedOASHeaderNames(headers map[string]string) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func legacyOASDescriptor(value *store.ResolvedSecret) (string, error) {
	plaintext := value.Bytes()
	defer clear(plaintext)
	descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, sha256.Sum256(plaintext))
	if err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

func scopedOASDescriptor(value secret.Value) (string, error) {
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

func (staged *stagedOASSecrets) destroyLegacy() {
	staged.legacyInline.Destroy()
	for _, value := range staged.legacyHeaders {
		value.Destroy()
	}
}

func (p *Plugin) beginOASPreparation() {
	p.preparationMu.Lock()
	if p.preparationCond == nil {
		p.preparationCond = sync.NewCond(&p.preparationMu)
	}
	for p.preparationActive {
		p.preparationCond.Wait()
	}
	p.preparationActive = true
	p.preparationMu.Unlock()
}

func (p *Plugin) endOASPreparation() {
	p.preparationMu.Lock()
	p.preparationActive = false
	p.preparationCond.Broadcast()
	p.preparationMu.Unlock()
}

func (p *Plugin) oasPreparationState() (bool, error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired.Load() {
		return false, secret.ErrCredentialUnavailable
	}
	return p.legacySet || p.scopedSet, nil
}

func (p *Plugin) installOASSecrets(staged stagedOASSecrets) error {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired.Load() {
		staged.destroyLegacy()
		return secret.ErrCredentialUnavailable
	}
	p.legacyInline = staged.legacyInline
	p.legacyHeaders = staged.legacyHeaders
	p.scopedInline = staged.scopedInline
	p.scopedHeaders = staged.scopedHeaders
	p.headerNames = staged.headerNames
	p.legacySet = staged.legacy
	p.scopedSet = !staged.legacy
	if staged.inlineDescriptor != "" {
		p.config.Spec = staged.inlineDescriptor
	}
	if len(staged.headerNames) > 0 {
		descriptors := make(map[string]string, len(staged.headerNames))
		for _, name := range staged.headerNames {
			descriptors[name] = staged.headerDescriptors[name]
		}
		p.config.SpecURLRequestHeaders = descriptors
	}
	return nil
}

func (p *Plugin) requirePreparedOASSecrets() error {
	if p.config.Spec == "" && len(p.config.SpecURLRequestHeaders) == 0 {
		return nil
	}
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired.Load() || (!p.legacySet && !p.scopedSet) {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

func (p *Plugin) acquireOASSecrets() (oasSecretSnapshot, func(), error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired.Load() || (!p.legacySet && !p.scopedSet) {
		return oasSecretSnapshot{}, nil, secret.ErrCredentialUnavailable
	}
	if p.activeUses == 0 {
		p.usesDone = make(chan struct{})
	}
	p.activeUses++
	legacyHeaders := make(map[string]*store.ResolvedSecret, len(p.legacyHeaders))
	maps.Copy(legacyHeaders, p.legacyHeaders)
	scopedHeaders := make(map[string]secret.Value, len(p.scopedHeaders))
	maps.Copy(scopedHeaders, p.scopedHeaders)
	return oasSecretSnapshot{
		legacy: p.legacySet, legacyInline: p.legacyInline, legacyHeaders: legacyHeaders,
		scopedInline: p.scopedInline, scopedHeaders: scopedHeaders,
		headerNames: append([]string(nil), p.headerNames...),
	}, p.releaseOASSecretUse, nil
}

func (p *Plugin) releaseOASSecretUse() {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	p.activeUses--
	if p.activeUses == 0 {
		close(p.usesDone)
		p.usesDone = nil
	}
}

func (p *Plugin) withInlineSpec(use func(string) error) error {
	if p.config.Spec == "" {
		return use("")
	}
	snapshot, release, err := p.acquireOASSecrets()
	if err != nil {
		return err
	}
	defer release()
	if !snapshot.legacy {
		return snapshot.scopedInline.Use(use)
	}
	plaintext := snapshot.legacyInline.Bytes()
	defer clear(plaintext)
	if plaintext == nil {
		return secret.ErrCredentialUnavailable
	}
	return use(string(plaintext))
}

func (p *Plugin) withRequestHeaders(use func(map[string]string) error) error {
	if len(p.config.SpecURLRequestHeaders) == 0 {
		return use(map[string]string{})
	}
	snapshot, release, err := p.acquireOASSecrets()
	if err != nil {
		return err
	}
	defer release()
	headers := make(map[string]string, len(snapshot.headerNames))
	var visit func(int) error
	visit = func(index int) error {
		if index == len(snapshot.headerNames) {
			return use(headers)
		}
		name := snapshot.headerNames[index]
		if !snapshot.legacy {
			return snapshot.scopedHeaders[name].Use(func(plaintext string) error {
				headers[name] = plaintext
				defer func() { headers[name] = "" }()
				return visit(index + 1)
			})
		}
		plaintext := snapshot.legacyHeaders[name].Bytes()
		if plaintext == nil {
			return secret.ErrCredentialUnavailable
		}
		headers[name] = string(plaintext)
		clear(plaintext)
		defer func() { headers[name] = "" }()
		return visit(index + 1)
	}
	return visit(0)
}

func (p *Plugin) retireOASSecrets() <-chan struct{} {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	return p.usesDone
}

func (p *Plugin) dropOASSecrets() stagedOASSecrets {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	legacy := stagedOASSecrets{
		legacyInline: p.legacyInline, legacyHeaders: p.legacyHeaders,
	}
	p.legacyInline = nil
	p.legacyHeaders = nil
	p.scopedInline = secret.Value{}
	p.scopedHeaders = nil
	p.headerNames = nil
	p.legacySet = false
	p.scopedSet = false
	return legacy
}

func (p *Plugin) publishOASValidator(compiled *compiledSpec) bool {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired.Load() {
		return false
	}
	p.compiled.Store(compiled)
	p.compiledAt.Store(p.currentTime().UnixNano())
	return true
}

func (p *Plugin) acquireOASWork() (func(), error) {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired.Load() {
		return nil, secret.ErrCredentialUnavailable
	}
	if p.activeWork == 0 {
		p.workDone = make(chan struct{})
	}
	p.activeWork++
	return p.releaseOASWork, nil
}

func (p *Plugin) releaseOASWork() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.activeWork--
	if p.activeWork == 0 {
		close(p.workDone)
		p.workDone = nil
	}
}

func (p *Plugin) retireOASWork() <-chan struct{} {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.retired.Store(true)
	return p.workDone
}
