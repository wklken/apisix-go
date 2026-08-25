package plugin

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPanicErrorDoesNotDiscloseValue(t *testing.T) {
	secret := "secret-panic-value"
	err := capturePluginPanicForTest("request-id", PhaseRewrite, secret)
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Error() disclosed panic value: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "request-id") || !strings.Contains(err.Error(), string(PhaseRewrite)) {
		t.Fatalf("Error() = %q, want bounded factory and phase", err.Error())
	}
	if err.Factory != "request-id" || err.Phase != PhaseRewrite || err.Value != secret || len(err.Stack) == 0 {
		t.Fatalf("panic metadata = %#v", err)
	}
}

func TestGuardCallPreservesReturnsAndTypedPanic(t *testing.T) {
	wantErr := errors.New("ordinary callback error")
	if got := guardCall("request-id", PhaseRewrite, func() error { return wantErr }); got != wantErr {
		t.Fatalf("guardCall() error = %v, want original %v", got, wantErr)
	}

	wantPanic := &PanicError{
		Factory: "inner",
		Phase:   PhaseAccess,
		Value:   "inner panic",
		Stack:   []byte("inner stack"),
	}
	if got := guardCall("outer", PhaseRewrite, func() error { panic(wantPanic) }); got != wantPanic {
		t.Fatalf("guardCall() panic error = %#v, want original %#v", got, wantPanic)
	}
}

func TestGuardCallAttributesRawPanic(t *testing.T) {
	want := &struct{ message string }{message: "callback invariant"}
	err := guardCall("request-id", PhaseAccess, func() error { panic(want) })
	got, ok := err.(*PanicError)
	if !ok {
		t.Fatalf("guardCall() error = %T, want *PanicError", err)
	}
	if got.Factory != "request-id" || got.Phase != PhaseAccess || got.Value != want || len(got.Stack) == 0 {
		t.Fatalf("panic metadata = %#v", got)
	}
}

