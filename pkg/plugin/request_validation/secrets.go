package request_validation

import (
	"context"
	"slices"
	"sort"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/util"
)

type requestValidationSecretState struct {
	preparationMu     sync.Mutex
	preparationCond   *sync.Cond
	preparationActive bool

	mu             sync.Mutex
	retired        bool
	prepared       bool
	headerSecrets  []schemaSecret
	bodySecrets    []schemaSecret
	activeUses     int
	usesDone       chan struct{}
	stopOnce       sync.Once
	compileOnce    sync.Once
	compileLimiter secret.GenerationLimiter
	stopping       chan struct{}
}

const requestValidationSensitiveCompileConcurrency = 4

type schemaPathStep struct {
	key     string
	index   int
	isIndex bool
}

type schemaSecret struct {
	path  []schemaPathStep
	value secret.Value
}

type stagedRequestValidationSecrets struct {
	headerSchema   map[string]any
	bodySchema     map[string]any
	headerSecrets  []schemaSecret
	bodySecrets    []schemaSecret
	compileLimiter secret.GenerationLimiter
}

type requestValidationSchemaSnapshot struct {
	headerSchema    map[string]any
	bodySchema      map[string]any
	headerSecrets   []schemaSecret
	bodySecrets     []schemaSecret
	headerCompiled  *util.CompiledSchema
	bodyCompiled    *util.CompiledSchema
	headerSensitive bool
	bodySensitive   bool
}

func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	p.beginSecretPreparation()
	defer p.endSecretPreparation()
	if prepared, err := p.secretPreparationState(); err != nil || prepared {
		return err
	}
	var compileLimiter secret.GenerationLimiter
	if schemaDocumentHasSecret(p.config.HeaderSchema) || schemaDocumentHasSecret(p.config.BodySchema) {
		var err error
		compileLimiter, err = access.SharedLimiter(
			"request-validation/sensitive-schema-compile",
			requestValidationSensitiveCompileConcurrency,
		)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
	}

	headerSchema, headerSecrets, err := materializeSchemaDocument(
		ctx, access, "header_schema", p.config.HeaderSchema,
	)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}
	bodySchema, bodySecrets, err := materializeSchemaDocument(
		ctx, access, "body_schema", p.config.BodySchema,
	)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}
	return p.installScopedSecrets(stagedRequestValidationSecrets{
		headerSchema: headerSchema, bodySchema: bodySchema,
		headerSecrets: headerSecrets, bodySecrets: bodySecrets,
		compileLimiter: compileLimiter,
	})
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
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.retired {
		return false, secret.ErrCredentialUnavailable
	}
	return state.prepared, nil
}

func (p *Plugin) installScopedSecrets(staged stagedRequestValidationSecrets) error {
	state := &p.secrets
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.retired {
		return secret.ErrCredentialUnavailable
	}
	p.config.HeaderSchema = staged.headerSchema
	p.config.BodySchema = staged.bodySchema
	state.headerSecrets = staged.headerSecrets
	state.bodySecrets = staged.bodySecrets
	state.compileLimiter = staged.compileLimiter
	state.prepared = true
	return nil
}

func materializeSchemaDocument(
	ctx context.Context,
	access base.ScopedSecretAccess,
	field string,
	document map[string]any,
) (map[string]any, []schemaSecret, error) {
	if document == nil {
		return nil, nil, nil
	}
	if err := validateRequestValidationSchemaDocument(document, true); err != nil {
		return nil, nil, err
	}
	value, entries, err := materializeSchemaValue(ctx, access, field, document, nil)
	if err != nil {
		return nil, nil, err
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, nil, secret.ErrCredentialUnavailable
	}
	return result, entries, nil
}

