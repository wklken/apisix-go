package plugin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type LogBinding struct {
	Plugin     Plugin
	Scope      Scope
	Provenance ResourceProvenance
	Policy     base.LogCapturePolicy
}

type LogExecutor struct {
	bindings        []LogBinding
	requestBodyMax  int
	responseBodyMax int
}

type LogRequestState struct {
	sealOnce               sync.Once
	registerOnce           sync.Once
	sealErr                error
	mu                     sync.RWMutex
	requestBody            []byte
	truncated              bool
	capturedRequestBodyMax int
	requestBodyMax         int
	bindings               []LogBinding
	sealed                 bool

	preparedRequest *http.Request
	preparedBody    io.ReadCloser
	prepared        bool
}

type logRequestStateKey struct{}

func NewLogExecutor(bindings []LogBinding) (LogExecutor, error) {
	cloned := append([]LogBinding(nil), bindings...)
	requestBodyMax, responseBodyMax := 0, 0
	for i := range cloned {
		if cloned[i].Plugin == nil {
			return LogExecutor{}, fmt.Errorf("log binding %d has nil plugin", i)
		}
		if err := base.ValidateLogCapturePolicy(cloned[i].Policy); err != nil {
			return LogExecutor{}, fmt.Errorf("log binding %q: %w", cloned[i].Plugin.GetName(), err)
		}
		requestBodyMax = max(requestBodyMax, cloned[i].Policy.RequestBodyBytes)
		responseBodyMax = max(responseBodyMax, cloned[i].Policy.ResponseBodyBytes)
	}
	return LogExecutor{
		bindings:        cloned,
		requestBodyMax:  requestBodyMax,
		responseBodyMax: responseBodyMax,
	}, nil
}

func (e LogExecutor) enabled() bool {
	return len(e.bindings) > 0
}

// NewLogExecutorFromBindings selects explicit sanitizer, log, and snapshot-
// finalizer owners from one materialized binding set. Dynamic finalizers
// register their own lifecycle callbacks and never enter this executor.
func NewLogExecutorFromBindings(bindings []Binding) (LogExecutor, error) {
	logBindings := make([]LogBinding, 0)
	routePrometheus := false
	for _, binding := range bindings {
		if binding.factoryName == "prometheus" &&
			binding.Scope != ScopeSystem && binding.Scope != ScopeGlobal {
			routePrometheus = true
			break
		}
	}
	globalPrometheusAdded := false
	for _, binding := range bindings {
		if binding.Plugin == nil {
			return LogExecutor{}, fmt.Errorf(
				"log materialization has nil plugin (factory=%q resource=%s/%s)",
				binding.factoryName,
				binding.Provenance.Kind,
				binding.Provenance.ID,
			)
		}
		spec, ok := CapabilitySpecForFactory(binding.factoryName)
		if !ok {
			return LogExecutor{}, fmt.Errorf("log materialization has unknown factory %q", binding.factoryName)
		}
		if binding.factoryName == "prometheus" {
			if binding.Scope == ScopeGlobal || binding.Scope == ScopeSystem {
				if routePrometheus || globalPrometheusAdded {
					continue
				}
				globalPrometheusAdded = true
			}
		}
		ownsLog := spec.Capabilities&CapabilityLog != 0
		ownsSanitizer := spec.Capabilities&CapabilityLogSanitizer != 0
		if ownsLog && isServerlessIdentity(binding.factoryName) {
			phase, err := configuredPhase(binding.Plugin.Config())
			if err != nil {
				return LogExecutor{}, fmt.Errorf("factory %q log phase: %w", binding.factoryName, err)
			}
			ownsLog = phase == "log"
		}
		ownsSnapshotFinalizer := spec.Finalizer == FinalizerSnapshot
		if !ownsSanitizer && !ownsLog && !ownsSnapshotFinalizer {
			continue
		}
		if ownsSanitizer {
			if _, ok := binding.Plugin.(base.LogSnapshotSanitizerPlugin); !ok {
				return LogExecutor{}, fmt.Errorf(
					"factory %q declares log sanitizer ownership without callback (resource=%s/%s)",
					binding.factoryName,
					binding.Provenance.Kind,
					binding.Provenance.ID,
				)
			}
		}
		if ownsLog {
			if _, ok := binding.Plugin.(base.LogPhasePlugin); !ok {
				return LogExecutor{}, fmt.Errorf(
					"factory %q declares log ownership without callback (resource=%s/%s)",
					binding.factoryName,
					binding.Provenance.Kind,
					binding.Provenance.ID,
				)
			}
		}
		if ownsSnapshotFinalizer {
			if _, ok := binding.Plugin.(base.SnapshotFinalizerPlugin); !ok {
				return LogExecutor{}, fmt.Errorf(
					"factory %q declares snapshot finalizer without callback (resource=%s/%s)",
					binding.factoryName,
					binding.Provenance.Kind,
					binding.Provenance.ID,
				)
			}
		}
		policy := base.LogCapturePolicy{}
		if provider, ok := binding.Plugin.(base.LogCapturePolicyPlugin); ok {
			policy = provider.LogCapturePolicy()
		}
		logBindings = append(logBindings, LogBinding{
			Plugin:     binding.Plugin,
			Scope:      binding.Scope,
			Provenance: binding.Provenance,
			Policy:     policy,
		})
	}
	return NewLogExecutor(logBindings)
}

