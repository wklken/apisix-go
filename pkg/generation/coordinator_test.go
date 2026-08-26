package generation

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errCoordinatorPublishedLoad = errors.New("load committed publication failed")

func TestCoordinatorAcknowledgesOnlyAfterCommitAndFinalize(t *testing.T) {
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	engine := newCoordinatorFakeEngine(recorder)

	ack, err := NewCoordinator(journal, engine).Apply(context.Background(), desiredHTTPBatch("etcd", "61"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"load-acknowledgement", "apply-desired", "load-desired", "load-published:http",
		"prepare", "stage", "activate", "commit", "finalize",
	}
	if got := recorder.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if ack.Cursor.Revision != "61" || engine.finalizeCalls != 1 {
		t.Fatalf("ack/finalize = %+v/%d", ack, engine.finalizeCalls)
	}
}

func TestCoordinatorStopsAtEachPreStageFailure(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*coordinatorFakeJournal, *coordinatorFakeEngine, error)
		want  []string
	}{
		{
			name: "apply desired",
			setup: func(j *coordinatorFakeJournal, _ *coordinatorFakeEngine, wantErr error) {
				j.applyErr = wantErr
			},
			want: []string{"load-acknowledgement", "apply-desired"},
		},
		{
			name: "load acknowledgement",
			setup: func(j *coordinatorFakeJournal, _ *coordinatorFakeEngine, wantErr error) {
				j.ackErr = wantErr
			},
			want: []string{"load-acknowledgement"},
		},
		{
			name: "load desired",
			setup: func(j *coordinatorFakeJournal, _ *coordinatorFakeEngine, wantErr error) {
				j.loadDesiredErr = wantErr
			},
			want: []string{"load-acknowledgement", "apply-desired", "load-desired"},
		},
		{
			name: "load published",
			setup: func(j *coordinatorFakeJournal, _ *coordinatorFakeEngine, wantErr error) {
				j.loadPublishedErr = wantErr
			},
			want: []string{
				"load-acknowledgement", "apply-desired", "load-desired", "load-published:http",
			},
		},
		{
			name: "prepare",
			setup: func(_ *coordinatorFakeJournal, e *coordinatorFakeEngine, wantErr error) {
				e.prepareErr = wantErr
			},
			want: []string{
				"load-acknowledgement", "apply-desired", "load-desired", "load-published:http", "prepare",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &callRecorder{}
			journal := newCoordinatorFakeJournal(t, recorder)
			engine := newCoordinatorFakeEngine(recorder)
			wantErr := errors.New(test.name + " failed")
			test.setup(journal, engine, wantErr)
			_, err := NewCoordinator(journal, engine).Apply(
				context.Background(),
				desiredHTTPBatch("etcd", "pre-stage"),
			)
			if !errors.Is(err, wantErr) {
				t.Fatalf("Apply() error = %v, want %v", err, wantErr)
			}
			if got := recorder.snapshot(); !slices.Equal(got, test.want) {
				t.Fatalf("calls = %v, want %v", got, test.want)
			}
			if engine.discardCalls != 0 {
				t.Fatalf("discard calls = %d, want 0", engine.discardCalls)
			}
		})
	}
}

func TestCoordinatorStageFailureDiscardsWithCleanupContext(t *testing.T) {
	type contextKey string
	const key contextKey = "cleanup-key"
	stageErr := errors.New("stage failed")
	discardErr := errors.New("discard failed")
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	journal.stageErr = stageErr
	engine := newCoordinatorFakeEngine(recorder)
	engine.discardErr = discardErr
	engine.contextKey = key
	engine.contextValue = "cleanup-value"
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key, "cleanup-value"))
	journal.stageHook = cancel

	_, err := NewCoordinator(journal, engine).Apply(ctx, desiredHTTPBatch("etcd", "stage"))
	if !errors.Is(err, stageErr) || !errors.Is(err, discardErr) {
		t.Fatalf("Apply() error = %v, want joined stage/discard errors", err)
	}
	if engine.discardCalls != 1 || !engine.cleanupContextOK {
		t.Fatalf("discard calls/context = %d/%t, want 1/true", engine.discardCalls, engine.cleanupContextOK)
	}
}

