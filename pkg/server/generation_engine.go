package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/wklken/apisix-go/pkg/compiler"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/runtime"
)

var _ generation.PublicationEngine = (*GenerationEngine)(nil)

var errGenerationEngineClosed = errors.New("generation engine is closed")

type retirementRecord struct {
	owner  *generationOwner
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

type generationEngineCloseAttempt struct {
	done chan struct{}
	err  error
}

// GenerationEngine compiles immutable runtime generations, publishes one
// atomic HTTP/stream bundle, and retires replaced owners after their leases
// drain.
type GenerationEngine struct {
	server  *Server
	factory *compiler.WorkerCompilerFactory

	mu         sync.Mutex
	closed     bool
	active     atomic.Pointer[activeBundle]
	checkpoint func(string) error

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

	closeMu             sync.Mutex
	closeAttempt        *generationEngineCloseAttempt
	closeStarted        bool
	closeRetirementDone bool
	closeFactoryDone    bool
	closeErrors         []error
	closeErr            error
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

// Publish compiles a detached candidate and atomically replaces only the
// requested protocol domains. Compilation failure leaves the active bundle
// unchanged.
func (engine *GenerationEngine) Publish(
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
	engine.mu.Lock()
	if engine.closed {
		engine.mu.Unlock()
		return generation.PublicationSet{}, errGenerationEngineClosed
	}
	engine.mu.Unlock()

	if len(ticket.RequiredDomains) == 0 {
		set := generation.PublicationSet{
			DesiredRevision: ticket.DesiredRevision,
			Domains:         map[generation.Domain]generation.PublicationCandidate{},
		}
		if err := generation.ValidatePublicationSet(ticket, set); err != nil {
			return generation.PublicationSet{}, err
		}
		engine.mu.Lock()
		defer engine.mu.Unlock()
		if engine.closed {
			return generation.PublicationSet{}, errGenerationEngineClosed
		}
		return set, nil
	}

	prepared, err := engine.factory.PrepareGeneration(
		ctx, ticket, desired, previous, engine.onTaskFailure,
	)
	if err != nil {
		return generation.PublicationSet{}, err
	}
	set := prepared.PublicationSet()
	cleanup := func(primary error) (generation.PublicationSet, error) {
		return generation.PublicationSet{}, errors.Join(
			primary,
			prepared.DiscardPrepared(context.WithoutCancel(ctx), set),
		)
	}
	if err := generation.ValidatePublicationSet(ticket, set); err != nil {
		return cleanup(err)
	}
	domains, err := ownerDomainsFromSet(set)
	if err != nil {
		return cleanup(err)
	}
	owner := newGenerationOwner(prepared)
	if domains&ownerDomainHTTP != 0 && prepared.HTTP() == nil {
		return cleanup(generation.ErrIntegrity)
	}
	if domains&ownerDomainStream != 0 && prepared.Stream() == nil {
		return cleanup(generation.ErrIntegrity)
	}

	engine.mu.Lock()
	if engine.closed {
		engine.mu.Unlock()
		return cleanup(errGenerationEngineClosed)
	}
	predecessor := engine.active.Load()
	if predecessor == nil {
		engine.mu.Unlock()
		return cleanup(generation.ErrIntegrity)
	}
	candidate := predecessor.withDomains(owner, domains)
	owner.activateDomains(domains)
	engine.active.Store(&candidate)
	if engine.checkpoint != nil {
		if err := engine.checkpoint("candidate-bundle-published"); err != nil {
			engine.active.Store(predecessor)
			owner.deactivateDomains(domains)
			engine.mu.Unlock()
			return cleanup(err)
		}
	}
	if domains&ownerDomainStream != 0 {
		engine.registerStreamMetricOwner(owner)
	}
	retire := replacedOwners(predecessor, owner, domains)
	for replaced, replacedDomains := range retire {
		if replaced.deactivateDomains(replacedDomains) {
			engine.enqueueRetirementLocked(replaced, context.WithoutCancel(ctx))
		}
	}
	engine.mu.Unlock()
	engine.wakeRetirement()
	return cloneEnginePublicationSet(set), nil
}

func replacedOwners(
	previous *activeBundle,
	candidate *generationOwner,
	domains ownerDomain,
) map[*generationOwner]ownerDomain {
	replaced := make(map[*generationOwner]ownerDomain, 2)
	if domains&ownerDomainHTTP != 0 && previous.http != nil && previous.http != candidate {
		replaced[previous.http] |= ownerDomainHTTP
	}
	if domains&ownerDomainStream != 0 && previous.stream != nil && previous.stream != candidate {
		replaced[previous.stream] |= ownerDomainStream
	}
	return replaced
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

func cloneEnginePublicationSet(set generation.PublicationSet) generation.PublicationSet {
	clone := generation.PublicationSet{
		DesiredRevision: set.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(set.Domains)),
	}
	for domain, candidate := range set.Domains {
		candidate.Snapshot = candidate.Snapshot.Clone()
		candidate.Closure = append([]generation.ResourceKey(nil), candidate.Closure...)
		candidate.Decisions = append([]generation.ResourceDecision(nil), candidate.Decisions...)
		clone.Domains[domain] = candidate
	}
	return clone
}

func (*GenerationEngine) onTaskFailure(runtime.TaskFailure) {}

func (engine *GenerationEngine) Close(ctx context.Context) error {
	if engine == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	engine.closeMu.Lock()
	if engine.closeFactoryDone {
		err := engine.closeErr
		engine.closeMu.Unlock()
		return err
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
	if engine.closeFactoryDone {
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
	engine.closeMu.Lock()
	started := engine.closeStarted
	engine.closeMu.Unlock()
	if !started {
		engine.captureCloseOwner(ctx)
	}
	engine.closeMu.Lock()
	retirementDone := engine.closeRetirementDone
	engine.closeMu.Unlock()
	if !retirementDone {
		if err := engine.closeRetirementOwners(ctx); err != nil {
			return engine.closeAttemptResult(err)
		}
		engine.closeMu.Lock()
		engine.closeRetirementDone = true
		engine.closeMu.Unlock()
	}
	engine.closeMu.Lock()
	factoryDone := engine.closeFactoryDone
	engine.closeMu.Unlock()
	if !factoryDone {
		engine.runCheckpoint("factory-close")
		factoryErr := engine.factory.Close(ctx)
		if generationEngineCleanupIncomplete(factoryErr) {
			return engine.closeAttemptResult(factoryErr)
		}
		engine.appendCloseError(factoryErr)
		engine.closeMu.Lock()
		engine.closeFactoryDone = true
		engine.closeMu.Unlock()
	}
	return engine.closeAttemptResult(nil)
}

func (engine *GenerationEngine) captureCloseOwner(ctx context.Context) {
	engine.mu.Lock()
	engine.closed = true
	bundle := engine.active.Load()
	engine.active.Store(&activeBundle{})
	owners := make(map[*generationOwner]struct{}, 2)
	if bundle != nil && bundle.http != nil {
		owners[bundle.http] = struct{}{}
	}
	if bundle != nil && bundle.stream != nil {
		owners[bundle.stream] = struct{}{}
	}
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
	engine.closeStarted = true
	engine.closeMu.Unlock()
	engine.wakeRetirement()
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

func (engine *GenerationEngine) appendCloseError(err error) {
	if err == nil {
		return
	}
	engine.closeMu.Lock()
	engine.closeErrors = append(engine.closeErrors, err)
	engine.closeMu.Unlock()
}

func (engine *GenerationEngine) closeAttemptResult(transient error) error {
	engine.closeMu.Lock()
	errs := append([]error(nil), engine.closeErrors...)
	engine.closeMu.Unlock()
	if transient != nil {
		errs = append(errs, transient)
	}
	return errors.Join(errs...)
}

func (engine *GenerationEngine) acquireHTTP() (httpGenerationLease, bool) {
	if engine == nil {
		return httpGenerationLease{}, false
	}
	for {
		engine.mu.Lock()
		if engine.closed {
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
		if engine.closed {
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

func (engine *GenerationEngine) runCheckpoint(stage string) {
	engine.mu.Lock()
	checkpoint := engine.checkpoint
	engine.mu.Unlock()
	if checkpoint != nil {
		_ = checkpoint(stage)
	}
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
	var joinedContextErrors []error
	if errors.Is(joinedErr, context.Canceled) {
		joinedContextErrors = append(joinedContextErrors, context.Canceled)
	}
	if errors.Is(joinedErr, context.DeadlineExceeded) {
		joinedContextErrors = append(joinedContextErrors, context.DeadlineExceeded)
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
		return errors.Join(errors.Join(joinedContextErrors...), errors.Join(incomplete...))
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
	terminalErrors := append([]error(nil), engine.retireErrors...)
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