// WithBindings returns a value copy with per-request materialized bindings.
// Route integration can construct the static executor before authentication,
// then replace this binding set after consumer/group resolution without
// mutating the published generation executor.
func (e LogExecutor) WithBindings(bindings []LogBinding) (LogExecutor, error) {
	return NewLogExecutor(bindings)
}

func (e LogExecutor) Bindings() []LogBinding {
	return append([]LogBinding(nil), e.bindings...)
}

func (e LogExecutor) Prepare(r *http.Request) (*http.Request, error) {
	if r == nil {
		return nil, fmt.Errorf("cannot prepare a nil request")
	}
	if !e.enabled() {
		return r, nil
	}
	if existing := logStateFromRequest(r); existing != nil {
		if capture, ok := base.ResponseCaptureFromRequest(r); ok {
			if err := capture.EnableBodyCapture(e.responseBodyMax); err != nil {
				return r, err
			}
		}
		existing.mu.Lock()
		existing.requestBodyMax = e.requestBodyMax
		existing.bindings = append([]LogBinding(nil), e.bindings...)
		canIncreaseCapture := !existing.sealed && e.requestBodyMax > existing.capturedRequestBodyMax &&
			r.Body != nil && r.Body != http.NoBody
		existing.mu.Unlock()
		if canIncreaseCapture {
			body, truncated, err := readAndRestoreBody(r, e.requestBodyMax)
			existing.mu.Lock()
			if err == nil {
				existing.requestBody = body
				existing.truncated = truncated
			} else {
				existing.requestBody = nil
				existing.truncated = false
			}
			existing.capturedRequestBodyMax = e.requestBodyMax
			existing.preparedBody = r.Body
			existing.sealErr = err
			existing.mu.Unlock()
			if err != nil {
				return r, err
			}
		}
		if lifecycle := apisixctx.GetRequestLifecycle(r); lifecycle != nil {
			lifecycle.SetFinalRequest(r)
		}
		return r, nil
	}
	if apisixctx.GetRequestLifecycle(r) == nil {
		var lifecycle *apisixctx.RequestLifecycle
		r, lifecycle = apisixctx.EnsureRequestLifecycle(r, time.Now())
		lifecycle.SetFinalRequest(r)
	}
	state := &LogRequestState{
		requestBodyMax: e.requestBodyMax,
		bindings:       append([]LogBinding(nil), e.bindings...),
	}
	r = r.WithContext(context.WithValue(r.Context(), logRequestStateKey{}, state))
	if capture, ok := base.ResponseCaptureFromRequest(r); ok {
		if err := capture.EnableBodyCapture(e.responseBodyMax); err != nil {
			return r, err
		}
	}
	if e.requestBodyMax > 0 && r.Body != nil && r.Body != http.NoBody {
		body, truncated, err := readAndRestoreBody(r, e.requestBodyMax)
		state.mu.Lock()
		if err == nil {
			state.requestBody = body
			state.truncated = truncated
		}
		state.capturedRequestBodyMax = e.requestBodyMax
		state.sealErr = err
		state.preparedRequest = r
		state.preparedBody = r.Body
		state.prepared = true
		state.mu.Unlock()
		if err != nil {
			return r, err
		}
	} else {
		state.mu.Lock()
		state.preparedRequest = r
		state.preparedBody = r.Body
		state.prepared = true
		state.mu.Unlock()
	}
	if lifecycle := apisixctx.GetRequestLifecycle(r); lifecycle != nil {
		lifecycle.SetFinalRequest(r)
	}
	return r, nil
}

