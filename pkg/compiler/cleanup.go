package compiler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

type cleanupPhase uint8

const (
	cleanupQuiesce cleanupPhase = iota
	cleanupRelease
)

type cleanupStep struct {
	name string
	run  func(context.Context) error
}

type cleanupCheckpoint struct {
	owner     *cleanupStack
	quiescers int
	releases  int
}

type cleanupStack struct {
	mu        sync.Mutex
	quiescers []cleanupStep
	releases  []cleanupStep
	sealed    bool
	closeOnce sync.Once
	closeErr  error
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
	return cleanupCheckpoint{owner: s, quiescers: len(s.quiescers), releases: len(s.releases)}, nil
}

func (s *cleanupStack) Rollback(ctx context.Context, checkpoint cleanupCheckpoint) error {
	if s == nil {
		return fmt.Errorf("%w: cleanup stack is required", ErrInvalidInput)
	}
	s.mu.Lock()
	if s.sealed || checkpoint.owner != s || checkpoint.quiescers < 0 || checkpoint.releases < 0 ||
		checkpoint.quiescers > len(s.quiescers) || checkpoint.releases > len(s.releases) {
		s.mu.Unlock()
		return fmt.Errorf("%w: cleanup checkpoint is invalid", ErrInvalidInput)
	}
	quiescers := append([]cleanupStep(nil), s.quiescers[checkpoint.quiescers:]...)
	releases := append([]cleanupStep(nil), s.releases[checkpoint.releases:]...)
	s.quiescers = s.quiescers[:checkpoint.quiescers]
	s.releases = s.releases[:checkpoint.releases]
	s.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	var cleanupErrs []error
	cleanupErrs = append(cleanupErrs, runCleanupSteps(ctx, "quiesce", quiescers)...)
	cleanupErrs = append(cleanupErrs, runCleanupSteps(ctx, "release", releases)...)
	return errors.Join(cleanupErrs...)
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
	case cleanupRelease:
		s.releases = append(s.releases, step)
	default:
		return fmt.Errorf("%w: unknown cleanup phase %d", ErrInvalidInput, phase)
	}
	return nil
}

func (s *cleanupStack) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}

		s.mu.Lock()
		s.sealed = true
		quiescers := append([]cleanupStep(nil), s.quiescers...)
		releases := append([]cleanupStep(nil), s.releases...)
		s.mu.Unlock()

		var cleanupErrs []error
		cleanupErrs = append(cleanupErrs, runCleanupSteps(ctx, "quiesce", quiescers)...)
		cleanupErrs = append(cleanupErrs, runCleanupSteps(ctx, "release", releases)...)
		s.closeErr = errors.Join(cleanupErrs...)
	})
	return s.closeErr
}

func runCleanupSteps(ctx context.Context, phase string, steps []cleanupStep) []error {
	cleanupErrs := make([]error, 0)
	for _, step := range slices.Backward(steps) {
		if err := step.run(ctx); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("cleanup %s %q: %w", phase, step.name, err))
		}
	}
	return cleanupErrs
}
