package ai_proxy_multi

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

type aiProxyMultiSecretState struct {
	preparationMu     sync.Mutex
	preparationCond   *sync.Cond
	preparationActive bool

	credentialMu sync.Mutex
	stopOnce     sync.Once
	prepared     bool
	retired      bool
	activeUses   int
	usesDone     chan struct{}
	instances    []aiProxyMultiInstanceSecrets
}

type aiProxyMultiInstanceSecrets struct {
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

type stagedAIProxyMultiInstanceSecrets struct {
	aiProxyMultiInstanceSecrets
	headerDescriptors map[string]string
	queryDescriptors  map[string]string
	gcpDescriptor     string
	awsDescriptor     string
	sessionDescriptor string
}

func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	p.beginMultiSecretPreparation()
	defer p.endMultiSecretPreparation()
	if prepared, err := p.multiSecretPreparationState(); err != nil || prepared {
		return err
	}

	staged := make([]stagedAIProxyMultiInstanceSecrets, len(p.config.Instances))
	for index := range p.config.Instances {
		instance := &p.config.Instances[index]
		item := &staged[index]
		item.headerNames = sortedMultiAuthNames(instance.Auth.Header)
		item.headers = make(map[string]secret.Value, len(instance.Auth.Header))
		item.headerDescriptors = make(map[string]string, len(instance.Auth.Header))
		item.queryNames = sortedMultiAuthNames(instance.Auth.Query)
		item.queries = make(map[string]secret.Value, len(instance.Auth.Query))
		item.queryDescriptors = make(map[string]string, len(instance.Auth.Query))
		var err error
		for _, header := range item.headerNames {
			item.headers[header], err = access.Materialize(
				ctx, "instances.*.auth.header", instance.Auth.Header[header],
			)
			if err != nil {
				return secret.ErrCredentialUnavailable
			}
			item.headerDescriptors[header], err = aiProxyMultiDescriptor(item.headers[header])
			if err != nil {
				return secret.ErrCredentialUnavailable
			}
		}
		for _, key := range item.queryNames {
			item.queries[key], err = access.Materialize(
				ctx, "instances.*.auth.query", instance.Auth.Query[key],
			)
			if err != nil {
				return secret.ErrCredentialUnavailable
			}
			item.queryDescriptors[key], err = aiProxyMultiDescriptor(item.queries[key])
			if err != nil {
				return secret.ErrCredentialUnavailable
			}
		}
		if instance.Auth.GCP != nil && instance.Auth.GCP.ServiceAccountJSON != "" {
			item.gcp, err = access.Materialize(
				ctx, "instances.*.auth.gcp.service_account_json", instance.Auth.GCP.ServiceAccountJSON,
			)
			if err != nil {
				return secret.ErrCredentialUnavailable
			}
			item.gcpDescriptor, err = aiProxyMultiDescriptor(item.gcp)
			if err != nil {
				return secret.ErrCredentialUnavailable
			}
			item.hasGCP = true
		}
		if instance.Auth.AWS != nil && instance.Auth.AWS.SecretAccessKey != "" {
			item.awsSecret, err = access.Materialize(
				ctx, "instances.*.auth.aws.secret_access_key", instance.Auth.AWS.SecretAccessKey,
			)
			if err != nil {
				return secret.ErrCredentialUnavailable
			}
			item.awsDescriptor, err = aiProxyMultiDescriptor(item.awsSecret)
			if err != nil {
				return secret.ErrCredentialUnavailable
			}
			item.hasAWS = true
		}
		if instance.Auth.AWS != nil && instance.Auth.AWS.SessionToken != "" {
			item.awsSession, err = access.Materialize(
				ctx, "instances.*.auth.aws.session_token", instance.Auth.AWS.SessionToken,
			)
			if err != nil {
				return secret.ErrCredentialUnavailable
			}
			item.sessionDescriptor, err = aiProxyMultiDescriptor(item.awsSession)
			if err != nil {
				return secret.ErrCredentialUnavailable
			}
			item.hasSession = true
		}
	}
	return p.installMultiScopedSecrets(staged)
}

