package server

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/wklken/apisix-go/pkg/compiler"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/runtime"
)

var _ generation.PublicationEngine = (*GenerationEngine)(nil)

var (
	errGenerationEngineClosed             = errors.New("generation engine is closed")
	errGenerationRecoveryNotInstalled     = errors.New("generation recovery is not installed")
	errGenerationRecoveryAlreadyInstalled = errors.New("generation recovery is already installed")
)

type preparedKey struct {
	Desired uint64
	HTTP    [32]byte
	Stream  [32]byte
}

type pendingRecord struct {
	key            preparedKey
	set            generation.PublicationSet
	owner          *generationOwner
	synthetic      bool
	discarding     bool
	discardDone    chan struct{}
	discardErr     error
	discardWaiters int
}

type activationRecord struct {
	token     generation.PublicationToken
	key       preparedKey
	set       generation.PublicationSet
	previous  *activeBundle
	candidate *activeBundle
	owner     *generationOwner
	restored  bool
}

type retirementRecord struct {
	owner *generationOwner
	ctx   context.Context
}

// GenerationEngine owns immutable prepared generations from compilation
// through atomic protocol publication and eventual lease-aware retirement.
type GenerationEngine struct {
	server  *Server
	factory *compiler.WorkerCompilerFactory

	mu                sync.Mutex
	closed            bool
	recoveryInstalled bool
	initialized       bool
	pending           map[preparedKey]*pendingRecord
	activations       map[generation.PublicationToken]*activationRecord
	fences            map[generation.Domain]generation.PublicationCandidate
	active            atomic.Pointer[activeBundle]

	retireMu      sync.Mutex
	retireQueue   []retirementRecord
	retireKnown   map[*generationOwner]struct{}
	retireActive  map[*generationOwner]struct{}
	retireErrors  []error
	retireWG      sync.WaitGroup
	retireClosing bool
	retireWake    chan struct{}
	retireStop    chan struct{}
	retireDone    chan struct{}

	// A stream route remains recordable while any active or draining owner can
	// still emit its terminal connection result.
	streamMetricsMu    sync.Mutex
	streamMetricOwners map[*generationOwner][]string
	streamMetricRefs   map[string]uint64

	checkpoint func(string) error

	closeOnce sync.Once
	closeErr  error
}

func NewGenerationEngine(
	server *Server,
	factory *compiler.WorkerCompilerFactory,
) (*GenerationEngine, error) {
	if server == nil || factory == nil {
		return nil, fmt.Errorf("%w: generation engine dependencies are required", compiler.ErrInvalidInput)
	}
	engine := &GenerationEngine{
		server:             server,
		factory:            factory,
		pending:            make(map[preparedKey]*pendingRecord),
		activations:        make(map[generation.PublicationToken]*activationRecord),
		fences:             make(map[generation.Domain]generation.PublicationCandidate),
		retireKnown:        make(map[*generationOwner]struct{}),
		retireActive:       make(map[*generationOwner]struct{}),
		retireWake:         make(chan struct{}, 1),
		retireStop:         make(chan struct{}),
		retireDone:         make(chan struct{}),
		streamMetricOwners: make(map[*generationOwner][]string),
		streamMetricRefs:   make(map[string]uint64),
	}
	engine.active.Store(&activeBundle{})
	if err := bindGenerationEngine(server, engine); err != nil {
		return nil, err
	}
	go engine.retirementLoop()
	return engine, nil
}

