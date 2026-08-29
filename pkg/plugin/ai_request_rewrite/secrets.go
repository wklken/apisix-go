package ai_request_rewrite

import (
	"context"
	"maps"
	"sort"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
)

type rewriteSecretState struct {
	preparationMu     sync.Mutex
	preparationCond   *sync.Cond
	preparationActive bool

	credentialMu sync.Mutex
	stopOnce     sync.Once
	prepared     bool
	retired      bool
	activeUses   int
	usesDone     chan struct{}

	headerNames []string
	headers     map[string]secret.Value
	queryNames  []string
	queries     map[string]secret.Value
	gcp         secret.Value
	hasGCP      bool
	awsSecret   secret.Value
	hasAWS      bool
	awsSession  secret.Value
	hasSession  bool
}

type stagedRewriteSecrets struct {
	headerNames       []string
	headers           map[string]secret.Value
	headerDescriptors map[string]string
	queryNames        []string
	queries           map[string]secret.Value
	queryDescriptors  map[string]string
	gcp               secret.Value
	gcpDescriptor     string
	hasGCP            bool
	awsSecret         secret.Value
	awsDescriptor     string
	hasAWS            bool
	awsSession        secret.Value
	sessionDescriptor string
	hasSession        bool
}

type rewriteSecretSnapshot struct {
	headerNames []string
	headers     map[string]secret.Value
	queryNames  []string
	queries     map[string]secret.Value
	gcp         secret.Value
	hasGCP      bool
	awsSecret   secret.Value
	hasAWS      bool
	awsSession  secret.Value
	hasSession  bool
}

func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context,
	access base.ScopedSecretAccess,
) error {
	p.beginSecretPreparation()
	defer p.endSecretPreparation()
	if prepared, err := p.secretPreparationState(); err != nil || prepared {
		return err
	}

	staged := stagedRewriteSecrets{
		headerNames:       sortedRewriteAuthNames(p.config.Auth.Header),
		headers:           make(map[string]secret.Value, len(p.config.Auth.Header)),
		headerDescriptors: make(map[string]string, len(p.config.Auth.Header)),
		queryNames:        sortedRewriteAuthNames(p.config.Auth.Query),
		queries:           make(map[string]secret.Value, len(p.config.Auth.Query)),
		queryDescriptors:  make(map[string]string, len(p.config.Auth.Query)),
	}
	var err error
	for _, name := range staged.headerNames {
		staged.headers[name], err = access.Materialize(ctx, "auth.header", p.config.Auth.Header[name])
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		staged.headerDescriptors[name], err = rewriteSecretDescriptor(staged.headers[name])
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
	}
	for _, name := range staged.queryNames {
		staged.queries[name], err = access.Materialize(ctx, "auth.query", p.config.Auth.Query[name])
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		staged.queryDescriptors[name], err = rewriteSecretDescriptor(staged.queries[name])
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
	}
	if p.config.Auth.GCP != nil && p.config.Auth.GCP.ServiceAccountJSON != "" {
		staged.gcp, err = access.Materialize(
			ctx,
			"auth.gcp.service_account_json",
			p.config.Auth.GCP.ServiceAccountJSON,
		)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		staged.gcpDescriptor, err = rewriteSecretDescriptor(staged.gcp)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		staged.hasGCP = true
	}
	if p.config.Auth.AWS != nil && p.config.Auth.AWS.SecretAccessKey != "" {
		staged.awsSecret, err = access.Materialize(
			ctx,
			"auth.aws.secret_access_key",
			p.config.Auth.AWS.SecretAccessKey,
		)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		staged.awsDescriptor, err = rewriteSecretDescriptor(staged.awsSecret)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		staged.hasAWS = true
	}
	if p.config.Auth.AWS != nil && p.config.Auth.AWS.SessionToken != "" {
		staged.awsSession, err = access.Materialize(
			ctx,
			"auth.aws.session_token",
			p.config.Auth.AWS.SessionToken,
		)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		staged.sessionDescriptor, err = rewriteSecretDescriptor(staged.awsSession)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		staged.hasSession = true
	}
	return p.installScopedSecrets(staged)
}

