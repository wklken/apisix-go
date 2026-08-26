package plugin

import (
	"fmt"
	"net/http"
	"runtime/debug"
)

// PanicError attributes a recovered panic to one explicit plugin callback.
// Value is retained for internal classification; Error deliberately exposes
// only bounded descriptor metadata.
type PanicError struct {
	Factory string
	Phase   Phase
	Value   any
	Stack   []byte
}

func (e *PanicError) Error() string {
	if e == nil {
		return "plugin panic"
	}
	return fmt.Sprintf("plugin %q panicked in phase %q", e.Factory, e.Phase)
}

func guardCall(factory string, phase Phase, call func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			if panicErr, ok := recovered.(*PanicError); ok {
				err = panicErr
				return
			}
			err = &PanicError{
				Factory: factory,
				Phase:   phase,
				Value:   recovered,
				Stack:   debug.Stack(),
			}
		}
	}()
	return call()
}

func guardValue[T any](factory string, phase Phase, call func() (T, error)) (value T, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			if panicErr, ok := recovered.(*PanicError); ok {
				err = panicErr
				return
			}
			err = &PanicError{
				Factory: factory,
				Phase:   phase,
				Value:   recovered,
				Stack:   debug.Stack(),
			}
		}
	}()
	return call()
}

type downstreamPanic struct {
	value any
}

func guardContinuation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				panic(downstreamPanic{value: recovered})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func guardMiddleware(
	factory string,
	phase Phase,
	build func(http.Handler) http.Handler,
	next http.Handler,
) http.Handler {
	if build == nil || next == nil {
		return nil
	}
	handler := build(guardContinuation(next))
	if handler == nil {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if downstream, ok := recovered.(downstreamPanic); ok {
				panic(downstream.value)
			}
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			if panicErr, ok := recovered.(*PanicError); ok {
				panic(panicErr)
			}
			panic(&PanicError{
				Factory: factory,
				Phase:   phase,
				Value:   recovered,
				Stack:   debug.Stack(),
			})
		}()
		handler.ServeHTTP(w, r)
	})
}