func materializeSchemaValue(
	ctx context.Context,
	access base.ScopedSecretAccess,
	field string,
	value any,
	path []schemaPathStep,
) (any, []schemaSecret, error) {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var entries []schemaSecret
		for _, key := range keys {
			childPath := appendSchemaPath(path, schemaPathStep{key: key})
			child, childEntries, err := materializeSchemaValue(
				ctx, access, field, typed[key], childPath,
			)
			if err != nil {
				return nil, nil, err
			}
			result[key] = child
			entries = append(entries, childEntries...)
		}
		return result, entries, nil
	case []any:
		result := make([]any, len(typed))
		var entries []schemaSecret
		for index, childValue := range typed {
			childPath := appendSchemaPath(path, schemaPathStep{index: index, isIndex: true})
			child, childEntries, err := materializeSchemaValue(
				ctx, access, field, childValue, childPath,
			)
			if err != nil {
				return nil, nil, err
			}
			result[index] = child
			entries = append(entries, childEntries...)
		}
		return result, entries, nil
	case string:
		if !capability.IsMaterializableSecretEnvelope(typed) {
			return typed, nil, nil
		}
		value, err := access.Materialize(ctx, field, typed)
		if err != nil {
			return nil, nil, err
		}
		descriptor, err := value.Descriptor(capability.SecretPluginConfig)
		if err != nil {
			return nil, nil, err
		}
		return descriptor.String(), []schemaSecret{{
			path: append([]schemaPathStep(nil), path...), value: value,
		}}, nil
	default:
		return typed, nil, nil
	}
}

func appendSchemaPath(path []schemaPathStep, step schemaPathStep) []schemaPathStep {
	result := make([]schemaPathStep, len(path)+1)
	copy(result, path)
	result[len(path)] = step
	return result
}

func schemaDocumentHasSecret(document map[string]any) bool {
	return schemaValueHasSecret(document)
}

func schemaValueHasSecret(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			if schemaValueHasSecret(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(typed, schemaValueHasSecret)
	case string:
		return capability.IsMaterializableSecretEnvelope(typed)
	}
	return false
}

func (p *Plugin) withSchemaDocuments(
	ctx context.Context,
	use func(map[string]any, map[string]any, bool, bool) error,
) error {
	if ctx == nil || use == nil {
		return secret.ErrCredentialUnavailable
	}
	snapshot, release, err := p.acquireSchemaSnapshot()
	if err != nil {
		return err
	}
	defer release()
	defer clearSchemaValue(snapshot.headerSchema)
	defer clearSchemaValue(snapshot.bodySchema)
	if len(snapshot.headerSecrets)+len(snapshot.bodySecrets) > 0 {
		releaseCompile, err := p.secrets.acquireSensitiveCompile(ctx)
		if err != nil {
			return err
		}
		defer releaseCompile()
	}

	entries := make([]schemaResolutionEntry, 0, len(snapshot.headerSecrets)+len(snapshot.bodySecrets))
	for _, item := range snapshot.headerSecrets {
		entries = append(entries, schemaResolutionEntry{document: snapshot.headerSchema, secret: item})
	}
	for _, item := range snapshot.bodySecrets {
		entries = append(entries, schemaResolutionEntry{document: snapshot.bodySchema, secret: item})
	}
	return withResolvedSchemaEntries(entries, func() error {
		return use(
			snapshot.headerSchema,
			snapshot.bodySchema,
			len(snapshot.headerSecrets) > 0,
			len(snapshot.bodySecrets) > 0,
		)
	})
}

func (state *requestValidationSecretState) initializeCompileGate() {
	state.compileOnce.Do(func() {
		state.stopping = make(chan struct{})
	})
}

func (state *requestValidationSecretState) acquireSensitiveCompile(
	ctx context.Context,
) (func(), error) {
	if ctx == nil {
		return nil, secret.ErrCredentialUnavailable
	}
	state.initializeCompileGate()
	state.mu.Lock()
	if state.retired {
		state.mu.Unlock()
		return nil, secret.ErrCredentialUnavailable
	}
	limiter := state.compileLimiter
	stopping := state.stopping
	state.mu.Unlock()
	return limiter.Acquire(ctx, stopping)
}

