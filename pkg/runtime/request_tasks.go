package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
)

var (
	ErrTaskGroupWaiting         = errors.New("request task group is waiting")
	ErrTaskGroupContextRequired = errors.New("request task group context is required")
	ErrTaskGroupOwnerRequired   = errors.New("request task group owner is required")
)

type RequestTaskGroup struct {
	ctx   context.Context
	owner string

	mu      sync.Mutex
	waiting bool
	errs    []error
	wg      sync.WaitGroup

	validationErr error
}

func NewRequestTaskGroup(parent context.Context, owner string) *RequestTaskGroup {
	group := &RequestTaskGroup{
		ctx:   parent,
		owner: owner,
		errs:  make([]error, 0),
	}
	if parent == nil {
		group.validationErr = ErrTaskGroupContextRequired
	} else if strings.TrimSpace(owner) == "" {
		group.validationErr = ErrTaskGroupOwnerRequired
	}
	return group
}

func (g *RequestTaskGroup) Go(run func(context.Context) error) error {
	if g.validationErr != nil {
		return g.validationErr
	}
	if run == nil {
		return ErrTaskCallbackRequired
	}

	g.mu.Lock()
	if g.waiting {
		g.mu.Unlock()
		return ErrTaskGroupWaiting
	}
	g.wg.Add(1)
	g.mu.Unlock()

	go func() {
		defer g.wg.Done()
		g.record(run(g.ctx))
	}()
	return nil
}

func (g *RequestTaskGroup) Wait() error {
	g.mu.Lock()
	g.waiting = true
	g.mu.Unlock()
	g.wg.Wait()

	g.mu.Lock()
	errs := append([]error(nil), g.errs...)
	g.mu.Unlock()
	return errors.Join(errs...)
}

func (g *RequestTaskGroup) record(err error) {
	if err == nil {
		return
	}
	g.mu.Lock()
	g.errs = append(g.errs, err)
	g.mu.Unlock()
}
