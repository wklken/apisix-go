package compiler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/runtime"
)

var ErrPreparedGenerationCleanupIncomplete = errors.New("prepared generation cleanup is incomplete")

type cleanupPhase uint8

const (
	cleanupQuiesce cleanupPhase = iota
	cleanupResourceFinalize
	cleanupRelease
)

type cleanupStep struct {
	name string
	run  func(context.Context) error
	done bool
}

type cleanupCheckpoint struct {
	owner      *cleanupStack
	quiescers  int
	finalizers int
	releases   int
}

type cleanupAttempt struct {
	done chan struct{}
	err  error
}

type cleanupStack struct {
	mu             sync.Mutex
	quiescers      []cleanupStep
	finalizers     []cleanupStep
	releases       []cleanupStep
	sealed         bool
	active         *cleanupAttempt
	terminalErrors []error
	terminal       bool
	closeErr       error
}

func (s *cleanupStack) Checkpoint() (cleanupCheckpoint, error) {
	if s == nil {
		return cleanupCheckpoint{}, fmt.Errorf("%w: cleanup stack is required", ErrInvalidInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return cleanupCheckpoint{}, fmt.Errorf("%w: cleanup ownership is sealed", ErrInvalidInput)
	}
	return cleanupCheckpoint{
		owner:      s,
		quiescers:  len(s.quiescers),
		finalizers: len(s.finalizers),
		releases:   len(s.releases),
	}, nil
}

func (s *cleanupStack) Rollback(ctx context.Context, checkpoint cleanupCheckpoint) error {
	if s == nil {
		return fmt.Errorf("%w: cleanup stack is required", ErrInvalidInput)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.sealed || checkpoint.owner != s || checkpoint.quiescers < 0 || checkpoint.finalizers < 0 ||
		checkpoint.releases < 0 || checkpoint.quiescers > len(s.quiescers) ||
		checkpoint.finalizers > len(s.finalizers) || checkpoint.releases > len(s.releases) {
		s.mu.Unlock()
		return fmt.Errorf("%w: cleanup checkpoint is invalid", ErrInvalidInput)
	}
	quiescers := s.quiescers[checkpoint.quiescers:]
	finalizers := s.finalizers[checkpoint.finalizers:]
	releases := s.releases[checkpoint.releases:]
	s.mu.Unlock()

	var terminalErrors []error
	rollbackErr, complete := executeCleanupAttempt(
		ctx,
		quiescers,
		finalizers,
		releases,
		&terminalErrors,
	)
	if !complete {
		return rollbackErr
	}

	s.mu.Lock()
	s.quiescers = s.quiescers[:checkpoint.quiescers]
	s.finalizers = s.finalizers[:checkpoint.finalizers]
	s.releases = s.releases[:checkpoint.releases]
	s.mu.Unlock()
	return rollbackErr
}

func (s *cleanupStack) Own(
	phase cleanupPhase,
	name string,
	run func(context.Context) error,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sealed {
		return fmt.Errorf("%w: cleanup ownership is sealed", ErrInvalidInput)
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: cleanup owner name is required", ErrInvalidInput)
	}
	if run == nil {
		return fmt.Errorf("%w: cleanup callback is required", ErrInvalidInput)
	}
	step := cleanupStep{name: name, run: run}
	switch phase {
	case cleanupQuiesce:
		s.quiescers = append(s.quiescers, step)
	case cleanupResourceFinalize:
		s.finalizers = append(s.finalizers, step)
	case cleanupRelease:
		s.releases = append(s.releases, step)
	default:
		return fmt.Errorf("%w: unknown cleanup phase %d", ErrInvalidInput, phase)
	}
	return nil
}

func (s *cleanupStack) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	s.sealed = true
	if s.terminal {
		closeErr := s.closeErr
		s.mu.Unlock()
		return closeErr
	}
	if s.active != nil {
		attempt := s.active
		s.mu.Unlock()
		return waitCleanupAttempt(ctx, attempt)
	}
	attempt := &cleanupAttempt{done: make(chan struct{})}
	s.active = attempt
	quiescers := s.quiescers
	finalizers := s.finalizers
	releases := s.releases
	s.mu.Unlock()

	attemptErr, complete := executeCleanupAttempt(
		ctx,
		quiescers,
		finalizers,
		releases,
		&s.terminalErrors,
	)

	s.mu.Lock()
	if complete {
		s.terminal = true
		s.closeErr = errors.Join(s.terminalErrors...)
		attempt.err = s.closeErr
	} else {
		attempt.err = attemptErr
	}
	if s.active == attempt {
		s.active = nil
	}
	close(attempt.done)
	s.mu.Unlock()
	return attempt.err
}

func (s *cleanupStack) terminallyClosed() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

func waitCleanupAttempt(ctx context.Context, attempt *cleanupAttempt) error {
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

func executeCleanupAttempt(
	ctx context.Context,
	quiescers []cleanupStep,
	finalizers []cleanupStep,
	releases []cleanupStep,
	terminalErrors *[]error,
) (error, bool) {
	var attemptErrors []error
	for index := range slices.Backward(quiescers) {
		step := &quiescers[index]
		if step.done {
			continue
		}
		err := step.run(ctx)
		if err == nil {
			step.done = true
			continue
		}
		attemptErrors = append(attemptErrors, cleanupStepError("quiesce", step.name, err))
	}
	if len(attemptErrors) != 0 {
		return joinCleanupErrors(*terminalErrors, attemptErrors), false
	}

	incomplete := false
	for index := range slices.Backward(finalizers) {
		step := &finalizers[index]
		if step.done {
			continue
		}
		err := step.run(ctx)
		if err == nil {
			step.done = true
			continue
		}
		wrapped := cleanupStepError("resource finalize", step.name, err)
		if cleanupFinalizationIncomplete(err) {
			incomplete = true
			attemptErrors = append(attemptErrors, wrapped)
			continue
		}
		step.done = true
		*terminalErrors = append(*terminalErrors, wrapped)
	}
	if incomplete {
		return joinCleanupErrors(*terminalErrors, attemptErrors), false
	}

	for index := range slices.Backward(releases) {
		step := &releases[index]
		if step.done {
			continue
		}
		err := step.run(ctx)
		step.done = true
		if err != nil {
			*terminalErrors = append(*terminalErrors, cleanupStepError("release", step.name, err))
		}
	}
	return errors.Join((*terminalErrors)...), true
}

func cleanupFinalizationIncomplete(err error) bool {
	var residual *runtime.TaskResidualError
	return errors.As(err, &residual) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrPreparedGenerationCleanupIncomplete)
}

func cleanupStepError(phase, name string, err error) error {
	return fmt.Errorf("cleanup %s %q: %w", phase, name, err)
}

func joinCleanupErrors(terminalErrors, attemptErrors []error) error {
	joined := make([]error, 0, len(terminalErrors)+len(attemptErrors))
	joined = append(joined, terminalErrors...)
	joined = append(joined, attemptErrors...)
	return errors.Join(joined...)
}