func TestGuardCallAndValuePreserveExactAbortHandler(t *testing.T) {
	tests := []struct {
		name string
		run  func()
	}{
		{
			name: "call",
			run: func() {
				_ = guardCall("request-id", PhaseRewrite, func() error { panic(http.ErrAbortHandler) })
			},
		},
		{
			name: "value",
			run: func() {
				_, _ = guardValue("response-rewrite", PhaseBodyFilter, func() (string, error) {
					panic(http.ErrAbortHandler)
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := recoverCallbackPanic(t, test.run); got != http.ErrAbortHandler {
				t.Fatalf("panic = %#v, want exact http.ErrAbortHandler", got)
			}
		})
	}
}

func TestGuardCallAndValueAttributeAbortLookalikes(t *testing.T) {
	helpers := []struct {
		name   string
		invoke func(any) error
	}{
		{
			name: "call",
			invoke: func(value any) error {
				return guardCall("request-id", PhaseRewrite, func() error { panic(value) })
			},
		},
		{
			name: "value",
			invoke: func(value any) error {
				_, err := guardValue("request-id", PhaseRewrite, func() (string, error) { panic(value) })
				return err
			},
		},
	}
	lookalikes := []struct {
		name  string
		value error
	}{
		{name: "same message", value: errors.New(http.ErrAbortHandler.Error())},
		{name: "wrapped sentinel", value: fmt.Errorf("wrapped: %w", http.ErrAbortHandler)},
	}
	for _, helper := range helpers {
		for _, lookalike := range lookalikes {
			t.Run(helper.name+"/"+lookalike.name, func(t *testing.T) {
				err := helper.invoke(lookalike.value)
				got, ok := err.(*PanicError)
				if !ok {
					t.Fatalf("error = %T, want *PanicError", err)
				}
				if got.Factory != "request-id" || got.Phase != PhaseRewrite || got.Value != lookalike.value ||
					len(got.Stack) == 0 {
					t.Fatalf("panic metadata = %#v", got)
				}
			})
		}
	}
}

func TestGuardValuePreservesReturnsAndTypedPanic(t *testing.T) {
	wantErr := errors.New("ordinary selector error")
	wantValue := &struct{ name string }{name: "selected"}
	gotValue, gotErr := guardValue("response-rewrite", PhaseHeaderFilter, func() (*struct{ name string }, error) {
		return wantValue, wantErr
	})
	if gotValue != wantValue || gotErr != wantErr {
		t.Fatalf("guardValue() = (%#v, %v), want original (%#v, %v)", gotValue, gotErr, wantValue, wantErr)
	}

	wantPanic := &PanicError{
		Factory: "inner",
		Phase:   PhaseBodyFilter,
		Value:   "inner panic",
		Stack:   []byte("inner stack"),
	}
	gotValue, gotErr = guardValue("outer", PhaseHeaderFilter, func() (*struct{ name string }, error) {
		panic(wantPanic)
	})
	if gotValue != nil || gotErr != wantPanic {
		t.Fatalf("guardValue() panic = (%#v, %#v), want (nil, original %#v)", gotValue, gotErr, wantPanic)
	}
}

func TestGuardValueAttributesRawPanic(t *testing.T) {
	want := &struct{ message string }{message: "selector invariant"}
	gotValue, err := guardValue("response-rewrite", PhaseBodyFilter, func() (string, error) {
		panic(want)
	})
	if gotValue != "" {
		t.Fatalf("guardValue() value = %q, want zero value", gotValue)
	}
	got, ok := err.(*PanicError)
	if !ok {
		t.Fatalf("guardValue() error = %T, want *PanicError", err)
	}
	if got.Factory != "response-rewrite" || got.Phase != PhaseBodyFilter || got.Value != want || len(got.Stack) == 0 {
		t.Fatalf("panic metadata = %#v", got)
	}
}

func TestGuardMiddlewarePreservesNormalExecution(t *testing.T) {
	called := false
	handler := guardMiddleware("outer", PhaseRewrite,
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Plugin", "outer")
				next.ServeHTTP(w, r)
			})
		},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusAccepted)
		}),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if !called || recorder.Code != http.StatusAccepted || recorder.Header().Get("X-Plugin") != "outer" {
		t.Fatalf(
			"normal execution = called:%t status:%d header:%q",
			called,
			recorder.Code,
			recorder.Header().Get("X-Plugin"),
		)
	}
}

func TestGuardMiddlewareAttributesEntryAndUnwindPanics(t *testing.T) {
	tests := []struct {
		name  string
		build func(http.Handler) http.Handler
	}{
		{
			name: "entry",
			build: func(http.Handler) http.Handler {
				return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("entry panic") })
			},
		},
		{
			name: "unwind",
			build: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r)
					panic("unwind panic")
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recovered := recoverHandlerPanic(t, guardMiddleware(
				"outer",
				PhaseRewrite,
				test.build,
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
			))
			panicErr, ok := recovered.(*PanicError)
			if !ok {
				t.Fatalf("panic = %T, want *PanicError", recovered)
			}
			if panicErr.Factory != "outer" || panicErr.Phase != PhaseRewrite || panicErr.Value != test.name+" panic" ||
				len(panicErr.Stack) == 0 {
				t.Fatalf("panic metadata = %#v", panicErr)
			}
		})
	}
}

func TestGuardMiddlewarePreservesInnerPanicErrorIdentity(t *testing.T) {
	want := &PanicError{
		Factory: "inner",
		Phase:   PhaseAccess,
		Value:   "inner panic",
		Stack:   []byte("inner stack"),
	}
	inner := guardMiddleware("inner", PhaseAccess,
		func(http.Handler) http.Handler {
			return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(want) })
		},
		http.NotFoundHandler(),
	)
	outer := guardMiddleware("outer", PhaseRewrite,
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, r) })
		},
		inner,
	)
	if got := recoverHandlerPanic(t, outer); got != want {
		t.Fatalf("panic = %#v, want original %#v", got, want)
	}
}