type schemaResolutionEntry struct {
	document map[string]any
	secret   schemaSecret
}

func withResolvedSchemaDocument(
	document map[string]any,
	secrets []schemaSecret,
	use func(map[string]any) error,
) error {
	if document == nil || len(secrets) == 0 || use == nil {
		return secret.ErrCredentialUnavailable
	}
	defer clearSchemaValue(document)
	entries := make([]schemaResolutionEntry, len(secrets))
	for index, item := range secrets {
		entries[index] = schemaResolutionEntry{document: document, secret: item}
	}
	return withResolvedSchemaEntries(entries, func() error { return use(document) })
}

func withResolvedSchemaEntries(entries []schemaResolutionEntry, use func() error) error {
	if use == nil {
		return secret.ErrCredentialUnavailable
	}
	var visit func(int) error
	visit = func(index int) error {
		if index == len(entries) {
			return use()
		}
		item := entries[index]
		if item.secret.value.Digest() == ([32]byte{}) {
			return secret.ErrCredentialUnavailable
		}
		original, ok := schemaStringAtPath(item.document, item.secret.path)
		if !ok {
			return secret.ErrCredentialUnavailable
		}
		return item.secret.value.Use(func(plaintext string) (result error) {
			if !setSchemaPath(item.document, item.secret.path, plaintext) {
				return secret.ErrCredentialUnavailable
			}
			defer func() {
				// A nested Use must not leave its plaintext in the parent callback.
				if !setSchemaPath(item.document, item.secret.path, original) {
					result = secret.ErrCredentialUnavailable
				}
			}()
			return visit(index + 1)
		})
	}
	return visit(0)
}

func (p *Plugin) acquireSchemaSnapshot() (requestValidationSchemaSnapshot, func(), error) {
	state := &p.secrets
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.retired {
		return requestValidationSchemaSnapshot{}, nil, secret.ErrCredentialUnavailable
	}
	if !state.prepared &&
		(schemaDocumentHasSecret(p.config.HeaderSchema) || schemaDocumentHasSecret(p.config.BodySchema)) {
		return requestValidationSchemaSnapshot{}, nil, secret.ErrCredentialUnavailable
	}
	if state.activeUses == 0 {
		state.usesDone = make(chan struct{})
	}
	state.activeUses++
	return requestValidationSchemaSnapshot{
		headerSchema:    cloneSchemaDocument(p.config.HeaderSchema),
		bodySchema:      cloneSchemaDocument(p.config.BodySchema),
		headerSecrets:   cloneSchemaSecrets(state.headerSecrets),
		bodySecrets:     cloneSchemaSecrets(state.bodySecrets),
		headerCompiled:  p.config.headerSchema,
		bodyCompiled:    p.config.bodySchema,
		headerSensitive: len(state.headerSecrets) > 0,
		bodySensitive:   len(state.bodySecrets) > 0,
	}, p.releaseSchemaUse, nil
}

func (p *Plugin) releaseSchemaUse() {
	state := &p.secrets
	state.mu.Lock()
	defer state.mu.Unlock()
	state.activeUses--
	if state.activeUses == 0 {
		close(state.usesDone)
		state.usesDone = nil
	}
}

func cloneSchemaDocument(document map[string]any) map[string]any {
	if document == nil {
		return nil
	}
	clone, _ := cloneSchemaValue(document).(map[string]any)
	return clone
}

func cloneSchemaValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			result[key] = cloneSchemaValue(child)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = cloneSchemaValue(child)
		}
		return result
	default:
		return typed
	}
}

func cloneSchemaSecrets(entries []schemaSecret) []schemaSecret {
	result := make([]schemaSecret, len(entries))
	for index, entry := range entries {
		result[index] = schemaSecret{
			path: append([]schemaPathStep(nil), entry.path...), value: entry.value,
		}
	}
	return result
}