func (e LogExecutor) SealFinalRequest(r *http.Request) error {
	if r == nil {
		return fmt.Errorf("cannot seal a nil request")
	}
	if lifecycle := apisixctx.GetRequestLifecycle(r); lifecycle != nil {
		lifecycle.SetFinalRequest(r)
	}
	state := logStateFromRequest(r)
	if state == nil {
		return nil
	}
	state.sealOnce.Do(func() {
		state.mu.Lock()
		state.sealed = true
		prepared := state.prepared && state.preparedRequest == r && sameReadCloser(state.preparedBody, r.Body)
		state.mu.Unlock()
		if prepared {
			return
		}
		state.mu.RLock()
		requestBodyMax := state.requestBodyMax
		state.mu.RUnlock()
		if requestBodyMax <= 0 || r.Body == nil || r.Body == http.NoBody {
			return
		}
		body, truncated, err := readAndRestoreBody(r, requestBodyMax)
		state.mu.Lock()
		state.requestBody = body
		state.truncated = truncated
		state.sealErr = err
		state.mu.Unlock()
	})
	state.mu.RLock()
	err := state.sealErr
	state.mu.RUnlock()
	return err
}

func sameReadCloser(a, b io.ReadCloser) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	aValue, bValue := reflect.ValueOf(a), reflect.ValueOf(b)
	if aValue.Type() != bValue.Type() || !aValue.Type().Comparable() {
		return false
	}
	return aValue.Interface() == bValue.Interface()
}

func (e LogExecutor) RegisterComposite(r *http.Request) bool {
	if r == nil {
		return false
	}
	state := logStateFromRequest(r)
	if state == nil {
		return false
	}
	lifecycle := apisixctx.GetRequestLifecycle(r)
	if lifecycle == nil {
		return false
	}
	registered := false
	state.registerOnce.Do(func() {
		registered = lifecycle.AddFinalizer("log-executor", func() error {
			return e.runComposite(lifecycle, r, state)
		})
	})
	return registered
}

func (e LogExecutor) SealAndRegister(r *http.Request) error {
	sealErr := e.SealFinalRequest(r)
	registered := e.RegisterComposite(r)
	recordLogPreparationFailure(sealErr, registered)
	return sealErr
}