func (engine *GenerationEngine) Prepare(
	ctx context.Context,
	ticket generation.ApplyTicket,
	desired generation.Snapshot,
	previous map[generation.Domain]generation.PublishedGeneration,
) (generation.PublicationSet, error) {
	if engine == nil || ctx == nil {
		return generation.PublicationSet{}, compiler.ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return generation.PublicationSet{}, err
	}
	if ticket.DesiredRevision == 0 || desired.Revision() != ticket.DesiredRevision ||
		desired.Digest() != ticket.DesiredDigest {
		return generation.PublicationSet{}, generation.ErrIntegrity
	}
	set := generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate),
	}
	if len(ticket.RequiredDomains) == 0 {
		if err := generation.ValidatePublicationSet(ticket, set); err != nil {
			return generation.PublicationSet{}, err
		}
		key, err := preparedKeyFromSet(set)
		if err != nil {
			return generation.PublicationSet{}, err
		}
		engine.mu.Lock()
		defer engine.mu.Unlock()
		if err := engine.requireReadyLocked(); err != nil {
			return generation.PublicationSet{}, err
		}
		if engine.preparedKeyInUseLocked(key) {
			return generation.PublicationSet{}, compiler.ErrPreparedSetMismatch
		}
		engine.pending[key] = &pendingRecord{
			key: key, set: cloneEnginePublicationSetValue(set), synthetic: true, discardDone: make(chan struct{}),
		}
		return cloneEnginePublicationSetValue(set), nil
	}

	engine.mu.Lock()
	if err := engine.requireReadyLocked(); err != nil {
		engine.mu.Unlock()
		return generation.PublicationSet{}, err
	}
	engine.mu.Unlock()

	prepared, err := engine.factory.PrepareGeneration(ctx, ticket, desired, previous, engine.onTaskFailure)
	if err != nil {
		return generation.PublicationSet{}, err
	}
	set = prepared.PublicationSet()
	cleanup := func(primary error) (generation.PublicationSet, error) {
		cleanupErr := prepared.DiscardPrepared(context.WithoutCancel(ctx), set)
		return generation.PublicationSet{}, errors.Join(primary, cleanupErr)
	}
	if err := generation.ValidatePublicationSet(ticket, set); err != nil {
		return cleanup(err)
	}
	key, err := preparedKeyFromSet(set)
	if err != nil {
		return cleanup(err)
	}
	owner := newGenerationOwner(prepared)
	engine.mu.Lock()
	if err := engine.requireReadyLocked(); err != nil {
		engine.mu.Unlock()
		return cleanup(err)
	}
	if engine.preparedKeyInUseLocked(key) {
		engine.mu.Unlock()
		return cleanup(compiler.ErrPreparedSetMismatch)
	}
	engine.pending[key] = &pendingRecord{
		key: key, set: cloneEnginePublicationSetValue(set), owner: owner, discardDone: make(chan struct{}),
	}
	engine.mu.Unlock()
	return cloneEnginePublicationSetValue(set), nil
}

func (engine *GenerationEngine) DiscardPrepared(
	ctx context.Context,
	set generation.PublicationSet,
) error {
	if engine == nil || ctx == nil {
		return compiler.ErrPreparedSetMismatch
	}
	key, err := preparedKeyFromSet(set)
	if err != nil {
		return compiler.ErrPreparedSetMismatch
	}
	engine.mu.Lock()
	record := engine.pending[key]
	if record == nil || !reflect.DeepEqual(record.set, set) {
		engine.mu.Unlock()
		return compiler.ErrPreparedSetMismatch
	}
	if record.discarding {
		record.discardWaiters++
		done := record.discardDone
		if engine.checkpoint != nil {
			_ = engine.checkpoint("discard-waiter-registered")
		}
		engine.mu.Unlock()
		select {
		case <-done:
			engine.mu.Lock()
			record.discardWaiters--
			err := record.discardErr
			if record.discardWaiters == 0 {
				delete(engine.pending, key)
			}
			engine.mu.Unlock()
			return err
		case <-ctx.Done():
			engine.mu.Lock()
			record.discardWaiters--
			engine.mu.Unlock()
			return ctx.Err()
		}
	}
	record.discarding = true
	record.discardWaiters = 1
	engine.mu.Unlock()

	cleanupCtx := context.WithoutCancel(ctx)
	if record.owner != nil {
		record.discardErr = record.owner.prepared.DiscardPrepared(cleanupCtx, record.set)
	}
	close(record.discardDone)
	engine.mu.Lock()
	record.discardWaiters--
	if record.discardWaiters == 0 {
		delete(engine.pending, key)
	}
	err = record.discardErr
	engine.mu.Unlock()
	return err
}