func TestCoordinatorActivationAndCommitFailuresRollbackThenAbort(t *testing.T) {
	for _, phase := range []string{"activation", "commit"} {
		t.Run(phase, func(t *testing.T) {
			type contextKey string
			const key contextKey = "cleanup-key"
			primaryErr := errors.New(phase + " failed")
			rollbackErr := errors.New("rollback failed")
			abortErr := errors.New("abort failed")
			recorder := &callRecorder{}
			journal := newCoordinatorFakeJournal(t, recorder)
			engine := newCoordinatorFakeEngine(recorder)
			engine.rollbackErr = rollbackErr
			engine.contextKey = key
			engine.contextValue = "cleanup-value"
			journal.abortErr = abortErr
			journal.contextKey = key
			journal.contextValue = "cleanup-value"
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key, "cleanup-value"))
			if phase == "activation" {
				engine.activateErr = primaryErr
				engine.activateHook = cancel
			} else {
				journal.commitErr = primaryErr
				journal.commitHook = cancel
			}

			_, err := NewCoordinator(journal, engine).Apply(
				ctx,
				desiredHTTPBatch("etcd", phase),
			)
			for _, wantErr := range []error{primaryErr, rollbackErr, abortErr} {
				if !errors.Is(err, wantErr) {
					t.Fatalf("Apply() error = %v, want joined %v", err, wantErr)
				}
			}
			wantTail := []string{"activate", "rollback", "abort"}
			if phase == "commit" {
				wantTail = []string{"activate", "commit", "rollback", "abort"}
			}
			got := recorder.snapshot()
			if len(got) < len(wantTail) || !slices.Equal(got[len(got)-len(wantTail):], wantTail) {
				t.Fatalf("calls = %v, want tail %v", got, wantTail)
			}
			if engine.finalizeCalls != 0 || journal.abortReason != phase+"-failed" {
				t.Fatalf("finalize/reason = %d/%q", engine.finalizeCalls, journal.abortReason)
			}
			if !engine.rollbackContextOK || !journal.abortContextOK {
				t.Fatalf("rollback/abort cleanup context = %t/%t", engine.rollbackContextOK, journal.abortContextOK)
			}
		})
	}
}