func TestGuardMiddlewareDoesNotRelabelDownstreamPanic(t *testing.T) {
	want := &struct{ message string }{message: "core invariant"}
	handler := guardMiddleware("outer", PhaseRewrite,
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, r) })
		},
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(want) }),
	)
	if got := recoverHandlerPanic(t, handler); got != want {
		t.Fatalf("panic = %#v, want original %#v", got, want)
	}
}

func TestGuardMiddlewarePreservesAbortHandler(t *testing.T) {
	tests := []struct {
		name  string
		build func(http.Handler) http.Handler
		next  http.Handler
	}{
		{
			name: "entry",
			build: func(http.Handler) http.Handler {
				return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
			},
			next: http.NotFoundHandler(),
		},
		{
			name: "unwind",
			build: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r)
					panic(http.ErrAbortHandler)
				})
			},
			next: http.NotFoundHandler(),
		},
		{
			name: "downstream",
			build: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, r) })
			},
			next: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := guardMiddleware("outer", PhaseRewrite, test.build, test.next)
			if got := recoverHandlerPanic(t, handler); got != http.ErrAbortHandler {
				t.Fatalf("panic = %#v, want exact http.ErrAbortHandler", got)
			}
		})
	}
}

func TestGuardMiddlewareAttributesAbortLookalike(t *testing.T) {
	tests := []struct {
		name string
		want error
	}{
		{name: "same message", want: errors.New(http.ErrAbortHandler.Error())},
		{name: "wrapped sentinel", want: fmt.Errorf("wrapped: %w", http.ErrAbortHandler)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := guardMiddleware("outer", PhaseRewrite,
				func(http.Handler) http.Handler {
					return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(test.want) })
				},
				http.NotFoundHandler(),
			)
			recovered := recoverHandlerPanic(t, handler)
			got, ok := recovered.(*PanicError)
			if !ok {
				t.Fatalf("panic = %T, want *PanicError", recovered)
			}
			if got.Value != test.want {
				t.Fatalf("panic value = %#v, want original lookalike %#v", got.Value, test.want)
			}
		})
	}
}

func TestGuardMiddlewarePreservesNilConstructionValidation(t *testing.T) {
	validBuild := func(next http.Handler) http.Handler { return next }
	tests := []struct {
		name  string
		build func(http.Handler) http.Handler
		next  http.Handler
	}{
		{name: "nil build", next: http.NotFoundHandler()},
		{name: "nil next", build: validBuild},
		{
			name:  "nil built handler",
			build: func(http.Handler) http.Handler { return nil },
			next:  http.NotFoundHandler(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := guardMiddleware("outer", PhaseRewrite, test.build, test.next); got != nil {
				t.Fatalf("guardMiddleware() = %#v, want nil construction result", got)
			}
		})
	}
}

func TestGuardMiddlewareDoesNotRecoverBuildPanic(t *testing.T) {
	want := &struct{ message string }{message: "construction invariant"}
	defer func() {
		if got := recover(); got != want {
			t.Fatalf("panic = %#v, want original construction panic %#v", got, want)
		}
	}()
	guardMiddleware("outer", PhaseRewrite,
		func(http.Handler) http.Handler { panic(want) },
		http.NotFoundHandler(),
	)
}

func capturePluginPanicForTest(factory string, phase Phase, value any) *PanicError {
	err := guardCall(factory, phase, func() error { panic(value) })
	panicErr, ok := err.(*PanicError)
	if !ok {
		panic("guardCall did not return *PanicError")
	}
	return panicErr
}

func recoverHandlerPanic(t *testing.T, handler http.Handler) (recovered any) {
	t.Helper()
	defer func() { recovered = recover() }()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	t.Fatal("handler did not panic")
	return nil
}

func recoverCallbackPanic(t *testing.T, call func()) (recovered any) {
	t.Helper()
	defer func() { recovered = recover() }()
	call()
	t.Fatal("callback did not panic")
	return nil
}