func sortedRewriteAuthNames(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func rewriteSecretDescriptor(value secret.Value) (string, error) {
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

func (p *Plugin) beginSecretPreparation() {
	state := &p.secrets
	state.preparationMu.Lock()
	if state.preparationCond == nil {
		state.preparationCond = sync.NewCond(&state.preparationMu)
	}
	for state.preparationActive {
		state.preparationCond.Wait()
	}
	state.preparationActive = true
	state.preparationMu.Unlock()
}

func (p *Plugin) endSecretPreparation() {
	state := &p.secrets
	state.preparationMu.Lock()
	state.preparationActive = false
	state.preparationCond.Broadcast()
	state.preparationMu.Unlock()
}

func (p *Plugin) secretPreparationState() (bool, error) {
	state := &p.secrets
	state.credentialMu.Lock()
	defer state.credentialMu.Unlock()
	if state.retired {
		return false, secret.ErrCredentialUnavailable
	}
	return state.prepared, nil
}

func (p *Plugin) installScopedSecrets(staged stagedRewriteSecrets) error {
	state := &p.secrets
	state.credentialMu.Lock()
	defer state.credentialMu.Unlock()
	if state.retired {
		return secret.ErrCredentialUnavailable
	}
	state.headerNames = staged.headerNames
	state.headers = staged.headers
	state.queryNames = staged.queryNames
	state.queries = staged.queries
	state.gcp, state.hasGCP = staged.gcp, staged.hasGCP
	state.awsSecret, state.hasAWS = staged.awsSecret, staged.hasAWS
	state.awsSession, state.hasSession = staged.awsSession, staged.hasSession
	state.prepared = true
	for _, name := range staged.headerNames {
		p.config.Auth.Header[name] = staged.headerDescriptors[name]
	}
	for _, name := range staged.queryNames {
		p.config.Auth.Query[name] = staged.queryDescriptors[name]
	}
	if staged.hasGCP {
		p.config.Auth.GCP.ServiceAccountJSON = staged.gcpDescriptor
	}
	if staged.hasAWS {
		p.config.Auth.AWS.SecretAccessKey = staged.awsDescriptor
	}
	if staged.hasSession {
		p.config.Auth.AWS.SessionToken = staged.sessionDescriptor
	}
	return nil
}

func (p *Plugin) withAuth(use func(Auth) error) error {
	if use == nil {
		return secret.ErrCredentialUnavailable
	}
	snapshot, release, prepared, err := p.acquireSecretSnapshot()
	if err != nil {
		return err
	}
	if !prepared {
		auth := cloneRewriteAuth(p.config.Auth)
		defer clearRewriteAuth(&auth)
		return use(auth)
	}
	defer release()
	auth := cloneRewriteAuthShape(p.config.Auth)
	defer clearRewriteAuth(&auth)
	entries := snapshot.entries()
	var visit func(int) error
	visit = func(index int) error {
		if index == len(entries) {
			return use(auth)
		}
		entry := entries[index]
		return entry.value.Use(func(plaintext string) error {
			entry.set(&auth, plaintext)
			defer entry.set(&auth, "")
			return visit(index + 1)
		})
	}
	return visit(0)
}

type rewriteSecretEntry struct {
	value secret.Value
	kind  uint8
	name  string
}

const (
	rewriteSecretHeader uint8 = iota
	rewriteSecretQuery
	rewriteSecretGCP
	rewriteSecretAWS
	rewriteSecretAWSSession
)

func (snapshot rewriteSecretSnapshot) entries() []rewriteSecretEntry {
	entries := make([]rewriteSecretEntry, 0, len(snapshot.headerNames)+len(snapshot.queryNames)+3)
	for _, name := range snapshot.headerNames {
		entries = append(entries, rewriteSecretEntry{
			value: snapshot.headers[name], kind: rewriteSecretHeader, name: name,
		})
	}
	for _, name := range snapshot.queryNames {
		entries = append(entries, rewriteSecretEntry{
			value: snapshot.queries[name], kind: rewriteSecretQuery, name: name,
		})
	}
	if snapshot.hasGCP {
		entries = append(entries, rewriteSecretEntry{value: snapshot.gcp, kind: rewriteSecretGCP})
	}
	if snapshot.hasAWS {
		entries = append(entries, rewriteSecretEntry{value: snapshot.awsSecret, kind: rewriteSecretAWS})
	}
	if snapshot.hasSession {
		entries = append(entries, rewriteSecretEntry{value: snapshot.awsSession, kind: rewriteSecretAWSSession})
	}
	return entries
}

func (entry rewriteSecretEntry) set(auth *Auth, value string) {
	switch entry.kind {
	case rewriteSecretHeader:
		auth.Header[entry.name] = value
	case rewriteSecretQuery:
		auth.Query[entry.name] = value
	case rewriteSecretGCP:
		auth.GCP.ServiceAccountJSON = value
	case rewriteSecretAWS:
		auth.AWS.SecretAccessKey = value
	case rewriteSecretAWSSession:
		auth.AWS.SessionToken = value
	}
}

func (p *Plugin) acquireSecretSnapshot() (rewriteSecretSnapshot, func(), bool, error) {
	state := &p.secrets
	state.credentialMu.Lock()
	defer state.credentialMu.Unlock()
	if state.retired {
		return rewriteSecretSnapshot{}, nil, false, secret.ErrCredentialUnavailable
	}
	if !state.prepared {
		return rewriteSecretSnapshot{}, nil, false, nil
	}
	if state.activeUses == 0 {
		state.usesDone = make(chan struct{})
	}
	state.activeUses++
	return rewriteSecretSnapshot{
		headerNames: append([]string(nil), state.headerNames...),
		headers:     state.headers,
		queryNames:  append([]string(nil), state.queryNames...),
		queries:     state.queries,
		gcp:         state.gcp,
		hasGCP:      state.hasGCP,
		awsSecret:   state.awsSecret,
		hasAWS:      state.hasAWS,
		awsSession:  state.awsSession,
		hasSession:  state.hasSession,
	}, p.releaseSecretUse, true, nil
}

func (p *Plugin) releaseSecretUse() {
	state := &p.secrets
	state.credentialMu.Lock()
	defer state.credentialMu.Unlock()
	state.activeUses--
	if state.activeUses == 0 {
		close(state.usesDone)
		state.usesDone = nil
	}
}

func cloneRewriteAuth(source Auth) Auth {
	auth := source
	if source.Header != nil {
		auth.Header = make(map[string]string, len(source.Header))
		maps.Copy(auth.Header, source.Header)
	}
	if source.Query != nil {
		auth.Query = make(map[string]string, len(source.Query))
		maps.Copy(auth.Query, source.Query)
	}
	if source.GCP != nil {
		gcp := *source.GCP
		auth.GCP = &gcp
	}
	if source.AWS != nil {
		aws := *source.AWS
		auth.AWS = &aws
	}
	return auth
}

func cloneRewriteAuthShape(source Auth) Auth {
	auth := cloneRewriteAuth(source)
	for name := range auth.Header {
		auth.Header[name] = ""
	}
	for name := range auth.Query {
		auth.Query[name] = ""
	}
	if auth.GCP != nil {
		auth.GCP.ServiceAccountJSON = ""
	}
	if auth.AWS != nil {
		auth.AWS.SecretAccessKey = ""
		auth.AWS.SessionToken = ""
	}
	return auth
}

func clearRewriteAuth(auth *Auth) {
	for name := range auth.Header {
		auth.Header[name] = ""
	}
	clear(auth.Header)
	for name := range auth.Query {
		auth.Query[name] = ""
	}
	clear(auth.Query)
	if auth.GCP != nil {
		*auth.GCP = ai_auth.GCPConfig{}
	}
	if auth.AWS != nil {
		*auth.AWS = ai_auth.AWSConfig{}
	}
	*auth = Auth{}
}

func (p *Plugin) Stop() {
	p.secrets.stopOnce.Do(func() {
		state := &p.secrets
		state.credentialMu.Lock()
		state.retired = true
		wait := state.usesDone
		state.credentialMu.Unlock()
		if p.client != nil {
			p.client.CloseIdleConnections()
		}
		if wait != nil {
			<-wait
		}
		state.credentialMu.Lock()
		for name := range state.headers {
			state.headers[name] = secret.Value{}
		}
		for name := range state.queries {
			state.queries[name] = secret.Value{}
		}
		state.headers = nil
		state.queries = nil
		state.headerNames = nil
		state.queryNames = nil
		state.gcp = secret.Value{}
		state.awsSecret = secret.Value{}
		state.awsSession = secret.Value{}
		state.prepared = false
		state.credentialMu.Unlock()
	})
}