func TestCoordinatorCommittedReplayConfirmsExactActiveSet(t *testing.T) {
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	committed := publishedForTicket(journal.ticket, DomainHTTP)
	journal.published[DomainHTTP] = committed
	journal.ackErr = nil
	journal.ack = acknowledgementForTicket(journal.ticket, committed)
	engine := newCoordinatorFakeEngine(recorder)

	ack, err := NewCoordinator(journal, engine).Apply(context.Background(), desiredHTTPBatch("etcd", "61"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"load-acknowledgement", "load-published:http", "confirm-active"}
	if got := recorder.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if ack.Cursor != journal.ticket.Cursor || engine.confirmed.DesiredRevision != journal.ticket.DesiredRevision {
		t.Fatalf("ack/confirmed = %+v/%+v", ack, engine.confirmed)
	}
	candidate := engine.confirmed.Domains[DomainHTTP]
	if candidate.Artifact != committed.Artifact || candidate.Snapshot.SnapshotID() != committed.Snapshot.SnapshotID() {
		t.Fatalf("confirmed candidate = %+v, want %+v", candidate, committed)
	}
}

func TestCoordinatorCommittedReplayIgnoresConflictingPayloadForSameCursor(t *testing.T) {
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	firstEngine := newCoordinatorFakeEngine(recorder)
	committedAck, err := NewCoordinator(journal, firstEngine).Apply(
		context.Background(),
		desiredHTTPBatch("etcd", "61"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if firstEngine.finalizeCalls != 1 {
		t.Fatalf("original finalize calls = %d, want 1", firstEngine.finalizeCalls)
	}
	committed := publishedForTicket(journal.ticket, DomainHTTP)
	journal.published[DomainHTTP] = committed
	journal.ackErr = nil
	journal.ack = committedAck
	recorder = &callRecorder{}
	journal.recorder = recorder
	engine := newCoordinatorFakeEngine(recorder)
	incoming := DesiredBatch{
		Cursor:         journal.ticket.Cursor,
		ReplaceManaged: true,
		Mutations: []Mutation{{
			Type:  MutationPut,
			Key:   ResourceKey{Kind: "stream_routes", ID: "different"},
			Value: []byte(`{"id":"different"}`),
		}},
		RequiredDomains: []Domain{DomainStream},
	}

	applyCallsBeforeReplay := journal.applyCalls
	ack, err := NewCoordinator(journal, engine).Apply(context.Background(), incoming)
	if err != nil {
		t.Fatal(err)
	}
	if got := recorder.snapshot(); !slices.Equal(got, []string{
		"load-acknowledgement", "load-published:http", "confirm-active",
	}) {
		t.Fatalf("calls = %v", got)
	}
	if journal.loadedCursor != incoming.Cursor || journal.applyCalls != applyCallsBeforeReplay ||
		ack.Cursor != journal.ack.Cursor ||
		!slices.Equal(ack.Decisions[DomainHTTP], journal.ack.Decisions[DomainHTTP]) {
		t.Fatalf("loaded cursor/apply calls/ack = %+v/%d/%+v, want %+v/%d/%+v",
			journal.loadedCursor, journal.applyCalls, ack,
			incoming.Cursor, applyCallsBeforeReplay, journal.ack)
	}
}

func TestCoordinatorUncommittedConflictingCursorStillUsesApplyDesired(t *testing.T) {
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	journal.applyErr = ErrCursorConflict
	engine := newCoordinatorFakeEngine(recorder)

	_, err := NewCoordinator(journal, engine).Apply(context.Background(), DesiredBatch{
		Cursor:         journal.ticket.Cursor,
		ReplaceManaged: true,
		Mutations: []Mutation{{
			Type: MutationDelete,
			Key:  ResourceKey{Kind: "routes", ID: "different"},
		}},
		RequiredDomains: []Domain{DomainHTTP},
	})
	if !errors.Is(err, ErrCursorConflict) {
		t.Fatalf("Apply() error = %v, want ErrCursorConflict", err)
	}
	if got := recorder.snapshot(); !slices.Equal(got, []string{
		"load-acknowledgement", "apply-desired",
	}) {
		t.Fatalf("calls = %v", got)
	}
}

func TestCoordinatorZeroDomainPublicationUsesSyntheticLifecycle(t *testing.T) {
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	journal.ticket.RequiredDomains = nil
	engine := newCoordinatorFakeEngine(recorder)
	batch := DesiredBatch{Cursor: journal.ticket.Cursor}

	ack, err := NewCoordinator(journal, engine).Apply(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"load-acknowledgement", "apply-desired", "load-desired", "prepare",
		"stage", "activate", "commit", "finalize",
	}
	if got := recorder.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if len(ack.Decisions) != 0 || ack.Revisions != (RevisionSet{Desired: journal.ticket.DesiredRevision}) {
		t.Fatalf("ack = %+v, want zero-domain desired revision", ack)
	}
	if engine.compiledDomains != 0 || len(engine.prepared.Domains) != 0 ||
		len(engine.activated.Domains) != 0 || len(engine.finalized.Domains) != 0 {
		t.Fatalf("compiled/prepared/activated/finalized = %d/%+v/%+v/%+v, want synthetic empty sets",
			engine.compiledDomains, engine.prepared, engine.activated, engine.finalized)
	}
}

func TestCoordinatorZeroDomainCommitFailureRetriesWithoutDomainWork(t *testing.T) {
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	journal.ticket.RequiredDomains = nil
	commitErr := errors.New("commit failed")
	journal.commitErr = commitErr
	engine := newCoordinatorFakeEngine(recorder)
	coordinator := NewCoordinator(journal, engine)
	batch := DesiredBatch{Cursor: journal.ticket.Cursor}

	if _, err := coordinator.Apply(context.Background(), batch); !errors.Is(err, commitErr) {
		t.Fatalf("first Apply() error = %v, want %v", err, commitErr)
	}
	if engine.finalizeCalls != 0 || engine.rollbackCalls != 1 || engine.compiledDomains != 0 {
		t.Fatalf("first finalize/rollback/compiled = %d/%d/%d, want 0/1/0",
			engine.finalizeCalls, engine.rollbackCalls, engine.compiledDomains)
	}
	journal.commitErr = nil
	ack, err := coordinator.Apply(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(ack.Decisions) != 0 || engine.finalizeCalls != 1 || engine.compiledDomains != 0 ||
		len(engine.activated.Domains) != 0 || len(engine.finalized.Domains) != 0 {
		t.Fatalf("retry ack/finalize/compiled/activated/finalized = %+v/%d/%d/%+v/%+v",
			ack, engine.finalizeCalls, engine.compiledDomains, engine.activated, engine.finalized)
	}
}

func TestCoordinatorZeroDomainStageFailureDiscardsSyntheticPreparation(t *testing.T) {
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	journal.ticket.RequiredDomains = nil
	stageErr := errors.New("stage failed")
	journal.stageErr = stageErr
	engine := newCoordinatorFakeEngine(recorder)

	_, err := NewCoordinator(journal, engine).Apply(
		context.Background(),
		DesiredBatch{Cursor: journal.ticket.Cursor},
	)
	if !errors.Is(err, stageErr) {
		t.Fatalf("Apply() error = %v, want %v", err, stageErr)
	}
	if engine.discardCalls != 1 || engine.compiledDomains != 0 ||
		len(engine.prepared.Domains) != 0 || engine.finalizeCalls != 0 {
		t.Fatalf("discard/compiled/prepared/finalize = %d/%d/%+v/%d, want 1/0/empty/0",
			engine.discardCalls, engine.compiledDomains, engine.prepared, engine.finalizeCalls)
	}
}

func TestCoordinatorAcknowledgementLookupErrorsNeverFallBack(t *testing.T) {
	for _, wantErr := range []error{ErrStaleCursor, ErrIntegrity, context.Canceled} {
		t.Run(wantErr.Error(), func(t *testing.T) {
			recorder := &callRecorder{}
			journal := newCoordinatorFakeJournal(t, recorder)
			journal.ackErr = wantErr

			_, err := NewCoordinator(journal, newCoordinatorFakeEngine(recorder)).Apply(
				context.Background(),
				desiredHTTPBatch("etcd", "61"),
			)
			if !errors.Is(err, wantErr) {
				t.Fatalf("Apply() error = %v, want %v", err, wantErr)
			}
			if got := recorder.snapshot(); !slices.Equal(got, []string{"load-acknowledgement"}) {
				t.Fatalf("calls = %v", got)
			}
		})
	}
}

func TestCoordinatorCommittedReplayNeverFallsBack(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*coordinatorFakeJournal, *coordinatorFakeEngine)
		want  error
	}{
		{
			name: "missing required publication",
			setup: func(j *coordinatorFakeJournal, _ *coordinatorFakeEngine) {
				j.ackErr = nil
				j.ack = acknowledgementForTicket(j.ticket, publishedForTicket(j.ticket, DomainHTTP))
			},
			want: ErrIntegrity,
		},
		{
			name: "wrong publication revision",
			setup: func(j *coordinatorFakeJournal, _ *coordinatorFakeEngine) {
				j.ackErr = nil
				published := publishedForTicket(j.ticket, DomainHTTP)
				j.ack = acknowledgementForTicket(j.ticket, published)
				published.Artifact.Revision++
				j.published[DomainHTTP] = published
			},
			want: ErrIntegrity,
		},
		{
			name: "revision without decision domain",
			setup: func(j *coordinatorFakeJournal, _ *coordinatorFakeEngine) {
				j.ackErr = nil
				j.ack = Acknowledgement{
					Cursor: j.ticket.Cursor,
					Revisions: RevisionSet{
						Desired: j.ticket.DesiredRevision,
						HTTP:    j.ticket.DesiredRevision,
					},
					Decisions: map[Domain][]ResourceDecision{},
				}
			},
			want: ErrIntegrity,
		},
		{
			name: "active mismatch",
			setup: func(j *coordinatorFakeJournal, e *coordinatorFakeEngine) {
				j.ackErr = nil
				published := publishedForTicket(j.ticket, DomainHTTP)
				j.published[DomainHTTP] = published
				j.ack = acknowledgementForTicket(j.ticket, published)
				e.confirmErr = ErrActiveGenerationMismatch
			},
			want: ErrActiveGenerationMismatch,
		},
		{
			name: "publication load I/O failure",
			setup: func(j *coordinatorFakeJournal, _ *coordinatorFakeEngine) {
				j.ackErr = nil
				published := publishedForTicket(j.ticket, DomainHTTP)
				j.published[DomainHTTP] = published
				j.ack = acknowledgementForTicket(j.ticket, published)
				j.loadPublishedErr = errCoordinatorPublishedLoad
			},
			want: errCoordinatorPublishedLoad,
		},
		{
			name: "publication load canceled",
			setup: func(j *coordinatorFakeJournal, _ *coordinatorFakeEngine) {
				j.ackErr = nil
				published := publishedForTicket(j.ticket, DomainHTTP)
				j.published[DomainHTTP] = published
				j.ack = acknowledgementForTicket(j.ticket, published)
				j.loadPublishedErr = context.Canceled
			},
			want: context.Canceled,
		},
		{
			name: "confirm canceled",
			setup: func(j *coordinatorFakeJournal, e *coordinatorFakeEngine) {
				j.ackErr = nil
				published := publishedForTicket(j.ticket, DomainHTTP)
				j.published[DomainHTTP] = published
				j.ack = acknowledgementForTicket(j.ticket, published)
				e.confirmErr = context.Canceled
			},
			want: context.Canceled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &callRecorder{}
			journal := newCoordinatorFakeJournal(t, recorder)
			engine := newCoordinatorFakeEngine(recorder)
			test.setup(journal, engine)
			_, err := NewCoordinator(journal, engine).Apply(
				context.Background(),
				desiredHTTPBatch("etcd", "61"),
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("Apply() error = %v, want %v", err, test.want)
			}
			for _, call := range recorder.snapshot() {
				if slices.Contains([]string{"prepare", "stage", "activate", "commit", "finalize"}, call) {
					t.Fatalf("committed replay fell back through %q", call)
				}
			}
		})
	}
}

func TestCoordinatorCommittedReplayReturnsDefensiveValues(t *testing.T) {
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	published := publishedForTicket(journal.ticket, DomainHTTP)
	journal.published[DomainHTTP] = published
	journal.ackErr = nil
	journal.ack = acknowledgementForTicket(journal.ticket, published)
	engine := newCoordinatorFakeEngine(recorder)
	engine.confirmHook = func(set PublicationSet) {
		candidate := set.Domains[DomainHTTP]
		candidate.Closure[0].ID = "mutated-by-engine"
		candidate.Decisions[0].Code = "mutated-by-engine"
		set.Domains[DomainHTTP] = candidate
	}

	ack, err := NewCoordinator(journal, engine).Apply(context.Background(), desiredHTTPBatch("etcd", "61"))
	if err != nil {
		t.Fatal(err)
	}
	ack.Decisions[DomainHTTP][0].Code = "mutated-by-caller"
	delete(ack.Decisions, DomainHTTP)
	if got := journal.ack.Decisions[DomainHTTP][0].Code; got != "test-published" {
		t.Fatalf("stored acknowledgement code = %q", got)
	}
	if got := journal.published[DomainHTTP].Closure[0].ID; got != "r1" {
		t.Fatalf("stored publication closure ID = %q", got)
	}
	if got := journal.published[DomainHTTP].Decisions[0].Code; got != "test-published" {
		t.Fatalf("stored publication decision code = %q", got)
	}
}

func TestCoordinatorMissingMarkerRejectsSameRevisionHeadWithoutLifecycle(t *testing.T) {
	for _, revisionDelta := range []uint64{0, 1} {
		t.Run(map[bool]string{true: "same", false: "future"}[revisionDelta == 0], func(t *testing.T) {
			recorder := &callRecorder{}
			journal := newCoordinatorFakeJournal(t, recorder)
			published := publishedForTicket(journal.ticket, DomainHTTP)
			published.Artifact.Revision += revisionDelta
			journal.published[DomainHTTP] = published
			engine := newCoordinatorFakeEngine(recorder)

			_, err := NewCoordinator(journal, engine).Apply(
				context.Background(),
				desiredHTTPBatch("etcd", "61"),
			)
			if !errors.Is(err, ErrIntegrity) {
				t.Fatalf("Apply() error = %v, want ErrIntegrity", err)
			}
			want := []string{
				"load-acknowledgement", "apply-desired", "load-desired", "load-published:http",
			}
			if got := recorder.snapshot(); !slices.Equal(got, want) {
				t.Fatalf("calls = %v, want %v", got, want)
			}
		})
	}
}

func TestCoordinatorCommittedZeroDomainReplayStillConfirmsFence(t *testing.T) {
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	journal.ticket.RequiredDomains = nil
	journal.ackErr = nil
	journal.ack = Acknowledgement{
		Cursor: journal.ticket.Cursor, Revisions: RevisionSet{Desired: journal.ticket.DesiredRevision},
		Decisions: map[Domain][]ResourceDecision{},
	}
	engine := newCoordinatorFakeEngine(recorder)

	if _, err := NewCoordinator(journal, engine).Apply(context.Background(), DesiredBatch{
		Cursor: journal.ticket.Cursor,
	}); err != nil {
		t.Fatal(err)
	}
	if got := recorder.snapshot(); !slices.Equal(got, []string{
		"load-acknowledgement", "confirm-active",
	}) {
		t.Fatalf("calls = %v", got)
	}
	if engine.confirmCalls != 1 || len(engine.confirmed.Domains) != 0 {
		t.Fatalf("confirm calls/set = %d/%+v", engine.confirmCalls, engine.confirmed)
	}
}

func TestCoordinatorConcurrentApplySerializesAndCanceledWaiterStopsAtLock(t *testing.T) {
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	engine := newCoordinatorFakeEngine(recorder)
	engine.prepareEntered = make(chan struct{}, 1)
	engine.prepareRelease = make(chan struct{})
	coordinator := NewCoordinator(journal, engine)

	firstDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Apply(context.Background(), desiredHTTPBatch("etcd", "first"))
		firstDone <- err
	}()
	select {
	case <-engine.prepareEntered:
	case <-time.After(time.Second):
		t.Fatal("first Apply did not enter Prepare")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	secondDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Apply(ctx, desiredHTTPBatch("etcd", "second"))
		secondDone <- err
	}()
	close(engine.prepareRelease)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("second Apply() error = %v, want context.Canceled", err)
	}
	if journal.applyCalls != 1 || engine.maxPrepare.Load() != 1 {
		t.Fatalf("apply calls/max prepare = %d/%d, want 1/1", journal.applyCalls, engine.maxPrepare.Load())
	}
}