func setSchemaPath(document map[string]any, path []schemaPathStep, value string) bool {
	if document == nil || len(path) == 0 {
		return false
	}
	var current any = document
	for index, step := range path {
		last := index == len(path)-1
		if step.isIndex {
			items, ok := current.([]any)
			if !ok || step.index < 0 || step.index >= len(items) {
				return false
			}
			if last {
				items[step.index] = value
				return true
			}
			current = items[step.index]
			continue
		}
		object, ok := current.(map[string]any)
		if !ok {
			return false
		}
		if last {
			object[step.key] = value
			return true
		}
		child, ok := object[step.key]
		if !ok {
			return false
		}
		current = child
	}
	return false
}

func schemaStringAtPath(document map[string]any, path []schemaPathStep) (string, bool) {
	if document == nil || len(path) == 0 {
		return "", false
	}
	var current any = document
	for _, step := range path {
		if step.isIndex {
			items, ok := current.([]any)
			if !ok || step.index < 0 || step.index >= len(items) {
				return "", false
			}
			current = items[step.index]
			continue
		}
		object, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		child, ok := object[step.key]
		if !ok {
			return "", false
		}
		current = child
	}
	value, ok := current.(string)
	return value, ok
}

func clearSchemaValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			clearSchemaValue(child)
			if _, ok := child.(string); ok {
				typed[key] = ""
			}
		}
	case []any:
		for index, child := range typed {
			clearSchemaValue(child)
			if _, ok := child.(string); ok {
				typed[index] = ""
			}
		}
	}
}

func (p *Plugin) installCompiledSchemas(header, body *util.CompiledSchema) error {
	state := &p.secrets
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.retired {
		return secret.ErrCredentialUnavailable
	}
	p.config.headerSchema = header
	p.config.bodySchema = body
	return nil
}

func (p *Plugin) acquireValidationSchemas() (requestValidationSchemaSnapshot, func(), error) {
	state := &p.secrets
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.retired ||
		(p.config.HeaderSchema != nil && p.config.headerSchema == nil && len(state.headerSecrets) == 0) ||
		(p.config.BodySchema != nil && p.config.bodySchema == nil && len(state.bodySecrets) == 0) {
		return requestValidationSchemaSnapshot{}, nil, secret.ErrCredentialUnavailable
	}
	if state.activeUses == 0 {
		state.usesDone = make(chan struct{})
	}
	state.activeUses++
	return requestValidationSchemaSnapshot{
		headerSchema:    cloneSchemaDocument(p.config.HeaderSchema),
		bodySchema:      cloneSchemaDocument(p.config.BodySchema),
		headerSecrets:   cloneSchemaSecrets(state.headerSecrets),
		bodySecrets:     cloneSchemaSecrets(state.bodySecrets),
		headerCompiled:  p.config.headerSchema,
		bodyCompiled:    p.config.bodySchema,
		headerSensitive: len(state.headerSecrets) > 0,
		bodySensitive:   len(state.bodySecrets) > 0,
	}, p.releaseSchemaUse, nil
}

func (p *Plugin) Stop() {
	p.secrets.stopOnce.Do(func() {
		state := &p.secrets
		state.initializeCompileGate()
		state.mu.Lock()
		state.retired = true
		close(state.stopping)
		wait := state.usesDone
		state.mu.Unlock()
		if wait != nil {
			<-wait
		}
		state.mu.Lock()
		for index := range state.headerSecrets {
			state.headerSecrets[index] = schemaSecret{}
		}
		for index := range state.bodySecrets {
			state.bodySecrets[index] = schemaSecret{}
		}
		state.headerSecrets = nil
		state.bodySecrets = nil
		state.compileLimiter = secret.GenerationLimiter{}
		state.prepared = false
		p.config.headerSchema = nil
		p.config.bodySchema = nil
		state.mu.Unlock()
	})
}