func (engine *GenerationEngine) Activate(
	ctx context.Context,
	token generation.PublicationToken,
	set generation.PublicationSet,
) error {
	if engine == nil || ctx == nil || token == "" {
		return compiler.ErrPreparedSetMismatch
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := preparedKeyFromSet(set)
	if err != nil {
		return compiler.ErrPreparedSetMismatch
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if err := engine.requireReadyLocked(); err != nil {
		return err
	}
	if _, exists := engine.activations[token]; exists {
		return compiler.ErrPreparedSetMismatch
	}
	record := engine.pending[key]
	if record == nil || record.discarding || !reflect.DeepEqual(record.set, set) {
		return compiler.ErrPreparedSetMismatch
	}
	previous := engine.active.Load()
	if previous == nil {
		return generation.ErrIntegrity
	}
	candidate := &activeBundle{http: previous.http, stream: previous.stream}
	domains, err := ownerDomainsFromSet(set)
	if err != nil {
		return err
	}
	if record.owner != nil {
		if domains&ownerDomainHTTP != 0 && record.owner.prepared.HTTP() == nil {
			return generation.ErrIntegrity
		}
		if domains&ownerDomainStream != 0 && record.owner.prepared.Stream() == nil {
			return generation.ErrIntegrity
		}
		record.owner.activateDomains(domains)
		if domains&ownerDomainStream != 0 {
			engine.registerStreamMetricOwner(record.owner)
		}
		next := candidate.withDomains(record.owner, domains)
		candidate = &next
	}
	delete(engine.pending, key)
	activation := &activationRecord{
		token: token, key: key, set: cloneEnginePublicationSetValue(set), previous: previous,
		candidate: candidate, owner: record.owner,
	}
	engine.activations[token] = activation
	if record.owner == nil {
		return nil
	}
	engine.active.Store(candidate)
	if engine.checkpoint != nil {
		if err := engine.checkpoint("candidate-bundle-published"); err != nil {
			engine.active.Store(previous)
			record.owner.deactivateDomains(domains)
			activation.restored = true
			return err
		}
	}
	return nil
}

func (engine *GenerationEngine) RollbackActivation(
	ctx context.Context,
	token generation.PublicationToken,
	set generation.PublicationSet,
) error {
	return engine.rollbackActivation(ctx, token, set)
}

func (engine *GenerationEngine) FinalizeActivation(
	ctx context.Context,
	token generation.PublicationToken,
	set generation.PublicationSet,
) {
	engine.finalizeActivation(ctx, token, set)
}

func (engine *GenerationEngine) ConfirmActive(
	ctx context.Context,
	set generation.PublicationSet,
) error {
	return engine.confirmActive(ctx, set)
}

func (engine *GenerationEngine) InstallRecovery(
	ctx context.Context,
	state generation.RecoveryState,
) error {
	return engine.installRecovery(ctx, state)
}

func (engine *GenerationEngine) Close(ctx context.Context) error {
	return engine.close(ctx)
}

func (engine *GenerationEngine) acquireHTTP() (httpGenerationLease, bool) {
	if engine == nil {
		return httpGenerationLease{}, false
	}
	for {
		engine.mu.Lock()
		if engine.closed || !engine.recoveryInstalled {
			engine.mu.Unlock()
			return httpGenerationLease{}, false
		}
		bundle := engine.active.Load()
		checkpoint := engine.checkpoint
		engine.mu.Unlock()
		if checkpoint != nil {
			_ = checkpoint("http-bundle-loaded")
		}
		if bundle == nil {
			return httpGenerationLease{}, false
		}
		if lease, ok := bundle.http.acquireHTTP(); ok {
			return lease, true
		}
		if engine.active.Load() == bundle {
			return httpGenerationLease{}, false
		}
	}
}

func (engine *GenerationEngine) acquireStream() (streamGenerationLease, bool) {
	if engine == nil {
		return streamGenerationLease{}, false
	}
	for {
		engine.mu.Lock()
		if engine.closed || !engine.recoveryInstalled {
			engine.mu.Unlock()
			return streamGenerationLease{}, false
		}
		bundle := engine.active.Load()
		checkpoint := engine.checkpoint
		engine.mu.Unlock()
		if checkpoint != nil {
			_ = checkpoint("stream-bundle-loaded")
		}
		if bundle == nil {
			return streamGenerationLease{}, false
		}
		if lease, ok := bundle.stream.acquireStream(); ok {
			return lease, true
		}
		if engine.active.Load() == bundle {
			return streamGenerationLease{}, false
		}
	}
}

func (engine *GenerationEngine) refreshStreamMetrics() {
	if engine == nil {
		return
	}
	engine.streamMetricsMu.Lock()
	defer engine.streamMetricsMu.Unlock()
	engine.publishStreamMetricsLocked()
}

func preparedKeyFromSet(set generation.PublicationSet) (preparedKey, error) {
	if set.DesiredRevision == 0 || len(set.Domains) > 2 {
		return preparedKey{}, generation.ErrIntegrity
	}
	key := preparedKey{Desired: set.DesiredRevision}
	for domain, candidate := range set.Domains {
		if err := generation.ValidatePublicationCandidate(domain, set.DesiredRevision, candidate); err != nil {
			return preparedKey{}, err
		}
		switch domain {
		case generation.DomainHTTP:
			key.HTTP = candidate.Artifact.Digest
		case generation.DomainStream:
			key.Stream = candidate.Artifact.Digest
		default:
			return preparedKey{}, generation.ErrIntegrity
		}
	}
	return key, nil
}

func ownerDomainsFromSet(set generation.PublicationSet) (ownerDomain, error) {
	var domains ownerDomain
	for domain := range set.Domains {
		switch domain {
		case generation.DomainHTTP:
			domains |= ownerDomainHTTP
		case generation.DomainStream:
			domains |= ownerDomainStream
		default:
			return 0, generation.ErrIntegrity
		}
	}
	return domains, nil
}

func cloneEnginePublicationSetValue(set generation.PublicationSet) generation.PublicationSet {
	cloned := generation.PublicationSet{
		DesiredRevision: set.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(set.Domains)),
	}
	for domain, candidate := range set.Domains {
		candidate.Snapshot = candidate.Snapshot.Clone()
		candidate.Closure = slices.Clone(candidate.Closure)
		candidate.Decisions = slices.Clone(candidate.Decisions)
		cloned.Domains[domain] = candidate
	}
	return cloned
}

func (engine *GenerationEngine) requireReadyLocked() error {
	if engine.closed {
		return errGenerationEngineClosed
	}
	if !engine.recoveryInstalled {
		return errGenerationRecoveryNotInstalled
	}
	return nil
}

func (engine *GenerationEngine) preparedKeyInUseLocked(key preparedKey) bool {
	if _, exists := engine.pending[key]; exists {
		return true
	}
	for _, record := range engine.activations {
		if record.key == key {
			return true
		}
	}
	return false
}

func (*GenerationEngine) onTaskFailure(runtime.TaskFailure) {}

func (engine *GenerationEngine) rollbackActivation(
	ctx context.Context,
	token generation.PublicationToken,
	set generation.PublicationSet,
) error {
	if engine == nil {
		return compiler.ErrPreparedSetMismatch
	}
	if ctx == nil {
		ctx = context.Background()
	}
	engine.mu.Lock()
	record := engine.activations[token]
	if record == nil || !reflect.DeepEqual(record.set, set) {
		engine.mu.Unlock()
		panic("generation activation token and publication set diverged")
	}
	delete(engine.activations, token)
	if record.owner == nil {
		engine.mu.Unlock()
		return nil
	}
	engine.active.Store(record.previous)
	if !record.restored {
		domains, err := ownerDomainsFromSet(record.set)
		if err != nil {
			engine.mu.Unlock()
			panic("generation activation contained invalid domains")
		}
		record.owner.deactivateDomains(domains)
	}
	engine.enqueueRetirementLocked(record.owner, context.WithoutCancel(ctx))
	engine.mu.Unlock()
	engine.wakeRetirement()
	select {
	case <-record.owner.closeDone:
		return record.owner.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (engine *GenerationEngine) finalizeActivation(
	ctx context.Context,
	token generation.PublicationToken,
	set generation.PublicationSet,
) {
	if engine == nil {
		panic("finalize nil generation engine")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	engine.mu.Lock()
	record := engine.activations[token]
	if record == nil || !reflect.DeepEqual(record.set, set) || record.restored {
		engine.mu.Unlock()
		panic("finalize generation activation invariant violated")
	}
	delete(engine.activations, token)
	if record.owner == nil {
		engine.initialized = true
		engine.mu.Unlock()
		return
	}
	retire := make(map[*generationOwner]ownerDomain)
	for domain, candidate := range record.set.Domains {
		engine.fences[domain] = cloneEngineCandidate(candidate)
		var predecessor *generationOwner
		var mask ownerDomain
		switch domain {
		case generation.DomainHTTP:
			predecessor = record.previous.http
			mask = ownerDomainHTTP
		case generation.DomainStream:
			predecessor = record.previous.stream
			mask = ownerDomainStream
		default:
			engine.mu.Unlock()
			panic("finalize generation activation domain invariant violated")
		}
		if predecessor != nil && predecessor != record.owner {
			retire[predecessor] |= mask
		}
	}
	for owner, domains := range retire {
		if owner.deactivateDomains(domains) {
			engine.enqueueRetirementLocked(owner, context.WithoutCancel(ctx))
		}
	}
	engine.initialized = true
	engine.mu.Unlock()
	engine.wakeRetirement()
}

func (engine *GenerationEngine) confirmActive(
	ctx context.Context,
	set generation.PublicationSet,
) error {
	if engine == nil || ctx == nil {
		return generation.ErrActiveGenerationMismatch
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := preparedKeyFromSet(set); err != nil {
		return generation.ErrActiveGenerationMismatch
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if engine.closed || !engine.recoveryInstalled {
		return generation.ErrActiveGenerationMismatch
	}
	if len(set.Domains) == 0 {
		if engine.initialized {
			return nil
		}
		return generation.ErrActiveGenerationMismatch
	}
	for domain, candidate := range set.Domains {
		if !reflect.DeepEqual(engine.fences[domain], candidate) {
			return generation.ErrActiveGenerationMismatch
		}
	}
	return nil
}

func (engine *GenerationEngine) installRecovery(
	ctx context.Context,
	state generation.RecoveryState,
) error {
	if engine == nil || ctx == nil {
		return compiler.ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	engine.mu.Lock()
	if engine.closed || engine.recoveryInstalled || engine.initialized ||
		len(engine.pending) != 0 || len(engine.activations) != 0 ||
		engine.active.Load().http != nil || engine.active.Load().stream != nil {
		engine.mu.Unlock()
		return errGenerationRecoveryAlreadyInstalled
	}
	if exactEmptyRecoveryState(state) {
		engine.recoveryInstalled = true
		engine.mu.Unlock()
		return nil
	}
	engine.mu.Unlock()
	if err := generation.ValidateRecoverySet(state.Revisions, state.Published); err != nil {
		return err
	}
	if len(state.Published) == 0 {
		engine.mu.Lock()
		if engine.closed || engine.recoveryInstalled {
			engine.mu.Unlock()
			return errGenerationRecoveryAlreadyInstalled
		}
		engine.recoveryInstalled = true
		engine.initialized = true
		engine.mu.Unlock()
		return nil
	}
	prepared, err := engine.factory.PrepareRecovery(ctx, state.Revisions, state.Published, engine.onTaskFailure)
	if err != nil {
		return err
	}
	expectedSet := recoveryPublicationSet(state.Revisions, state.Published)
	if !reflect.DeepEqual(prepared.PublicationSet(), expectedSet) {
		return errors.Join(generation.ErrIntegrity, prepared.Close(context.WithoutCancel(ctx)))
	}
	owner := newGenerationOwner(prepared)
	var domains ownerDomain
	bundle := &activeBundle{}
	fences := make(map[generation.Domain]generation.PublicationCandidate, len(state.Published))
	for domain, published := range state.Published {
		candidate := generation.PublicationCandidate(published)
		fences[domain] = cloneEngineCandidate(candidate)
		switch domain {
		case generation.DomainHTTP:
			if prepared.HTTP() == nil {
				return errors.Join(generation.ErrIntegrity, prepared.Close(context.WithoutCancel(ctx)))
			}
			domains |= ownerDomainHTTP
			bundle.http = owner
		case generation.DomainStream:
			if prepared.Stream() == nil {
				return errors.Join(generation.ErrIntegrity, prepared.Close(context.WithoutCancel(ctx)))
			}
			domains |= ownerDomainStream
			bundle.stream = owner
		default:
			return errors.Join(generation.ErrIntegrity, prepared.Close(context.WithoutCancel(ctx)))
		}
	}
	owner.activateDomains(domains)
	engine.mu.Lock()
	if engine.closed || engine.recoveryInstalled {
		engine.mu.Unlock()
		owner.deactivateDomains(domains)
		closeErr := owner.closePrepared(context.WithoutCancel(ctx))
		return errors.Join(errGenerationRecoveryAlreadyInstalled, closeErr)
	}
	if domains&ownerDomainStream != 0 {
		engine.registerStreamMetricOwner(owner)
	}
	engine.active.Store(bundle)
	engine.fences = fences
	engine.recoveryInstalled = true
	engine.initialized = true
	engine.mu.Unlock()
	return nil
}

func (engine *GenerationEngine) close(ctx context.Context) error {
	if engine == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cleanupCtx := context.WithoutCancel(ctx)
	engine.closeOnce.Do(func() {
		engine.mu.Lock()
		engine.closed = true
		bundle := engine.active.Load()
		engine.active.Store(&activeBundle{})
		pending := make([]*pendingRecord, 0, len(engine.pending))
		for _, record := range engine.pending {
			pending = append(pending, record)
		}
		engine.pending = make(map[preparedKey]*pendingRecord)

		owners := make(map[*generationOwner]struct{})
		for _, record := range engine.activations {
			if record.owner != nil {
				owners[record.owner] = struct{}{}
			}
			if record.previous != nil {
				if record.previous.http != nil {
					owners[record.previous.http] = struct{}{}
				}
				if record.previous.stream != nil {
					owners[record.previous.stream] = struct{}{}
				}
			}
		}
		if bundle != nil && bundle.http != nil {
			owners[bundle.http] = struct{}{}
		}
		if bundle != nil && bundle.stream != nil {
			owners[bundle.stream] = struct{}{}
		}
		engine.activations = make(map[generation.PublicationToken]*activationRecord)
		for owner := range owners {
			owner.mu.Lock()
			domains := owner.activeDomains
			owner.mu.Unlock()
			if domains != 0 {
				owner.deactivateDomains(domains)
			}
			engine.enqueueRetirementLocked(owner, cleanupCtx)
		}
		engine.mu.Unlock()

		var cleanupErrors []error
		for _, record := range pending {
			if record.discarding {
				<-record.discardDone
				cleanupErrors = append(cleanupErrors, record.discardErr)
				continue
			}
			if record.owner != nil {
				cleanupErrors = append(cleanupErrors,
					record.owner.prepared.DiscardPrepared(cleanupCtx, record.set))
			}
		}
		engine.wakeRetirement()
		engine.retireMu.Lock()
		engine.retireClosing = true
		engine.retireMu.Unlock()
		close(engine.retireStop)
		<-engine.retireDone
		engine.retireWG.Wait()
		engine.clearStreamMetrics()
		engine.retireMu.Lock()
		cleanupErrors = append(cleanupErrors, engine.retireErrors...)
		engine.retireMu.Unlock()
		cleanupErrors = append(cleanupErrors, engine.factory.Close(cleanupCtx))
		engine.closeErr = errors.Join(cleanupErrors...)
	})
	return engine.closeErr
}

func (engine *GenerationEngine) enqueueRetirementLocked(owner *generationOwner, ctx context.Context) {
	if owner == nil {
		return
	}
	engine.retireMu.Lock()
	if engine.retireClosing {
		engine.retireMu.Unlock()
		panic("enqueue generation owner after retirement shutdown")
	}
	if _, exists := engine.retireKnown[owner]; exists {
		engine.retireMu.Unlock()
		return
	}
	select {
	case <-owner.closeDone:
		engine.retireMu.Unlock()
		return
	default:
	}
	engine.retireKnown[owner] = struct{}{}
	engine.retireQueue = append(engine.retireQueue, retirementRecord{owner: owner, ctx: ctx})
	engine.retireMu.Unlock()
}

func (engine *GenerationEngine) wakeRetirement() {
	select {
	case engine.retireWake <- struct{}{}:
	default:
	}
}

func (engine *GenerationEngine) retirementLoop() {
	defer close(engine.retireDone)
	stopping := false
	for {
		engine.retireMu.Lock()
		if len(engine.retireQueue) != 0 {
			record := engine.retireQueue[0]
			engine.retireQueue = engine.retireQueue[1:]
			engine.retireActive[record.owner] = struct{}{}
			engine.retireWG.Add(1)
			engine.retireMu.Unlock()
			go engine.retireOwner(record)
			continue
		}
		engine.retireMu.Unlock()
		if stopping {
			return
		}
		select {
		case <-engine.retireWake:
		case <-engine.retireStop:
			stopping = true
		}
	}
}

func (engine *GenerationEngine) retireOwner(record retirementRecord) {
	defer engine.retireWG.Done()
	engine.mu.Lock()
	checkpoint := engine.checkpoint
	engine.mu.Unlock()
	if checkpoint != nil {
		_ = checkpoint("before-owner-retirement")
	}
	err := record.owner.closePrepared(record.ctx)
	engine.unregisterStreamMetricOwner(record.owner)
	engine.retireMu.Lock()
	delete(engine.retireActive, record.owner)
	delete(engine.retireKnown, record.owner)
	if err != nil {
		engine.retireErrors = append(engine.retireErrors, err)
	}
	engine.retireMu.Unlock()
}

func (engine *GenerationEngine) registerStreamMetricOwner(owner *generationOwner) {
	if owner == nil || owner.prepared == nil {
		return
	}
	snapshot := owner.prepared.Stream()
	if snapshot == nil || snapshot.Router() == nil {
		return
	}
	routeIDs := snapshot.Router().RouteIDs()
	unique := make(map[string]struct{}, len(routeIDs))
	registered := make([]string, 0, len(routeIDs))
	for _, routeID := range routeIDs {
		if routeID == "" {
			continue
		}
		if _, exists := unique[routeID]; exists {
			continue
		}
		unique[routeID] = struct{}{}
		registered = append(registered, routeID)
	}
	engine.streamMetricsMu.Lock()
	defer engine.streamMetricsMu.Unlock()
	if _, exists := engine.streamMetricOwners[owner]; exists {
		panic("register stream metric owner twice")
	}
	engine.streamMetricOwners[owner] = registered
	for _, routeID := range registered {
		engine.streamMetricRefs[routeID]++
	}
	engine.publishStreamMetricsLocked()
}

func (engine *GenerationEngine) unregisterStreamMetricOwner(owner *generationOwner) {
	if owner == nil {
		return
	}
	engine.streamMetricsMu.Lock()
	defer engine.streamMetricsMu.Unlock()
	routeIDs, exists := engine.streamMetricOwners[owner]
	if !exists {
		return
	}
	delete(engine.streamMetricOwners, owner)
	for _, routeID := range routeIDs {
		refs := engine.streamMetricRefs[routeID]
		if refs == 0 {
			panic("unregister stream metric route without owner reference")
		}
		if refs == 1 {
			delete(engine.streamMetricRefs, routeID)
			continue
		}
		engine.streamMetricRefs[routeID] = refs - 1
	}
	engine.publishStreamMetricsLocked()
}

func (engine *GenerationEngine) publishStreamMetricsLocked() {
	routeIDs := make([]string, 0, len(engine.streamMetricRefs))
	for routeID := range engine.streamMetricRefs {
		routeIDs = append(routeIDs, routeID)
	}
	metrics.SetStreamRoutes(routeIDs)
}

func (engine *GenerationEngine) clearStreamMetrics() {
	engine.streamMetricsMu.Lock()
	defer engine.streamMetricsMu.Unlock()
	clear(engine.streamMetricOwners)
	clear(engine.streamMetricRefs)
	metrics.SetStreamRoutes(nil)
}

func exactEmptyRecoveryState(state generation.RecoveryState) bool {
	return state.Revisions == (generation.RevisionSet{}) && state.Desired.Revision() == 0 &&
		len(state.Published) == 0 && len(state.Failures) == 0
}

func cloneEngineCandidate(candidate generation.PublicationCandidate) generation.PublicationCandidate {
	candidate.Snapshot = candidate.Snapshot.Clone()
	candidate.Closure = slices.Clone(candidate.Closure)
	candidate.Decisions = slices.Clone(candidate.Decisions)
	return candidate
}

func recoveryPublicationSet(
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) generation.PublicationSet {
	set := generation.PublicationSet{
		DesiredRevision: revisions.Desired,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(published)),
	}
	for domain, value := range published {
		set.Domains[domain] = cloneEngineCandidate(generation.PublicationCandidate(value))
	}
	return set
}

func bindGenerationEngine(server *Server, engine *GenerationEngine) error {
	if server == nil || engine == nil {
		return compiler.ErrInvalidInput
	}
	if server.routes != nil {
		return fmt.Errorf("%w: server runtime source is already bound", compiler.ErrInvalidInput)
	}
	server.routes = newGenerationRouteHandler(engine.acquireHTTP)
	return nil
}