func TestCoordinatorConcurrentSuccessfulApplyHasOneLifecycleAtATime(t *testing.T) {
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	engine := newCoordinatorFakeEngine(recorder)
	engine.prepareEntered = make(chan struct{}, 2)
	engine.prepareRelease = make(chan struct{})
	coordinator := NewCoordinator(journal, engine)

	done := make(chan error, 2)
	go func() {
		_, err := coordinator.Apply(context.Background(), desiredHTTPBatch("etcd", "first"))
		done <- err
	}()
	select {
	case <-engine.prepareEntered:
	case <-time.After(time.Second):
		t.Fatal("first Apply did not enter Prepare")
	}
	go func() {
		_, err := coordinator.Apply(context.Background(), desiredHTTPBatch("etcd", "second"))
		done <- err
	}()
	select {
	case <-engine.prepareEntered:
		t.Fatal("second Apply entered Prepare before first lifecycle completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(engine.prepareRelease)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if engine.prepareCalls != 2 || engine.maxPrepare.Load() != 1 {
		t.Fatalf("prepare calls/max = %d/%d, want 2/1", engine.prepareCalls, engine.maxPrepare.Load())
	}
}

func TestCoordinatorUncommittedCursorCanRetry(t *testing.T) {
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	engine := newCoordinatorFakeEngine(recorder)
	engine.activateErr = errors.New("first activation failed")
	coordinator := NewCoordinator(journal, engine)
	batch := desiredHTTPBatch("etcd", "retry")
	if _, err := coordinator.Apply(context.Background(), batch); err == nil {
		t.Fatal("first Apply() unexpectedly succeeded")
	}
	engine.activateErr = nil
	if _, err := coordinator.Apply(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if journal.applyCalls != 2 || engine.prepareCalls != 2 || journal.commitCalls != 1 {
		t.Fatalf("apply/prepare/commit = %d/%d/%d, want 2/2/1",
			journal.applyCalls, engine.prepareCalls, journal.commitCalls)
	}
}

func TestCoordinatorFinalizePanicPropagatesAfterCommit(t *testing.T) {
	recorder := &callRecorder{}
	journal := newCoordinatorFakeJournal(t, recorder)
	engine := newCoordinatorFakeEngine(recorder)
	engine.finalizePanic = "fatal finalize invariant"
	defer func() {
		if recovered := recover(); recovered != engine.finalizePanic {
			t.Fatalf("recovered = %v, want %v", recovered, engine.finalizePanic)
		}
		if journal.commitCalls != 1 || journal.abortCalls != 0 || engine.rollbackCalls != 0 {
			t.Fatalf("commit/abort/rollback = %d/%d/%d", journal.commitCalls, journal.abortCalls, engine.rollbackCalls)
		}
	}()
	_, _ = NewCoordinator(journal, engine).Apply(context.Background(), desiredHTTPBatch("etcd", "panic"))
}

func TestStableAbortCodeIsBoundedAndSanitized(t *testing.T) {
	tests := []struct {
		phase string
		err   error
		want  string
	}{
		{phase: "activation", err: context.Canceled, want: "activation-context-canceled"},
		{phase: "commit", err: context.DeadlineExceeded, want: "commit-deadline-exceeded"},
		{phase: "commit", err: errors.New("secret=do-not-persist"), want: "commit-failed"},
	}
	for _, test := range tests {
		if got := stableAbortCode(test.phase, test.err); got != test.want || len(got) > 128 {
			t.Fatalf("stableAbortCode(%q, %v) = %q, want %q", test.phase, test.err, got, test.want)
		}
	}
}

type callRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *callRecorder) add(call string) {
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
}

func (r *callRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

type coordinatorFakeJournal struct {
	recorder  *callRecorder
	desired   Snapshot
	ticket    ApplyTicket
	ack       Acknowledgement
	ackErr    error
	published map[Domain]PublishedGeneration

	applyErr         error
	loadDesiredErr   error
	loadPublishedErr error
	stageErr         error
	commitErr        error
	abortErr         error
	stageHook        func()
	commitHook       func()
	contextKey       any
	contextValue     any
	abortContextOK   bool
	loadedCursor     ProviderCursor

	applyCalls  int
	commitCalls int
	abortCalls  int
	abortReason string
}

func newCoordinatorFakeJournal(t *testing.T, recorder *callRecorder) *coordinatorFakeJournal {
	t.Helper()
	desired, err := NewSnapshot(1, []Resource{{
		Key: ResourceKey{Kind: "routes", ID: "r1"}, Value: []byte(`{"id":"r1"}`),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &coordinatorFakeJournal{
		recorder: recorder,
		desired:  desired,
		ticket: ApplyTicket{
			DesiredRevision: 1,
			DesiredDigest:   desired.Digest(),
			Cursor:          ProviderCursor{Provider: "etcd", Revision: "61"},
			RequiredDomains: []Domain{DomainHTTP},
		},
		ackErr:    ErrNotFound,
		published: make(map[Domain]PublishedGeneration),
	}
}

func (j *coordinatorFakeJournal) ApplyDesired(_ context.Context, batch DesiredBatch) (ApplyTicket, error) {
	j.recorder.add("apply-desired")
	j.applyCalls++
	if j.applyErr != nil {
		return ApplyTicket{}, j.applyErr
	}
	ticket := cloneCoordinatorTicket(j.ticket)
	ticket.Cursor = batch.Cursor
	j.ticket.Cursor = batch.Cursor
	if batch.RequiredDomains != nil {
		ticket.RequiredDomains = slices.Clone(batch.RequiredDomains)
		j.ticket.RequiredDomains = slices.Clone(batch.RequiredDomains)
	}
	return ticket, nil
}

func (j *coordinatorFakeJournal) LoadAcknowledgement(
	_ context.Context,
	cursor ProviderCursor,
) (Acknowledgement, error) {
	j.recorder.add("load-acknowledgement")
	j.loadedCursor = cursor
	return cloneCoordinatorAcknowledgement(j.ack), j.ackErr
}

func (j *coordinatorFakeJournal) LoadDesired(context.Context, uint64) (Snapshot, error) {
	j.recorder.add("load-desired")
	return j.desired.Clone(), j.loadDesiredErr
}

func (j *coordinatorFakeJournal) LoadPublished(_ context.Context, domain Domain) (PublishedGeneration, error) {
	j.recorder.add("load-published:" + string(domain))
	if j.loadPublishedErr != nil {
		return PublishedGeneration{}, j.loadPublishedErr
	}
	published, found := j.published[domain]
	if !found {
		return PublishedGeneration{}, ErrNotFound
	}
	return cloneCoordinatorPublished(published), nil
}

func (j *coordinatorFakeJournal) Stage(_ context.Context, _ ApplyTicket, _ PublicationSet) (PublicationToken, error) {
	j.recorder.add("stage")
	if j.stageHook != nil {
		j.stageHook()
	}
	if j.stageErr != nil {
		return "", j.stageErr
	}
	return PublicationToken("test-token"), nil
}

func (j *coordinatorFakeJournal) Commit(context.Context, PublicationToken) (Acknowledgement, error) {
	j.recorder.add("commit")
	j.commitCalls++
	if j.commitHook != nil {
		j.commitHook()
	}
	if j.commitErr != nil {
		return Acknowledgement{}, j.commitErr
	}
	ack := Acknowledgement{
		Cursor: j.ticket.Cursor, Revisions: RevisionSet{Desired: j.ticket.DesiredRevision},
		Decisions: make(map[Domain][]ResourceDecision, len(j.ticket.RequiredDomains)),
	}
	if slices.Contains(j.ticket.RequiredDomains, DomainHTTP) {
		ack.Revisions.HTTP = j.ticket.DesiredRevision
		ack.Decisions[DomainHTTP] = []ResourceDecision{{
			Key:         ResourceKey{Kind: "routes", ID: "r1"},
			Disposition: DispositionPublished, Code: "test-published",
		}}
	}
	return ack, nil
}

func (j *coordinatorFakeJournal) Abort(ctx context.Context, _ PublicationToken, reason string) error {
	j.recorder.add("abort")
	j.abortCalls++
	j.abortReason = reason
	j.abortContextOK = ctx.Err() == nil &&
		(j.contextKey == nil || ctx.Value(j.contextKey) == j.contextValue)
	return j.abortErr
}

func (j *coordinatorFakeJournal) Revisions(context.Context) (RevisionSet, error) {
	return RevisionSet{Desired: j.ticket.DesiredRevision}, nil
}

func (j *coordinatorFakeJournal) Recover(context.Context) (RecoveryState, error) {
	return RecoveryState{}, nil
}

func (j *coordinatorFakeJournal) Close() error { return nil }

type coordinatorFakeEngine struct {
	recorder *callRecorder

	prepareErr    error
	discardErr    error
	activateErr   error
	activateHook  func()
	rollbackErr   error
	confirmErr    error
	confirmHook   func(PublicationSet)
	finalizePanic any

	contextKey        any
	contextValue      any
	cleanupContextOK  bool
	rollbackContextOK bool
	prepareEntered    chan struct{}
	prepareRelease    chan struct{}
	activePrepare     atomic.Int32
	maxPrepare        atomic.Int32

	prepareCalls    int
	compiledDomains int
	discardCalls    int
	rollbackCalls   int
	finalizeCalls   int
	confirmCalls    int
	confirmed       PublicationSet
	prepared        PublicationSet
	activated       PublicationSet
	finalized       PublicationSet
}

func newCoordinatorFakeEngine(recorder *callRecorder) *coordinatorFakeEngine {
	return &coordinatorFakeEngine{recorder: recorder}
}

func (e *coordinatorFakeEngine) Prepare(
	_ context.Context,
	ticket ApplyTicket,
	desired Snapshot,
	_ map[Domain]PublishedGeneration,
) (PublicationSet, error) {
	e.recorder.add("prepare")
	e.prepareCalls++
	active := e.activePrepare.Add(1)
	defer e.activePrepare.Add(-1)
	for {
		maximum := e.maxPrepare.Load()
		if active <= maximum || e.maxPrepare.CompareAndSwap(maximum, active) {
			break
		}
	}
	if e.prepareEntered != nil {
		e.prepareEntered <- struct{}{}
		<-e.prepareRelease
	}
	if e.prepareErr != nil {
		return PublicationSet{}, e.prepareErr
	}
	if len(ticket.RequiredDomains) == 0 {
		set := PublicationSet{
			DesiredRevision: ticket.DesiredRevision,
			Domains:         map[Domain]PublicationCandidate{},
		}
		e.prepared = cloneCoordinatorPublicationSet(set)
		return set, nil
	}
	e.compiledDomains += len(ticket.RequiredDomains)
	resources := desired.Resources()
	closure := make([]ResourceKey, 0, len(resources))
	decisions := make([]ResourceDecision, 0, len(resources))
	for _, item := range resources {
		closure = append(closure, item.Key)
		decisions = append(decisions, ResourceDecision{
			Key: item.Key, Disposition: DispositionPublished, Code: "test-published",
		})
	}
	set := PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains: map[Domain]PublicationCandidate{DomainHTTP: {
			Artifact: GenerationArtifact{
				Domain: DomainHTTP, Revision: ticket.DesiredRevision,
				Digest: desired.Digest(), Snapshot: desired.SnapshotID(),
			},
			Snapshot: desired.Clone(), Closure: closure, Decisions: decisions,
		}},
	}
	e.prepared = cloneCoordinatorPublicationSet(set)
	return set, nil
}

func (e *coordinatorFakeEngine) DiscardPrepared(ctx context.Context, _ PublicationSet) error {
	e.recorder.add("discard")
	e.discardCalls++
	e.cleanupContextOK = ctx.Err() == nil &&
		(e.contextKey == nil || ctx.Value(e.contextKey) == e.contextValue)
	return e.discardErr
}

func (e *coordinatorFakeEngine) Activate(_ context.Context, _ PublicationToken, set PublicationSet) error {
	e.recorder.add("activate")
	e.activated = cloneCoordinatorPublicationSet(set)
	if e.activateHook != nil {
		e.activateHook()
	}
	return e.activateErr
}

func (e *coordinatorFakeEngine) RollbackActivation(ctx context.Context, _ PublicationToken, _ PublicationSet) error {
	e.recorder.add("rollback")
	e.rollbackCalls++
	e.rollbackContextOK = ctx.Err() == nil &&
		(e.contextKey == nil || ctx.Value(e.contextKey) == e.contextValue)
	if ctx.Err() != nil {
		return errors.Join(e.rollbackErr, ctx.Err())
	}
	return e.rollbackErr
}

func (e *coordinatorFakeEngine) FinalizeActivation(_ context.Context, _ PublicationToken, set PublicationSet) {
	e.recorder.add("finalize")
	e.finalizeCalls++
	e.finalized = cloneCoordinatorPublicationSet(set)
	if e.finalizePanic != nil {
		panic(e.finalizePanic)
	}
}

func (e *coordinatorFakeEngine) ConfirmActive(_ context.Context, set PublicationSet) error {
	e.recorder.add("confirm-active")
	e.confirmCalls++
	e.confirmed = cloneCoordinatorPublicationSet(set)
	if e.confirmHook != nil {
		e.confirmHook(set)
	}
	return e.confirmErr
}

func desiredHTTPBatch(provider, revision string) DesiredBatch {
	return DesiredBatch{
		Cursor: ProviderCursor{Provider: provider, Revision: revision},
		Mutations: []Mutation{{
			Type: MutationPut, Key: ResourceKey{Kind: "routes", ID: "r1"}, Value: []byte(`{"id":"r1"}`),
		}},
		RequiredDomains: []Domain{DomainHTTP},
	}
}

func publishedForTicket(ticket ApplyTicket, domain Domain) PublishedGeneration {
	snapshot, _ := NewSnapshot(ticket.DesiredRevision, []Resource{{
		Key: ResourceKey{Kind: "routes", ID: "r1"}, Value: []byte(`{"id":"r1"}`),
	}}, nil)
	key := ResourceKey{Kind: "routes", ID: "r1"}
	return PublishedGeneration{
		Artifact: GenerationArtifact{
			Domain: domain, Revision: ticket.DesiredRevision,
			Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure:  []ResourceKey{key},
		Decisions: []ResourceDecision{{
			Key: key, Disposition: DispositionPublished, Code: "test-published",
		}},
	}
}

func acknowledgementForTicket(ticket ApplyTicket, published PublishedGeneration) Acknowledgement {
	revisions := RevisionSet{Desired: ticket.DesiredRevision}
	if published.Artifact.Domain == DomainHTTP {
		revisions.HTTP = published.Artifact.Revision
	} else {
		revisions.Stream = published.Artifact.Revision
	}
	return Acknowledgement{
		Cursor:    ticket.Cursor,
		Revisions: revisions,
		Decisions: map[Domain][]ResourceDecision{
			published.Artifact.Domain: slices.Clone(published.Decisions),
		},
	}
}

func cloneCoordinatorTicket(ticket ApplyTicket) ApplyTicket {
	ticket.RequiredDomains = slices.Clone(ticket.RequiredDomains)
	return ticket
}

func cloneCoordinatorAcknowledgement(ack Acknowledgement) Acknowledgement {
	clone := ack
	clone.Decisions = make(map[Domain][]ResourceDecision, len(ack.Decisions))
	for domain, decisions := range ack.Decisions {
		clone.Decisions[domain] = slices.Clone(decisions)
	}
	return clone
}

func cloneCoordinatorPublished(published PublishedGeneration) PublishedGeneration {
	clone := published
	clone.Snapshot = published.Snapshot.Clone()
	clone.Closure = slices.Clone(published.Closure)
	clone.Decisions = slices.Clone(published.Decisions)
	return clone
}

func cloneCoordinatorPublicationSet(set PublicationSet) PublicationSet {
	clone := PublicationSet{
		DesiredRevision: set.DesiredRevision,
		Domains:         make(map[Domain]PublicationCandidate, len(set.Domains)),
	}
	for domain, candidate := range set.Domains {
		candidate.Snapshot = candidate.Snapshot.Clone()
		candidate.Closure = slices.Clone(candidate.Closure)
		candidate.Decisions = slices.Clone(candidate.Decisions)
		clone.Domains[domain] = candidate
	}
	return clone
}