func sortedMultiAuthNames(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func aiProxyMultiDescriptor(value secret.Value) (string, error) {
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

func (p *Plugin) beginMultiSecretPreparation() {
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

func (p *Plugin) endMultiSecretPreparation() {
	state := &p.secrets
	state.preparationMu.Lock()
	state.preparationActive = false
	state.preparationCond.Broadcast()
	state.preparationMu.Unlock()
}

func (p *Plugin) multiSecretPreparationState() (bool, error) {
	state := &p.secrets
	state.credentialMu.Lock()
	defer state.credentialMu.Unlock()
	if state.retired {
		return false, secret.ErrCredentialUnavailable
	}
	return state.prepared, nil
}

func (p *Plugin) installMultiScopedSecrets(staged []stagedAIProxyMultiInstanceSecrets) error {
	state := &p.secrets
	state.credentialMu.Lock()
	defer state.credentialMu.Unlock()
	if state.retired {
		return secret.ErrCredentialUnavailable
	}
	state.instances = make([]aiProxyMultiInstanceSecrets, len(staged))
	for index := range staged {
		item := &staged[index]
		state.instances[index] = item.aiProxyMultiInstanceSecrets
		for _, name := range item.headerNames {
			p.config.Instances[index].Auth.Header[name] = item.headerDescriptors[name]
		}
		for _, name := range item.queryNames {
			p.config.Instances[index].Auth.Query[name] = item.queryDescriptors[name]
		}
		if item.hasGCP {
			p.config.Instances[index].Auth.GCP.ServiceAccountJSON = item.gcpDescriptor
		}
		if item.hasAWS {
			p.config.Instances[index].Auth.AWS.SecretAccessKey = item.awsDescriptor
		}
		if item.hasSession {
			p.config.Instances[index].Auth.AWS.SessionToken = item.sessionDescriptor
		}
	}
	state.prepared = true
	return nil
}

func (p *Plugin) withInstanceAuth(index int, use func(Auth) error) error {
	if use == nil || index < 0 || index >= len(p.config.Instances) {
		return secret.ErrCredentialUnavailable
	}
	snapshot, release, prepared, err := p.acquireMultiSecretSnapshot(index)
	if err != nil {
		return err
	}
	if !prepared {
		auth := cloneMultiPlainAuth(p.config.Instances[index].Auth)
		defer clearMultiAuth(&auth)
		return use(auth)
	}
	defer release()
	auth := cloneMultiAuthShape(p.config.Instances[index].Auth)
	defer clearMultiAuth(&auth)
	entries := snapshot.entries()
	var visit func(int) error
	visit = func(position int) error {
		if position == len(entries) {
			return use(auth)
		}
		entry := entries[position]
		return entry.value.Use(func(plaintext string) error {
			entry.set(&auth, plaintext)
			defer entry.set(&auth, "")
			return visit(position + 1)
		})
	}
	return visit(0)
}

type aiProxyMultiSecretEntry struct {
	value secret.Value
	kind  uint8
	name  string
}

const (
	multiSecretHeader uint8 = iota
	multiSecretQuery
	multiSecretGCP
	multiSecretAWS
	multiSecretAWSSession
)

func (snapshot aiProxyMultiInstanceSecrets) entries() []aiProxyMultiSecretEntry {
	entries := make([]aiProxyMultiSecretEntry, 0, len(snapshot.headerNames)+len(snapshot.queryNames)+3)
	for _, name := range snapshot.headerNames {
		entries = append(entries, aiProxyMultiSecretEntry{
			value: snapshot.headers[name], kind: multiSecretHeader, name: name,
		})
	}
	for _, name := range snapshot.queryNames {
		entries = append(entries, aiProxyMultiSecretEntry{
			value: snapshot.queries[name], kind: multiSecretQuery, name: name,
		})
	}
	if snapshot.hasGCP {
		entries = append(entries, aiProxyMultiSecretEntry{value: snapshot.gcp, kind: multiSecretGCP})
	}
	if snapshot.hasAWS {
		entries = append(entries, aiProxyMultiSecretEntry{value: snapshot.awsSecret, kind: multiSecretAWS})
	}
	if snapshot.hasSession {
		entries = append(entries, aiProxyMultiSecretEntry{value: snapshot.awsSession, kind: multiSecretAWSSession})
	}
	return entries
}

func (entry aiProxyMultiSecretEntry) set(auth *Auth, value string) {
	switch entry.kind {
	case multiSecretHeader:
		auth.Header[entry.name] = value
	case multiSecretQuery:
		auth.Query[entry.name] = value
	case multiSecretGCP:
		auth.GCP.ServiceAccountJSON = value
	case multiSecretAWS:
		auth.AWS.SecretAccessKey = value
	case multiSecretAWSSession:
		auth.AWS.SessionToken = value
	}
}

func (p *Plugin) acquireMultiSecretSnapshot(
	index int,
) (aiProxyMultiInstanceSecrets, func(), bool, error) {
	state := &p.secrets
	state.credentialMu.Lock()
	defer state.credentialMu.Unlock()
	if state.retired {
		return aiProxyMultiInstanceSecrets{}, nil, false, secret.ErrCredentialUnavailable
	}
	if !state.prepared {
		return aiProxyMultiInstanceSecrets{}, nil, false, nil
	}
	if index >= len(state.instances) {
		return aiProxyMultiInstanceSecrets{}, nil, false, secret.ErrCredentialUnavailable
	}
	if state.activeUses == 0 {
		state.usesDone = make(chan struct{})
	}
	state.activeUses++
	snapshot := state.instances[index]
	snapshot.headerNames = append([]string(nil), snapshot.headerNames...)
	snapshot.queryNames = append([]string(nil), snapshot.queryNames...)
	return snapshot, p.releaseMultiSecretUse, true, nil
}

func (p *Plugin) releaseMultiSecretUse() {
	state := &p.secrets
	state.credentialMu.Lock()
	defer state.credentialMu.Unlock()
	state.activeUses--
	if state.activeUses == 0 {
		close(state.usesDone)
		state.usesDone = nil
	}
}

func cloneMultiPlainAuth(source Auth) Auth {
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

func cloneMultiAuthShape(source Auth) Auth {
	auth := cloneMultiPlainAuth(source)
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

func clearMultiAuth(auth *Auth) {
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

func (p *Plugin) stopMultiSecrets() {
	p.secrets.stopOnce.Do(func() {
		state := &p.secrets
		state.credentialMu.Lock()
		state.retired = true
		wait := state.usesDone
		state.credentialMu.Unlock()
		if wait != nil {
			<-wait
		}
		state.credentialMu.Lock()
		for index := range state.instances {
			for name := range state.instances[index].headers {
				state.instances[index].headers[name] = secret.Value{}
			}
			for name := range state.instances[index].queries {
				state.instances[index].queries[name] = secret.Value{}
			}
			state.instances[index] = aiProxyMultiInstanceSecrets{}
		}
		state.instances = nil
		state.prepared = false
		state.credentialMu.Unlock()
	})
}
