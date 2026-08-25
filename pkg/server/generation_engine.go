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

type discardAttempt struct {
	done     chan struct{}
	err      error
	waiters  int
	terminal bool
}

type pendingRecord struct {
	key       preparedKey
	set       generation.PublicationSet
	owner     *generationOwner
	synthetic bool
	discard   *discardAttempt
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
	owner  *generationOwner
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

type generationEngineClosePhase uint8

const (
	engineCloseInitial generationEngineClosePhase = iota
	engineCloseOwnersCaptured
	engineClosePendingDone
	engineCloseRetirementDone
	engineCloseFactoryDone
)

type generationEngineCloseAttempt struct {
	done chan struct{}
	err  error
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
	retireQueue   []*retirementRecord
	retireKnown   map[*generationOwner]struct{}
	retireActive  map[*generationOwner]*retirementRecord
	retireLatest  map[*generationOwner]error
	retireErrors  []error
	retireClosing bool
	retireChanged chan struct{}
	retireWake    chan struct{}
	retireStop    chan struct{}
	retireStopped bool
	retireDone    chan struct{}

	// A stream route remains recordable while any active or draining owner can
	// still emit its terminal connection result.
	streamMetricsMu    sync.Mutex
	streamMetricOwners map[*generationOwner][]string
	streamMetricRefs   map[string]uint64

	checkpoint func(string) error

	closeMu      sync.Mutex
	closeAttempt *generationEngineCloseAttempt
	closePhase   generationEngineClosePhase
	closePending map[preparedKey]*pendingRecord
	closeErrors  []error
	closeErr     error
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
		retireActive:       make(map[*generationOwner]*retirementRecord),
		retireLatest:       make(map[*generationOwner]error),
		retireChanged:      make(chan struct{}),
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
			key: key, set: cloneEnginePublicationSetValue(set), synthetic: true,
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
		key: key, set: cloneEnginePublicationSetValue(set), owner: owner,
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
	if err := ctx.Err(); err != nil {
		engine.mu.Unlock()
		return err
	}
	if attempt := record.discard; attempt != nil {
		select {
		case <-attempt.done:
			if attempt.terminal {
				attempt.waiters++
				engine.mu.Unlock()
				return engine.waitDiscardAttempt(ctx, record, attempt, true)
			}
		default:
			attempt.waiters++
			engine.mu.Unlock()
			engine.runCheckpoint("discard-waiter-registered")
			return engine.waitDiscardAttempt(ctx, record, attempt, true)
		}
	}
	attempt := &discardAttempt{done: make(chan struct{}), waiters: 1}
	record.discard = attempt
	engine.mu.Unlock()
	engine.runCheckpoint("discard-attempt-started")

	var discardErr error
	if record.owner != nil {
		discardErr = record.owner.prepared.DiscardPrepared(ctx, record.set)
	}
	engine.mu.Lock()
	attempt.err = discardErr
	attempt.terminal = !generationEngineCleanupIncomplete(discardErr)
	close(attempt.done)
	engine.mu.Unlock()
	return engine.consumeDiscardAttempt(record, attempt)
}

func (engine *GenerationEngine) waitDiscardAttempt(
	ctx context.Context,
	record *pendingRecord,
	attempt *discardAttempt,
	joined bool,
) error {
	select {
	case <-attempt.done:
		if joined {
			engine.runCheckpoint("discard-waiter-observed")
		}
		return engine.consumeDiscardAttempt(record, attempt)
	case <-ctx.Done():
		select {
		case <-attempt.done:
			if joined {
				engine.runCheckpoint("discard-waiter-observed")
			}
			return engine.consumeDiscardAttempt(record, attempt)
		default:
		}
		engine.mu.Lock()
		attempt.waiters--
		if attempt.waiters == 0 && attempt.terminal && record.discard == attempt {
			delete(engine.pending, record.key)
		}
		engine.mu.Unlock()
		return ctx.Err()
	}
}

func (engine *GenerationEngine) consumeDiscardAttempt(
	record *pendingRecord,
	attempt *discardAttempt,
) error {
	engine.mu.Lock()
	attempt.waiters--
	if attempt.waiters == 0 && attempt.terminal && record.discard == attempt {
		delete(engine.pending, record.key)
	}
	err := attempt.err
	engine.mu.Unlock()
	return err
}

func (engine *GenerationEngine) runCheckpoint(stage string) {
	engine.mu.Lock()
	checkpoint := engine.checkpoint
	engine.mu.Unlock()
	if checkpoint != nil {
		_ = checkpoint(stage)
	}
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
	if record == nil || record.discard != nil || !reflect.DeepEqual(record.set, set) {
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
	engine.closeMu.Lock()
	if engine.closePhase == engineCloseFactoryDone {
		closeErr := engine.closeErr
		engine.closeMu.Unlock()
		return closeErr
	}
	if engine.closeAttempt != nil {
		attempt := engine.closeAttempt
		engine.closeMu.Unlock()
		return waitGenerationEngineCloseAttempt(ctx, attempt)
	}
	attempt := &generationEngineCloseAttempt{done: make(chan struct{})}
	engine.closeAttempt = attempt
	engine.closeMu.Unlock()

	attemptErr := engine.runCloseAttempt(ctx)
	engine.closeMu.Lock()
	attempt.err = attemptErr
	if engine.closePhase == engineCloseFactoryDone {
		engine.closeErr = attemptErr
	}
	if engine.closeAttempt == attempt {
		engine.closeAttempt = nil
	}
	close(attempt.done)
	engine.closeMu.Unlock()
	return attemptErr
}

func (engine *GenerationEngine) runCloseAttempt(ctx context.Context) error {
	if engine.closePhaseValue() == engineCloseInitial {
		engine.captureCloseOwners(ctx)
	}
	if engine.closePhaseValue() == engineCloseOwnersCaptured {
		if err := engine.closePendingOwners(ctx); err != nil {
			return engine.closeAttemptResult(err)
		}
		engine.setClosePhase(engineClosePendingDone)
	}
	if engine.closePhaseValue() == engineClosePendingDone {
		if err := engine.closeRetirementOwners(ctx); err != nil {
			return engine.closeAttemptResult(err)
		}
		engine.setClosePhase(engineCloseRetirementDone)
	}
	if engine.closePhaseValue() == engineCloseRetirementDone {
		engine.runCheckpoint("factory-close")
		factoryErr := engine.factory.Close(ctx)
		if generationEngineCleanupIncomplete(factoryErr) {
			return engine.closeAttemptResult(factoryErr)
		}
		engine.appendCloseError(factoryErr)
		engine.setClosePhase(engineCloseFactoryDone)
	}
	return engine.closeAttemptResult(nil)
}

func (engine *GenerationEngine) captureCloseOwners(ctx context.Context) {
	engine.mu.Lock()
	engine.closed = true
	bundle := engine.active.Load()
	engine.active.Store(&activeBundle{})
	pending := engine.pending
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
		engine.enqueueRetirementLocked(owner, ctx)
	}
	engine.mu.Unlock()
	engine.closeMu.Lock()
	engine.closePending = pending
	engine.closePhase = engineCloseOwnersCaptured
	engine.closeMu.Unlock()
	engine.wakeRetirement()
}

func (engine *GenerationEngine) closePendingOwners(ctx context.Context) error {
	engine.closeMu.Lock()
	records := make([]*pendingRecord, 0, len(engine.closePending))
	for _, record := range engine.closePending {
		records = append(records, record)
	}
	engine.closeMu.Unlock()
	slices.SortFunc(records, func(left, right *pendingRecord) int {
		if left.key.Desired < right.key.Desired {
			return -1
		}
		if left.key.Desired > right.key.Desired {
			return 1
		}
		if compared := slices.Compare(left.key.HTTP[:], right.key.HTTP[:]); compared != 0 {
			return compared
		}
		return slices.Compare(left.key.Stream[:], right.key.Stream[:])
	})
	var incomplete []error
	for _, record := range records {
		err := engine.closePendingRecord(ctx, record)
		if generationEngineCleanupIncomplete(err) {
			incomplete = append(incomplete, err)
			continue
		}
		engine.appendCloseError(err)
		engine.closeMu.Lock()
		if engine.closePending[record.key] == record {
			delete(engine.closePending, record.key)
		}
		engine.closeMu.Unlock()
	}
	return errors.Join(incomplete...)
}

func (engine *GenerationEngine) closePendingRecord(ctx context.Context, record *pendingRecord) error {
	engine.mu.Lock()
	if attempt := record.discard; attempt != nil {
		select {
		case <-attempt.done:
			if !attempt.terminal {
				break
			}
			attempt.waiters++
			engine.mu.Unlock()
			return engine.waitDiscardAttempt(ctx, record, attempt, false)
		default:
			attempt.waiters++
			engine.mu.Unlock()
			return engine.waitDiscardAttempt(ctx, record, attempt, false)
		}
	}
	attempt := &discardAttempt{done: make(chan struct{}), waiters: 1}
	record.discard = attempt
	engine.mu.Unlock()
	var discardErr error
	if record.owner != nil {
		discardErr = record.owner.prepared.DiscardPrepared(ctx, record.set)
	}
	engine.mu.Lock()
	attempt.err = discardErr
	attempt.terminal = !generationEngineCleanupIncomplete(discardErr)
	close(attempt.done)
	engine.mu.Unlock()
	return engine.consumeDiscardAttempt(record, attempt)
}

func waitGenerationEngineCloseAttempt(
	ctx context.Context,
	attempt *generationEngineCloseAttempt,
) error {
	select {
	case <-attempt.done:
		return attempt.err
	default:
	}
	select {
	case <-attempt.done:
		return attempt.err
	case <-ctx.Done():
		select {
		case <-attempt.done:
			return attempt.err
		default:
			return ctx.Err()
		}
	}
}

func (engine *GenerationEngine) closePhaseValue() generationEngineClosePhase {
	engine.closeMu.Lock()
	defer engine.closeMu.Unlock()
	return engine.closePhase
}

func (engine *GenerationEngine) setClosePhase(phase generationEngineClosePhase) {
	engine.closeMu.Lock()
	engine.closePhase = phase
	engine.closeMu.Unlock()
}

func (engine *GenerationEngine) appendCloseError(err error) {
	if err == nil {
		return
	}
	engine.closeMu.Lock()
	defer engine.closeMu.Unlock()
	engine.closeErrors = append(engine.closeErrors, err)
}

func (engine *GenerationEngine) closeAttemptResult(transient error) error {
	engine.closeMu.Lock()
	errs := slices.Clone(engine.closeErrors)
	engine.closeMu.Unlock()
	if transient != nil {
		errs = append(errs, transient)
	}
	return errors.Join(errs...)
}

func (engine *GenerationEngine) enqueueRetirementLocked(owner *generationOwner, ctx context.Context) {
	if owner == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
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
	attemptCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	record := &retirementRecord{
		owner: owner, ctx: attemptCtx, cancel: cancel, done: make(chan struct{}),
	}
	engine.retireKnown[owner] = struct{}{}
	engine.retireQueue = append(engine.retireQueue, record)
	engine.signalRetirementChangedLocked()
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
			engine.retireActive[record.owner] = record
			engine.signalRetirementChangedLocked()
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

func (engine *GenerationEngine) retireOwner(record *retirementRecord) {
	defer record.cancel()
	engine.runCheckpoint("before-owner-retirement")
	err := record.owner.closePrepared(record.ctx)
	record.err = err
	terminal := !generationEngineCleanupIncomplete(err)
	if terminal {
		engine.runCheckpoint("owner-terminal")
		engine.unregisterStreamMetricOwner(record.owner)
		engine.runCheckpoint("metrics-unregister")
	}
	engine.retireMu.Lock()
	if engine.retireActive[record.owner] == record {
		delete(engine.retireActive, record.owner)
	}
	if terminal {
		delete(engine.retireKnown, record.owner)
		delete(engine.retireLatest, record.owner)
	} else {
		engine.retireLatest[record.owner] = err
	}
	if terminal && err != nil {
		engine.retireErrors = append(engine.retireErrors, err)
	}
	close(record.done)
	engine.signalRetirementChangedLocked()
	engine.retireMu.Unlock()
}

func (engine *GenerationEngine) signalRetirementChangedLocked() {
	close(engine.retireChanged)
	engine.retireChanged = make(chan struct{})
}

func (engine *GenerationEngine) closeRetirementOwners(ctx context.Context) error {
	joinedErr, err := engine.cancelAndJoinActiveRetirements(ctx)
	if err != nil {
		return errors.Join(joinedErr, err)
	}
	if generationEngineResidualIncomplete(joinedErr) {
		return joinedErr
	}

	engine.retireMu.Lock()
	owners := make([]*generationOwner, 0, len(engine.retireKnown))
	for owner := range engine.retireKnown {
		owners = append(owners, owner)
	}
	engine.retireMu.Unlock()
	var incomplete []error
	for _, owner := range owners {
		ownerErr := owner.closePrepared(ctx)
		if generationEngineCleanupIncomplete(ownerErr) {
			incomplete = append(incomplete, ownerErr)
			continue
		}
		engine.runCheckpoint("owner-terminal")
		engine.unregisterStreamMetricOwner(owner)
		engine.runCheckpoint("metrics-unregister")
		engine.retireMu.Lock()
		delete(engine.retireKnown, owner)
		delete(engine.retireLatest, owner)
		if ownerErr != nil {
			engine.retireErrors = append(engine.retireErrors, ownerErr)
		}
		engine.signalRetirementChangedLocked()
		engine.retireMu.Unlock()
	}
	if len(incomplete) != 0 {
		return errors.Join(incomplete...)
	}

	engine.retireMu.Lock()
	if len(engine.retireKnown) != 0 {
		engine.retireMu.Unlock()
		return compiler.ErrPreparedGenerationCleanupIncomplete
	}
	if !engine.retireStopped {
		engine.retireStopped = true
		close(engine.retireStop)
	}
	retireDone := engine.retireDone
	engine.retireMu.Unlock()
	select {
	case <-retireDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	engine.clearStreamMetrics()
	engine.retireMu.Lock()
	terminalErrors := slices.Clone(engine.retireErrors)
	engine.retireErrors = nil
	engine.retireMu.Unlock()
	for _, terminalErr := range terminalErrors {
		engine.appendCloseError(terminalErr)
	}
	return nil
}

func (engine *GenerationEngine) cancelAndJoinActiveRetirements(
	ctx context.Context,
) (error, error) {
	engine.retireMu.Lock()
	engine.retireClosing = true
	hadAttempts := len(engine.retireQueue) != 0 || len(engine.retireActive) != 0
	joinedOwners := make(map[*generationOwner]struct{}, len(engine.retireQueue)+len(engine.retireActive))
	for _, attempt := range engine.retireQueue {
		joinedOwners[attempt.owner] = struct{}{}
	}
	for _, attempt := range engine.retireActive {
		joinedOwners[attempt.owner] = struct{}{}
		attempt.cancel()
	}
	engine.retireMu.Unlock()
	engine.wakeRetirement()

	for {
		engine.retireMu.Lock()
		for _, attempt := range engine.retireActive {
			joinedOwners[attempt.owner] = struct{}{}
			attempt.cancel()
		}
		if len(engine.retireQueue) == 0 && len(engine.retireActive) == 0 {
			var attemptErrors []error
			if hadAttempts {
				for owner := range joinedOwners {
					if attemptErr := engine.retireLatest[owner]; attemptErr != nil {
						attemptErrors = append(attemptErrors, attemptErr)
						delete(engine.retireLatest, owner)
					}
				}
			}
			engine.retireMu.Unlock()
			return errors.Join(attemptErrors...), nil
		}
		changed := engine.retireChanged
		var attemptErrors []error
		for owner := range joinedOwners {
			if attemptErr := engine.retireLatest[owner]; attemptErr != nil {
				attemptErrors = append(attemptErrors, attemptErr)
			}
		}
		engine.retireMu.Unlock()
		engine.wakeRetirement()
		select {
		case <-changed:
		case <-ctx.Done():
			return errors.Join(attemptErrors...), ctx.Err()
		}
	}
}

func generationEngineCleanupIncomplete(err error) bool {
	if err == nil {
		return false
	}
	var residual *runtime.TaskResidualError
	return errors.As(err, &residual) ||
		errors.Is(err, compiler.ErrPreparedGenerationCleanupIncomplete) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func generationEngineResidualIncomplete(err error) bool {
	if err == nil {
		return false
	}
	var residual *runtime.TaskResidualError
	return errors.As(err, &residual) ||
		errors.Is(err, compiler.ErrPreparedGenerationCleanupIncomplete)
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
