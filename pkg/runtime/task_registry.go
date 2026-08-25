package runtime

import (
	"context"
	"errors"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
)

var (
	ErrTaskRegistryStopped    = errors.New("task registry is stopped")
	ErrTaskRegistryRequired   = errors.New("task registry is required")
	ErrTaskOwnerFailed        = errors.New("task owner has failed")
	ErrTaskOwnerRequired      = errors.New("task owner is required")
	ErrTaskComponentInvalid   = errors.New("task component is invalid")
	ErrTaskCriticalityInvalid = errors.New("task criticality is invalid")
	ErrTaskCallbackRequired   = errors.New("task callback is required")
)

type TaskCriticality string

const (
	TaskPlugin TaskCriticality = "plugin"
	TaskCore   TaskCriticality = "core"
)

type TaskSpec struct {
	Owner       string
	Criticality TaskCriticality
}

type TaskOwner struct {
	registry    *TaskRegistry
	prefix      string
	criticality TaskCriticality
}

func NewTaskOwner(registry *TaskRegistry, prefix string, criticality TaskCriticality) (*TaskOwner, error) {
	if registry == nil {
		return nil, ErrTaskRegistryRequired
	}
	if strings.TrimSpace(prefix) == "" || strings.TrimSpace(prefix) != prefix {
		return nil, ErrTaskOwnerRequired
	}
	if criticality != TaskPlugin && criticality != TaskCore {
		return nil, ErrTaskCriticalityInvalid
	}
	return &TaskOwner{
		registry:    registry,
		prefix:      prefix,
		criticality: criticality,
	}, nil
}

func (owner *TaskOwner) Go(component string, run func(context.Context) error) error {
	if !validTaskComponent(component) {
		return ErrTaskComponentInvalid
	}
	return owner.registry.Go(TaskSpec{
		Owner:       owner.prefix + "/" + component,
		Criticality: owner.criticality,
	}, run)
}

func validTaskComponent(component string) bool {
	if len(component) == 0 || len(component) > 64 {
		return false
	}
	for index := range len(component) {
		value := component[index]
		alphanumeric := value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
		if alphanumeric {
			continue
		}
		if value != '-' || index == 0 || index == len(component)-1 {
			return false
		}
	}
	return true
}

type TaskFailure struct {
	Owner      string
	Err        error
	PanicValue any
	Stack      []byte
}

type TaskResidual struct {
	Owner string
}

type TaskResidualError struct {
	residuals []TaskResidual
	cause     error
}

func (e *TaskResidualError) Error() string {
	return "task registry stop has residual tasks"
}

func (e *TaskResidualError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *TaskResidualError) Residuals() []TaskResidual {
	if e == nil {
		return nil
	}
	return slices.Clone(e.residuals)
}

type TaskRegistry struct {
	ctx       context.Context
	cancel    context.CancelFunc
	onFailure func(TaskFailure)

	mu      sync.Mutex
	stopped bool
	active  map[string]int
	failed  map[string]struct{}
	wg      sync.WaitGroup

	cancelOnce sync.Once
	waitOnce   sync.Once
	waitDone   chan struct{}
}

func NewTaskRegistry(parent context.Context, onFailure func(TaskFailure)) *TaskRegistry {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &TaskRegistry{
		ctx:       ctx,
		cancel:    cancel,
		onFailure: onFailure,
		active:    make(map[string]int),
		failed:    make(map[string]struct{}),
		waitDone:  make(chan struct{}),
	}
}

func (r *TaskRegistry) Go(spec TaskSpec, run func(context.Context) error) error {
	if err := validateTaskSpec(spec, run); err != nil {
		return err
	}

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return ErrTaskRegistryStopped
	}
	if _, failed := r.failed[spec.Owner]; failed {
		r.mu.Unlock()
		return ErrTaskOwnerFailed
	}
	r.active[spec.Owner]++
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.finish(spec.Owner)
		if spec.Criticality == TaskCore {
			if err := run(r.ctx); err != nil {
				r.report(TaskFailure{Owner: spec.Owner, Err: err})
			}
			return
		}

		defer func() {
			if recovered := recover(); recovered != nil {
				r.failOwner(spec.Owner)
				r.report(TaskFailure{
					Owner:      spec.Owner,
					PanicValue: recovered,
					Stack:      debug.Stack(),
				})
			}
		}()
		if err := run(r.ctx); err != nil {
			r.failOwner(spec.Owner)
			r.report(TaskFailure{Owner: spec.Owner, Err: err})
		}
	}()
	return nil
}

func (r *TaskRegistry) Stop(ctx context.Context) ([]TaskResidual, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	r.cancelOnce.Do(r.cancel)
	r.waitOnce.Do(func() {
		go func() {
			r.wg.Wait()
			close(r.waitDone)
		}()
	})
	if taskRegistryWaitCompleted(r.waitDone) {
		return nil, nil
	}

	select {
	case <-r.waitDone:
		return nil, nil
	case <-ctx.Done():
		if taskRegistryWaitCompleted(r.waitDone) {
			return nil, nil
		}
		owners := r.Active()
		if len(owners) == 0 {
			<-r.waitDone
			return nil, nil
		}
		residuals := make([]TaskResidual, len(owners))
		for i, owner := range owners {
			residuals[i] = TaskResidual{Owner: owner}
		}
		return residuals, &TaskResidualError{
			residuals: slices.Clone(residuals),
			cause:     ctx.Err(),
		}
	}
}

func taskRegistryWaitCompleted(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func (r *TaskRegistry) Active() []string {
	r.mu.Lock()
	owners := make([]string, 0, len(r.active))
	for owner := range r.active {
		owners = append(owners, owner)
	}
	r.mu.Unlock()
	sort.Strings(owners)
	return owners
}

func validateTaskSpec(spec TaskSpec, run func(context.Context) error) error {
	if strings.TrimSpace(spec.Owner) == "" {
		return ErrTaskOwnerRequired
	}
	if spec.Criticality != TaskPlugin && spec.Criticality != TaskCore {
		return ErrTaskCriticalityInvalid
	}
	if run == nil {
		return ErrTaskCallbackRequired
	}
	return nil
}

func (r *TaskRegistry) finish(owner string) {
	r.mu.Lock()
	if r.active[owner] == 1 {
		delete(r.active, owner)
	} else {
		r.active[owner]--
	}
	r.mu.Unlock()
	r.wg.Done()
}

func (r *TaskRegistry) failOwner(owner string) {
	r.mu.Lock()
	r.failed[owner] = struct{}{}
	r.mu.Unlock()
}

func (r *TaskRegistry) report(failure TaskFailure) {
	if r.onFailure != nil {
		r.onFailure(failure)
	}
}