func (e LogExecutor) runComposite(
	lifecycle *apisixctx.RequestLifecycle,
	fallback *http.Request,
	state *LogRequestState,
) (firstErr error) {
	request := lifecycle.FinalRequest()
	if request == nil {
		request = fallback
	}
	outcome := lifecycle.Outcome()
	source := lifecycle.ResponseSource()
	var response base.ResponseCaptureSnapshot
	if capture, ok := base.ResponseCaptureFromRequest(request); ok {
		response = capture.Snapshot()
	}
	var requestBody []byte
	var requestBodyTruncated bool
	if state != nil {
		state.mu.RLock()
		requestBody = append([]byte(nil), state.requestBody...)
		requestBodyTruncated = state.truncated
		state.mu.RUnlock()
	}
	snapshot := base.BuildLogSnapshotFromOwnedInputs(
		request,
		response,
		requestBody,
		requestBodyTruncated,
		outcome,
		source,
		lifecycle.StartedAt(),
		lifecycle.FinishedAt(),
	)
	bindings := append([]LogBinding(nil), e.bindings...)
	if state != nil {
		state.mu.RLock()
		bindings = append(bindings[:0], state.bindings...)
		state.mu.RUnlock()
	}
	slices.SortStableFunc(bindings, func(a, b LogBinding) int {
		if scopeRank(a.Scope) != scopeRank(b.Scope) {
			return scopeRank(a.Scope) - scopeRank(b.Scope)
		}
		if a.Plugin == nil || b.Plugin == nil {
			return 0
		}
		return b.Plugin.GetPriority() - a.Plugin.GetPriority()
	})
	selectedSanitizers := make([]bool, len(bindings))
	var preSanitizedSnapshot base.LogSnapshot
	var hasPreSanitizedSnapshot bool
	for index, binding := range bindings {
		if binding.Plugin == nil {
			continue
		}
		_, ok := binding.Plugin.(base.LogSnapshotSanitizerPlugin)
		if !ok {
			continue
		}
		selectedSanitizers[index] = true
		selector, ok := binding.Plugin.(base.LogSnapshotSanitizerSelectorPlugin)
		if !ok {
			continue
		}
		if !hasPreSanitizedSnapshot {
			preSanitizedSnapshot = base.CloneLogSnapshotForPolicy(snapshot, base.LogCapturePolicy{
				RequestBodyBytes:  base.MAX_REQ_BODY,
				ResponseBodyBytes: base.MAX_RESP_BODY,
			})
			hasPreSanitizedSnapshot = true
		}
		if err := runLogCallback(func() error {
			selectedSanitizers[index] = selector.ShouldSanitizeLogSnapshot(preSanitizedSnapshot)
			return nil
		}); err != nil {
			return fmt.Errorf("log sanitizer selector %q: %w", binding.Plugin.GetName(), err)
		}
	}
	for index, binding := range bindings {
		if !selectedSanitizers[index] {
			continue
		}
		callback := binding.Plugin.(base.LogSnapshotSanitizerPlugin)
		if err := runLogCallback(func() error {
			return callback.SanitizeLogSnapshot(&snapshot)
		}); err != nil {
			return fmt.Errorf("log sanitizer %q: %w", binding.Plugin.GetName(), err)
		}
	}
	for _, binding := range bindings {
		if binding.Plugin == nil {
			continue
		}
		callback, ok := binding.Plugin.(base.LogPhasePlugin)
		if !ok {
			continue
		}
		if err := runLogCallback(func() error {
			return callback.RunLogPhase(base.CloneLogSnapshotForPolicy(snapshot, binding.Policy))
		}); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("log callback %q: %w", binding.Plugin.GetName(), err)
		}
	}
	for _, binding := range bindings {
		if binding.Plugin == nil {
			continue
		}
		callback, ok := binding.Plugin.(base.SnapshotFinalizerPlugin)
		if !ok {
			continue
		}
		if err := runLogCallback(func() error {
			return callback.RunSnapshotFinalizer(base.CloneLogSnapshotForPolicy(snapshot, binding.Policy))
		}); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("snapshot finalizer %q: %w", binding.Plugin.GetName(), err)
		}
	}
	return firstErr
}

func runLogCallback(callback func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("callback panic: %v", recovered)
		}
	}()
	return callback()
}

func scopeRank(scope Scope) int {
	if scope == ScopeSystem || scope == ScopeGlobal {
		return 0
	}
	return 1
}

func logStateFromRequest(r *http.Request) *LogRequestState {
	if r == nil {
		return nil
	}
	state, _ := r.Context().Value(logRequestStateKey{}).(*LogRequestState)
	return state
}

func readAndRestoreBody(r *http.Request, limit int) ([]byte, bool, error) {
	original := r.Body
	prefix, err := io.ReadAll(io.LimitReader(original, int64(limit)+1))
	r.Body = &logReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), original), Closer: original}
	truncated := len(prefix) > limit
	if truncated {
		prefix = prefix[:limit]
	}
	return append([]byte(nil), prefix...), truncated, err
}

type logReadCloser struct {
	io.Reader
	io.Closer
}

func recordLogPreparationFailure(err error, registered bool) {
	if err == nil {
		return
	}
	logger.Errorf("log preparation failed (registered=%t): %T", registered, err)
}

func writeStableLogPreparationError(w http.ResponseWriter, _ error) {
	if w == nil {
		return
	}
	base.WriteJSONMessage(w, http.StatusInternalServerError, "Internal Server Error")
}

// Keep the stable writer as an explicit package seam for the server boundary;
// route integration invokes it only when sealing fails before commit.
var _ = writeStableLogPreparationError
