package plugin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type responseCommitRecorder struct {
	header   http.Header
	statuses []int
	body     bytes.Buffer
}

type responseOptionalWriter struct {
	*responseCommitRecorder
	flushed          bool
	fullDuplex       bool
	writeCalls       int
	writeStringCalls int
	readFromCalls    int
	flushCalls       int
	flushErrorCalls  int
}

func (w *responseOptionalWriter) Write(body []byte) (int, error) {
	w.writeCalls++
	return w.responseCommitRecorder.Write(body)
}

func (w *responseOptionalWriter) WriteString(value string) (int, error) {
	w.writeStringCalls++
	return w.responseCommitRecorder.Write([]byte(value))
}

func (w *responseOptionalWriter) ReadFrom(reader io.Reader) (int64, error) {
	w.readFromCalls++
	body, err := io.ReadAll(reader)
	if err != nil {
		return 0, err
	}
	n, err := w.responseCommitRecorder.Write(body)
	return int64(n), err
}

func (w *responseOptionalWriter) Flush() {
	w.flushed = true
	w.flushCalls++
}

func (w *responseOptionalWriter) FlushError() error {
	w.flushed = true
	w.flushErrorCalls++
	return nil
}

func (w *responseOptionalWriter) CloseNotify() <-chan bool {
	return make(chan bool)
}

func (w *responseOptionalWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("not used")
}
func (w *responseOptionalWriter) Push(string, *http.PushOptions) error { return nil }
func (w *responseOptionalWriter) SetReadDeadline(time.Time) error      { return nil }
func (w *responseOptionalWriter) SetWriteDeadline(time.Time) error     { return nil }
func (w *responseOptionalWriter) EnableFullDuplex() error {
	w.fullDuplex = true
	return nil
}

func newResponseCommitRecorder() *responseCommitRecorder {
	return &responseCommitRecorder{header: make(http.Header)}
}

func (r *responseCommitRecorder) Header() http.Header { return r.header }
func (r *responseCommitRecorder) WriteHeader(status int) {
	r.statuses = append(r.statuses, status)
}

func (r *responseCommitRecorder) Write(body []byte) (int, error) {
	if len(r.statuses) == 0 {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(body)
}

type finalResponseCommitterFunc func(
	http.ResponseWriter,
	*http.Request,
	*base.ResponseState,
	BaseCommit,
)

func (f finalResponseCommitterFunc) CommitFinalResponse(
	w http.ResponseWriter,
	r *http.Request,
	state *base.ResponseState,
	commit BaseCommit,
) {
	f(w, r, state, commit)
}

func newBufferedTestExecutor(t *testing.T, bindings []Binding) *BufferedResponseExecutor {
	t.Helper()
	executor, err := NewBufferedResponseExecutor(
		bindings,
		TerminalDescriptor{Owner: TerminalOwnerOrdinaryProxy},
		base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
	)
	if err != nil {
		t.Fatalf("NewBufferedResponseExecutor() error = %v", err)
	}
	return executor
}

func serveBufferedTestPipeline(
	t *testing.T,
	bindings []Binding,
	resolver ConsumerBindingResolver,
	executor *BufferedResponseExecutor,
	terminal http.Handler,
	w http.ResponseWriter,
) *apisixctx.RequestLifecycle {
	t.Helper()
	request, lifecycle := executorRequest(t)
	NewRequestPipeline(bindings, resolver).
		WithBufferedResponseExecutor(executor).
		Then(terminal).
		ServeHTTP(w, request)
	return lifecycle
}

func upstreamTerminal(status int, body []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

func TestBufferedPluginPanicCarriesBindingIdentity(t *testing.T) {
	for _, test := range []struct {
		name    string
		factory string
		phase   Phase
		plugin  func(any) Plugin
	}{
		{
			name:    "mode selector",
			factory: "ai-rate-limiting",
			phase:   PhaseBodyFilter,
			plugin: func(panicValue any) Plugin {
				plugin := newDualModeResponseTestPlugin(base.RequestResponseModeBounded)
				plugin.Name = "non-canonical-mode-selector"
				plugin.selectMode = func(*http.Request) base.RequestResponseMode { panic(panicValue) }
				return plugin
			},
		},
		{
			name:    "response eligibility",
			factory: "echo",
			phase:   PhaseHeaderFilter,
			plugin: func(panicValue any) Plugin {
				plugin := newResponseTestPlugin(
					"non-canonical-eligibility",
					1,
					responseTestConfig{stage: "none", header: true},
				)
				plugin.eligible = func(apisixctx.ResponseSource) bool { panic(panicValue) }
				return plugin
			},
		},
		{
			name:    "header filter",
			factory: "echo",
			phase:   PhaseHeaderFilter,
			plugin: func(panicValue any) Plugin {
				plugin := newResponseTestPlugin(
					"non-canonical-header",
					1,
					responseTestConfig{stage: "none", header: true},
				)
				plugin.header = func(*http.Request, *base.ResponseState) error { panic(panicValue) }
				return plugin
			},
		},
		{
			name:    "buffered body filter",
			factory: "body-transformer",
			phase:   PhaseBodyFilter,
			plugin: func(panicValue any) Plugin {
				plugin := newResponseTestPlugin(
					"non-canonical-body",
					1,
					responseTestConfig{stage: "none", body: true},
				)
				plugin.body = func(*http.Request, *base.ResponseState) error { panic(panicValue) }
				return plugin
			},
		},
		{
			name:    "final response store",
			factory: "proxy-cache",
			phase:   PhaseBodyFilter,
			plugin: func(panicValue any) Plugin {
				plugin := newResponseTestPlugin("non-canonical-store", 1, nil)
				plugin.store = func(*http.Request, base.ResponseState) error { panic(panicValue) }
				return plugin
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			panicValue := &struct{ callback string }{callback: test.name}
			plugin := test.plugin(panicValue)
			binding := checkedResponseBinding(t, test.factory, plugin, ScopeRoute, "route")
			response := newResponseCommitRecorder()
			recovered := recoverCallbackPanic(t, func() {
				serveBufferedTestPipeline(
					t,
					[]Binding{binding},
					nil,
					newBufferedTestExecutor(t, []Binding{binding}),
					upstreamTerminal(http.StatusOK, []byte("uncommitted")),
					response,
				)
			})
			panicErr, ok := recovered.(*PanicError)
			if !ok {
				t.Fatalf("panic = %T, want *PanicError", recovered)
			}
			if panicErr.Factory != test.factory || panicErr.Phase != test.phase ||
				panicErr.Value != panicValue || len(panicErr.Stack) == 0 {
				t.Fatalf("panic metadata = %#v", panicErr)
			}
			if len(response.statuses) != 0 || response.body.Len() != 0 {
				t.Fatalf(
					"response committed after plugin panic: statuses=%v body=%q",
					response.statuses,
					response.body.String(),
				)
			}
		})
	}
}

func TestSwitchingWriterTransparentModePreservesEveryUnderlyingOptionalInterfaceAndBytes(t *testing.T) {
	executor := newBufferedTestExecutor(t, nil)
	t.Run("minimal header only", func(t *testing.T) {
		response := newResponseCommitRecorder()
		serveBufferedTestPipeline(
			t,
			nil,
			nil,
			executor,
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, ok := w.(http.Flusher); ok {
					t.Fatal("minimal writer unexpectedly exposes Flusher")
				}
				if _, ok := w.(http.Hijacker); ok {
					t.Fatal("minimal writer unexpectedly exposes Hijacker")
				}
				w.Header().Set("X-Header-Only", "yes")
			}),
			response,
		)
		if response.Header().Get("X-Header-Only") != "yes" || len(response.statuses) != 0 {
			t.Fatalf("header-only response = %q/%v", response.Header().Get("X-Header-Only"), response.statuses)
		}
	})

	t.Run("full optional writer", func(t *testing.T) {
		response := &responseOptionalWriter{responseCommitRecorder: newResponseCommitRecorder()}
		serveBufferedTestPipeline(
			t,
			nil,
			nil,
			executor,
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for name, supported := range map[string]bool{
					"Flusher":      implements[http.Flusher](w),
					"Hijacker":     implements[http.Hijacker](w),
					"Pusher":       implements[http.Pusher](w),
					"ReaderFrom":   implements[io.ReaderFrom](w),
					"StringWriter": implements[io.StringWriter](w),
				} {
					if !supported {
						t.Fatalf("full writer lost %s", name)
					}
				}
				_, _ = w.Write([]byte("zero"))
				_, _ = w.(io.StringWriter).WriteString("one")
				_, _ = w.(io.ReaderFrom).ReadFrom(strings.NewReader("two"))
				w.(http.Flusher).Flush()
				if err := w.(interface{ FlushError() error }).FlushError(); err != nil {
					t.Fatalf("FlushError() error = %v", err)
				}
				if err := w.(interface{ EnableFullDuplex() error }).EnableFullDuplex(); err != nil {
					t.Fatalf("EnableFullDuplex() error = %v", err)
				}
			}),
			response,
		)
		if response.body.String() != "zeroonetwo" || !response.flushed || !response.fullDuplex ||
			response.writeCalls != 1 || response.writeStringCalls != 1 || response.readFromCalls != 1 ||
			response.flushCalls != 1 || response.flushErrorCalls != 1 {
			t.Fatalf(
				"transparent response = %q flushed:%v duplex:%v calls=%d/%d/%d/%d/%d",
				response.body.String(),
				response.flushed,
				response.fullDuplex,
				response.writeCalls,
				response.writeStringCalls,
				response.readFromCalls,
				response.flushCalls,
				response.flushErrorCalls,
			)
		}
	})
}

func implements[T any](value any) bool {
	_, ok := value.(T)
	return ok
}

func TestSwitchingWriterDynamicBoundedWinnerDecidesBeforeTerminal(t *testing.T) {
	plugin := newResponseTestPlugin(
		"body-transformer",
		1,
		responseTestConfig{stage: "none", body: true},
	)
	plugin.body = func(_ *http.Request, state *base.ResponseState) error {
		state.Body = append(state.Body, []byte("-filtered")...)
		return nil
	}
	dynamic := checkedResponseBinding(t, "body-transformer", plugin, ScopeConsumer, "consumer")
	executor := newBufferedTestExecutor(t, nil)
	response := httptest.NewRecorder()
	serveBufferedTestPipeline(
		t,
		nil,
		func(r *http.Request) (ConsumerResolution, error) {
			return ConsumerResolution{Request: r, Resolved: true, Bindings: []Binding{dynamic}}, nil
		},
		executor,
		upstreamTerminal(http.StatusCreated, []byte("body")),
		response,
	)
	if response.Code != http.StatusCreated || response.Body.String() != "body-filtered" {
		t.Fatalf("response = %d/%q, want 201/body-filtered", response.Code, response.Body.String())
	}
}

func TestSwitchingWriterCommittedTransparentModeRejectsDynamicBoundedWinner(t *testing.T) {
	auth := newExecutorRequestPlugin("auth", 10, func(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
		w.(http.Flusher).Flush()
		return base.ContinueRequest(r)
	})
	authBinding := pipelineBinding("jwt-auth", auth, ScopeRoute, 10)
	bounded := newResponseTestPlugin(
		"body-transformer",
		1,
		responseTestConfig{stage: "none", body: true},
	)
	dynamic := checkedResponseBinding(t, "body-transformer", bounded, ScopeConsumer, "consumer")
	response := &responseOptionalWriter{responseCommitRecorder: newResponseCommitRecorder()}
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("panic = %#v, want http.ErrAbortHandler", got)
		}
		if !response.flushed || response.body.Len() != 0 {
			t.Fatalf("transparent response = flushed:%v body:%q", response.flushed, response.body.String())
		}
	}()
	serveBufferedTestPipeline(
		t,
		[]Binding{authBinding},
		func(r *http.Request) (ConsumerResolution, error) {
			return ConsumerResolution{Request: r, Resolved: true, Bindings: []Binding{dynamic}}, nil
		},
		newBufferedTestExecutor(t, []Binding{authBinding}),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("terminal called") }),
		response,
	)
}

func TestSwitchingWriterCommittedTransparentModeAbortsResolverFallback(t *testing.T) {
	auth := newExecutorRequestPlugin("auth", 10, func(w http.ResponseWriter, _ *http.Request) base.RequestPhaseResult {
		w.(http.Flusher).Flush()
		return base.ContinueRequest(httptest.NewRequest(http.MethodGet, "/replacement", nil))
	})
	authBinding := pipelineBinding("jwt-auth", auth, ScopeRoute, 10)
	response := &responseOptionalWriter{responseCommitRecorder: newResponseCommitRecorder()}
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("panic = %#v, want http.ErrAbortHandler", got)
		}
		if !response.flushed || response.body.Len() != 0 {
			t.Fatalf("transparent response = flushed:%v body:%q", response.flushed, response.body.String())
		}
	}()
	serveBufferedTestPipeline(
		t,
		[]Binding{authBinding},
		func(*http.Request) (ConsumerResolution, error) {
			return ConsumerResolution{}, errors.New("resolver failed")
		},
		newBufferedTestExecutor(t, []Binding{authBinding}),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("terminal called") }),
		response,
	)
}

func TestBufferedResponseRunsHeadersBeforeBodiesAcrossPartitions(t *testing.T) {
	order := make([]string, 0, 4)
	global := newResponseTestPlugin("global", 1, responseTestConfig{stage: "none", header: true, body: true})
	global.header = func(*http.Request, *base.ResponseState) error {
		order = append(order, "global-header")
		return nil
	}
	global.body = func(*http.Request, *base.ResponseState) error {
		order = append(order, "global-body")
		return nil
	}
	mergedHeader := newResponseTestPlugin("merged-header", 2, responseTestConfig{stage: "none", header: true})
	mergedHeader.header = func(*http.Request, *base.ResponseState) error {
		order = append(order, "merged-header")
		return nil
	}
	mergedBody := newResponseTestPlugin("merged-body", 1, responseTestConfig{stage: "none", body: true})
	mergedBody.body = func(*http.Request, *base.ResponseState) error {
		order = append(order, "merged-body")
		return nil
	}
	bindings := []Binding{
		checkedResponseBinding(t, "echo", global, ScopeGlobal, "global"),
		checkedResponseBinding(t, "serverless-pre-function", mergedHeader, ScopeRoute, "route"),
		checkedResponseBinding(t, "body-transformer", mergedBody, ScopeRoute, "route"),
	}
	serveBufferedTestPipeline(
		t,
		bindings,
		nil,
		newBufferedTestExecutor(t, bindings),
		upstreamTerminal(http.StatusOK, []byte("ok")),
		httptest.NewRecorder(),
	)
	want := []string{"global-header", "merged-header", "global-body", "merged-body"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("phase order = %v, want %v", order, want)
	}
}

func TestBufferedResponseAcceptsExactFourMiB(t *testing.T) {
	plugin := newResponseTestPlugin(
		"body-transformer",
		1,
		responseTestConfig{stage: "none", body: true},
	)
	binding := checkedResponseBinding(t, "body-transformer", plugin, ScopeRoute, "route")
	executor := newBufferedTestExecutor(t, []Binding{binding})
	response := httptest.NewRecorder()
	body := bytes.Repeat([]byte("x"), int(base.DefaultBufferedResponseMaxBytes))
	serveBufferedTestPipeline(t, []Binding{binding}, nil, executor, upstreamTerminal(http.StatusOK, body), response)
	if response.Code != http.StatusOK || response.Body.Len() != len(body) {
		t.Fatalf("response = %d/%d bytes, want 200/%d", response.Code, response.Body.Len(), len(body))
	}
}

func TestBufferedResponseAbsorbsUpstreamFlushUntilBodyTransformCompletes(t *testing.T) {
	plugin := newResponseTestPlugin(
		"body-transformer",
		1,
		responseTestConfig{stage: "none", body: true},
	)
	plugin.body = func(_ *http.Request, state *base.ResponseState) error {
		state.Body = append([]byte("before-"), state.Body...)
		return nil
	}
	binding := checkedResponseBinding(t, "body-transformer", plugin, ScopeRoute, "route")
	response := &responseOptionalWriter{responseCommitRecorder: newResponseCommitRecorder()}
	var flushErr error
	serveBufferedTestPipeline(
		t,
		[]Binding{binding},
		nil,
		newBufferedTestExecutor(t, []Binding{binding}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			_, _ = w.Write([]byte("hello "))
			flushErr = http.NewResponseController(w).Flush()
			_, _ = w.Write([]byte("world"))
		}),
		response,
	)
	if flushErr != nil {
		t.Fatalf("buffered upstream Flush() error = %v, want nil", flushErr)
	}
	if !reflect.DeepEqual(response.statuses, []int{http.StatusOK}) || response.body.String() != "before-hello world" {
		t.Fatalf(
			"response = %v/%q, want [200]/transformed chunked body",
			response.statuses,
			response.body.String(),
		)
	}
	if response.flushed {
		t.Fatal("destination flushed before the buffered body transform committed")
	}
}

func TestBufferedResponseCapPlusOneReturnsStable502WithoutCallbacks(t *testing.T) {
	plugin := newResponseTestPlugin(
		"body-transformer",
		1,
		responseTestConfig{stage: "none", body: true},
	)
	callbackCalls := 0
	plugin.body = func(*http.Request, *base.ResponseState) error {
		callbackCalls++
		return nil
	}
	binding := checkedResponseBinding(t, "body-transformer", plugin, ScopeRoute, "route")
	committerCalls := 0
	executor := newBufferedTestExecutor(t, []Binding{binding}).WithFinalResponseCommitter(
		finalResponseCommitterFunc(
			func(w http.ResponseWriter, _ *http.Request, state *base.ResponseState, commit BaseCommit) {
				committerCalls++
				commit(w, state)
			},
		),
	)
	response := httptest.NewRecorder()
	var written int
	var writeErr error
	serveBufferedTestPipeline(
		t,
		[]Binding{binding},
		nil,
		executor,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			body := bytes.Repeat([]byte("x"), int(base.DefaultBufferedResponseMaxBytes+1))
			written, writeErr = w.Write(body)
		}),
		response,
	)
	if written != int(base.DefaultBufferedResponseMaxBytes+1) || writeErr != nil {
		t.Fatalf("Write() = %d/%v", written, writeErr)
	}
	if callbackCalls != 0 || committerCalls != 0 {
		t.Fatalf("callback/committer calls = %d/%d, want 0/0", callbackCalls, committerCalls)
	}
	if response.Code != http.StatusBadGateway || response.Body.String() != `{"message":"Bad Gateway"}` {
		t.Fatalf("response = %d/%q", response.Code, response.Body.String())
	}
}

func TestBufferedResponseBodylessWriteDoesNotConsumeBodyCap(t *testing.T) {
	plugin := newResponseTestPlugin(
		"body-transformer",
		1,
		responseTestConfig{stage: "none", body: true},
	)
	binding := checkedResponseBinding(t, "body-transformer", plugin, ScopeRoute, "route")
	request := httptest.NewRequest(http.MethodHead, "/", nil)
	request, lifecycle := apisixctx.EnsureRequestLifecycle(request, time.Now())
	response := httptest.NewRecorder()
	NewRequestPipeline([]Binding{binding}, nil).
		WithBufferedResponseExecutor(newBufferedTestExecutor(t, []Binding{binding})).
		Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			w.WriteHeader(http.StatusOK)
			n, err := w.Write(bytes.Repeat([]byte("x"), int(base.DefaultBufferedResponseMaxBytes+1)))
			if n != int(base.DefaultBufferedResponseMaxBytes+1) || err != nil {
				t.Fatalf("HEAD Write() = %d/%v", n, err)
			}
		})).ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("HEAD response = %d/%d bytes", response.Code, response.Body.Len())
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceUpstream {
		t.Fatalf("source = %q", lifecycle.ResponseSource())
	}
}

func TestBufferedResponseRejectsUpgradeAndInvalidTrailerBeforeCallbacks(t *testing.T) {
	for _, test := range []struct {
		name     string
		terminal http.Handler
	}{
		{
			name: "upgrade",
			terminal: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
				w.WriteHeader(http.StatusSwitchingProtocols)
			}),
		},
		{
			name: "forbidden trailer",
			terminal: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
				w.Header().Set("Trailer", "Content-Length")
				_, _ = w.Write([]byte("body"))
				w.Header().Set("Content-Length", "4")
			}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plugin := newResponseTestPlugin(
				"body-transformer",
				1,
				responseTestConfig{stage: "none", body: true},
			)
			callbackCalls := 0
			plugin.body = func(*http.Request, *base.ResponseState) error {
				callbackCalls++
				return nil
			}
			binding := checkedResponseBinding(t, "body-transformer", plugin, ScopeRoute, "route")
			response := httptest.NewRecorder()
			serveBufferedTestPipeline(
				t,
				[]Binding{binding},
				nil,
				newBufferedTestExecutor(t, []Binding{binding}),
				test.terminal,
				response,
			)
			if callbackCalls != 0 || response.Code != http.StatusBadGateway ||
				response.Body.String() != `{"message":"Bad Gateway"}` {
				t.Fatalf(
					"response = callbacks:%d status:%d body:%q",
					callbackCalls,
					response.Code,
					response.Body.String(),
				)
			}
		})
	}
}

func TestBufferedResponseSeparatesAndCommitsDeclaredTrailers(t *testing.T) {
	responsePlugin := newResponseTestPlugin(
		"body-transformer",
		1,
		responseTestConfig{stage: "none", body: true},
	)
	callbackCalls := 0
	responsePlugin.body = func(_ *http.Request, state *base.ResponseState) error {
		callbackCalls++
		if state.Header.Get("Trailer") != "" || state.Header.Get("Grpc-Status") != "" {
			t.Fatalf("ordinary response headers contain trailers: %v", state.Header)
		}
		if state.Trailer.Get("Grpc-Status") != "0" || state.Trailer.Get("Grpc-Message") != "complete" {
			t.Fatalf("callback trailers = %v", state.Trailer)
		}
		state.Trailer.Set("Grpc-Message", "transcoded")
		return nil
	}
	binding := checkedResponseBinding(t, "body-transformer", responsePlugin, ScopeRoute, "route")
	response := httptest.NewRecorder()
	serveBufferedTestPipeline(
		t,
		[]Binding{binding},
		nil,
		newBufferedTestExecutor(t, []Binding{binding}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			w.Header().Add("Trailer", "Grpc-Status, Grpc-Message")
			_, _ = w.Write([]byte("body"))
			w.Header().Set("Grpc-Status", "0")
			w.Header().Set("Grpc-Message", "complete")
		}),
		response,
	)

	result := response.Result()
	if callbackCalls != 1 || result.Trailer.Get("Grpc-Status") != "0" ||
		result.Trailer.Get("Grpc-Message") != "transcoded" {
		t.Fatalf("response = callbacks:%d trailers:%v", callbackCalls, result.Trailer)
	}
}

func TestBufferedResponseCacheHitConsumesOnceAndSkipsTransformsStores(t *testing.T) {
	plugin := newResponseTestPlugin("proxy-cache", 1, nil)
	callbackCalls := 0
	plugin.request = func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
		holder := base.CacheHitResponseHolderFromRequest(r)
		holder.Publish(base.CachedResponseState{
			Status: http.StatusOK,
			Header: http.Header{"X-Cache": {"hit"}},
			Body:   []byte("cached"),
		})
		apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceCacheHit)
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceCacheHit)
	}
	plugin.store = func(*http.Request, base.ResponseState) error {
		callbackCalls++
		return nil
	}
	binding := checkedResponseBinding(t, "proxy-cache", plugin, ScopeRoute, "route")
	response := httptest.NewRecorder()
	serveBufferedTestPipeline(
		t,
		[]Binding{binding},
		nil,
		newBufferedTestExecutor(t, []Binding{binding}),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("terminal called on cache hit") }),
		response,
	)
	if response.Code != http.StatusOK || response.Body.String() != "cached" ||
		response.Header().Get("X-Cache") != "hit" {
		t.Fatalf(
			"cache hit response = %d/%q/%q",
			response.Code,
			response.Body.String(),
			response.Header().Get("X-Cache"),
		)
	}
	if callbackCalls != 0 {
		t.Fatalf("cache hit store calls = %d, want 0", callbackCalls)
	}
}

func TestResponsePlanMakesCompressionCacheSafe(t *testing.T) {
	cache := newResponseTestPlugin("proxy-cache", 1, nil)
	storedVary := ""
	cache.store = func(_ *http.Request, state base.ResponseState) error {
		storedVary = state.Header.Get("Vary")
		return nil
	}
	cacheBinding := checkedResponseBinding(t, "proxy-cache", cache, ScopeRoute, "cache-route")

	gzipPlugin := New("gzip", base.Dependencies{})
	if gzipPlugin == nil {
		t.Fatal("gzip plugin is not registered")
	}
	if err := gzipPlugin.Init(); err != nil {
		t.Fatalf("gzip Init() error = %v", err)
	}
	if err := util.Parse(
		map[string]any{"types": []string{"text/plain"}, "min_length": 1},
		gzipPlugin.Config(),
	); err != nil {
		t.Fatalf("parse gzip config: %v", err)
	}
	if err := gzipPlugin.PostInit(); err != nil {
		t.Fatalf("gzip PostInit() error = %v", err)
	}
	gzipBinding := resolvedPlan16Binding(t, "gzip", gzipPlugin, "gzip-route")
	rewrite := newResponseTestPlugin(
		"response-rewrite",
		2,
		responseTestConfig{stage: "none", streamingHeader: true},
	)
	rewrite.streamingHeader = func(_ *http.Request, state *base.StreamingResponseState) error {
		state.Header.Del("Vary")
		return nil
	}
	rewriteBinding := checkedResponseBinding(t, "response-rewrite", rewrite, ScopeRoute, "rewrite-route")
	bindings := []Binding{cacheBinding, gzipBinding, rewriteBinding}
	plan, err := BuildResponsePlan(ResponsePlanInput{
		StaticBindings: bindings,
		BufferedConfig: base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
	})
	if err != nil {
		t.Fatalf("BuildResponsePlan() error = %v", err)
	}

	request, lifecycle := executorRequest(t)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("cached"))
	})
	plan.Install(NewRequestPipeline(bindings, nil), terminal).
		ServeHTTP(response, request)
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceUpstream {
		t.Fatalf("response source = %q, want upstream", lifecycle.ResponseSource())
	}
	if storedVary != "Accept-Encoding" {
		t.Fatalf("stored Vary = %q, want Accept-Encoding", storedVary)
	}
	if got := response.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("response Vary = %q, want Accept-Encoding", got)
	}
	if got := response.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("response Content-Encoding = %q, want gzip", got)
	}
}

func TestFinalStoreErrorsContinueAndCommitUnchangedResponse(t *testing.T) {
	order := make([]string, 0, 2)
	observed := make(chan logger.Entry, 1)
	stopObserver := logger.ReplaceObserver(t.Name(), func(entry logger.Entry) {
		if strings.Contains(entry.Message, "final response store failed") {
			observed <- entry
		}
	})
	t.Cleanup(stopObserver)
	first := newResponseTestPlugin("proxy-cache", 200, nil)
	first.store = func(_ *http.Request, state base.ResponseState) error {
		order = append(order, "first")
		state.Header.Set("X-Mutated", "yes")
		state.Body[0] = 'X'
		return errors.New("disk\nunavailable " + strings.Repeat("x", 256))
	}
	second := newResponseTestPlugin("graphql-proxy-cache", 100, nil)
	second.store = func(_ *http.Request, state base.ResponseState) error {
		order = append(order, "second")
		if state.Header.Get("X-Mutated") != "" || string(state.Body) != "unchanged" {
			t.Fatalf("second store received aliased state: %#v/%q", state.Header, state.Body)
		}
		return nil
	}
	bindings := []Binding{
		checkedResponseBinding(t, "proxy-cache", first, ScopeRoute, "route"),
		checkedResponseBinding(t, "graphql-proxy-cache", second, ScopeRoute, "route"),
	}
	response := httptest.NewRecorder()
	serveBufferedTestPipeline(
		t,
		bindings,
		nil,
		newBufferedTestExecutor(t, bindings),
		upstreamTerminal(http.StatusAccepted, []byte("unchanged")),
		response,
	)
	if !reflect.DeepEqual(order, []string{"first", "second"}) {
		t.Fatalf("store order = %v", order)
	}
	if response.Code != http.StatusAccepted || response.Body.String() != "unchanged" {
		t.Fatalf("response = %d/%q", response.Code, response.Body.String())
	}
	select {
	case entry := <-observed:
		if strings.ContainsAny(entry.Message, "\r\n") {
			t.Fatalf("store diagnostic contains control characters: %q", entry.Message)
		}
		if len(entry.Message) > 384 {
			t.Fatalf("store diagnostic length = %d, want <= 384", len(entry.Message))
		}
		if !strings.Contains(entry.Message, `factory="proxy-cache" resource=route/"route"`) {
			t.Fatalf("store diagnostic lost bounded provenance: %q", entry.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("store failure diagnostic was not observed")
	}
}

func TestFinalStorePanicDoesNotClaimOrAttemptRollback(t *testing.T) {
	order := make([]string, 0, 3)
	panicValue := &struct{ callback string }{callback: "store"}
	first := newResponseTestPlugin("global-store", 300, nil)
	first.store = func(*http.Request, base.ResponseState) error {
		order = append(order, "first-side-effect")
		return nil
	}
	panicking := newResponseTestPlugin("panicking-store", 200, nil)
	panicking.store = func(*http.Request, base.ResponseState) error {
		order = append(order, "panic")
		panic(panicValue)
	}
	last := newResponseTestPlugin("last-store", 100, nil)
	last.store = func(*http.Request, base.ResponseState) error {
		order = append(order, "last")
		return nil
	}
	bindings := []Binding{
		checkedResponseBinding(t, "proxy-cache", first, ScopeGlobal, "global"),
		checkedResponseBinding(t, "proxy-cache", panicking, ScopeRoute, "route"),
		checkedResponseBinding(t, "graphql-proxy-cache", last, ScopeRoute, "route"),
	}
	response := newResponseCommitRecorder()
	defer func() {
		recovered := recover()
		got, ok := recovered.(*PanicError)
		if !ok {
			t.Fatalf("panic = %T, want *PanicError", recovered)
		}
		if got.Factory != "proxy-cache" || got.Phase != PhaseBodyFilter ||
			got.Value != panicValue || len(got.Stack) == 0 {
			t.Fatalf("panic metadata = %#v", got)
		}
		if !reflect.DeepEqual(order, []string{"first-side-effect", "panic"}) {
			t.Fatalf("store order = %v", order)
		}
		if len(response.statuses) != 0 || response.body.Len() != 0 {
			t.Fatalf(
				"response committed after store panic: statuses=%v body=%q",
				response.statuses,
				response.body.String(),
			)
		}
	}()
	serveBufferedTestPipeline(
		t,
		bindings,
		nil,
		newBufferedTestExecutor(t, bindings),
		upstreamTerminal(http.StatusOK, []byte("uncommitted")),
		response,
	)
}

func TestFinalResponseCommitterPanicRemainsRaw(t *testing.T) {
	plugin := newResponseTestPlugin("body-transformer", 1, responseTestConfig{stage: "none", body: true})
	binding := checkedResponseBinding(t, "body-transformer", plugin, ScopeRoute, "route")
	panicValue := &struct{ owner string }{owner: "core committer"}
	executor := newBufferedTestExecutor(t, []Binding{binding}).WithFinalResponseCommitter(
		finalResponseCommitterFunc(func(
			http.ResponseWriter,
			*http.Request,
			*base.ResponseState,
			BaseCommit,
		) {
			panic(panicValue)
		}),
	)
	response := newResponseCommitRecorder()
	recovered := recoverCallbackPanic(t, func() {
		serveBufferedTestPipeline(
			t,
			[]Binding{binding},
			nil,
			executor,
			upstreamTerminal(http.StatusOK, []byte("uncommitted")),
			response,
		)
	})
	if recovered != panicValue {
		t.Fatalf("panic = %#v, want original core panic %#v", recovered, panicValue)
	}
	if len(response.statuses) != 0 || response.body.Len() != 0 {
		t.Fatalf(
			"response committed after committer panic: statuses=%v body=%q",
			response.statuses,
			response.body.String(),
		)
	}
}

func TestFinalCommitterBaseCommitPreservesPrivate103AndRunsOnce(t *testing.T) {
	plugin := newResponseTestPlugin("body-transformer", 1, responseTestConfig{stage: "none", body: true})
	binding := checkedResponseBinding(t, "body-transformer", plugin, ScopeRoute, "route")
	committerCalls := 0
	executor := newBufferedTestExecutor(t, []Binding{binding}).WithFinalResponseCommitter(
		finalResponseCommitterFunc(func(
			w http.ResponseWriter,
			_ *http.Request,
			state *base.ResponseState,
			commit BaseCommit,
		) {
			committerCalls++
			state.Header.Set("X-Committer", "yes")
			commit(w, state)
		}),
	)
	response := newResponseCommitRecorder()
	serveBufferedTestPipeline(
		t,
		[]Binding{binding},
		nil,
		executor,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("body"))
		}),
		response,
	)
	if committerCalls != 1 || !reflect.DeepEqual(response.statuses, []int{http.StatusEarlyHints, http.StatusCreated}) ||
		response.body.String() != "body" || response.Header().Get("X-Committer") != "yes" {
		t.Fatalf(
			"commit = calls:%d statuses:%v body:%q header:%q",
			committerCalls,
			response.statuses,
			response.body.String(),
			response.Header().Get("X-Committer"),
		)
	}
}

func TestFinalCommitterRejectsZeroAndDoubleBaseCommit(t *testing.T) {
	plugin := newResponseTestPlugin("body-transformer", 1, responseTestConfig{stage: "none", body: true})
	binding := checkedResponseBinding(t, "body-transformer", plugin, ScopeRoute, "route")

	for _, test := range []struct {
		name      string
		committer finalResponseCommitterFunc
		wantBody  string
	}{
		{
			name:      "missing",
			committer: func(http.ResponseWriter, *http.Request, *base.ResponseState, BaseCommit) {},
		},
		{
			name: "double",
			committer: func(w http.ResponseWriter, _ *http.Request, state *base.ResponseState, commit BaseCommit) {
				commit(w, state)
				commit(w, state)
			},
			wantBody: "body",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			defer func() {
				if recover() == nil {
					t.Fatal("committer contract did not panic")
				}
				if response.Body.String() != test.wantBody {
					t.Fatalf("body = %q, want %q", response.Body.String(), test.wantBody)
				}
			}()
			serveBufferedTestPipeline(
				t,
				[]Binding{binding},
				nil,
				newBufferedTestExecutor(t, []Binding{binding}).WithFinalResponseCommitter(test.committer),
				upstreamTerminal(http.StatusOK, []byte("body")),
				response,
			)
		})
	}
}

func TestFinalCommitterRejectsConcurrentDoubleBaseCommit(t *testing.T) {
	plugin := newResponseTestPlugin("body-transformer", 1, responseTestConfig{stage: "none", body: true})
	binding := checkedResponseBinding(t, "body-transformer", plugin, ScopeRoute, "route")
	for range 32 {
		var panics atomic.Int32
		committer := finalResponseCommitterFunc(func(
			w http.ResponseWriter,
			_ *http.Request,
			state *base.ResponseState,
			commit BaseCommit,
		) {
			start := make(chan struct{})
			var workers sync.WaitGroup
			workers.Add(2)
			for range 2 {
				go func() {
					defer workers.Done()
					defer func() {
						if recover() != nil {
							panics.Add(1)
						}
					}()
					<-start
					commit(w, state)
				}()
			}
			close(start)
			workers.Wait()
		})
		response := httptest.NewRecorder()
		serveBufferedTestPipeline(
			t,
			[]Binding{binding},
			nil,
			newBufferedTestExecutor(t, []Binding{binding}).WithFinalResponseCommitter(committer),
			upstreamTerminal(http.StatusOK, []byte("body")),
			response,
		)
		if panics.Load() != 1 || response.Body.String() != "body" {
			t.Fatalf("concurrent commit = panics:%d body:%q, want 1/body", panics.Load(), response.Body.String())
		}
	}
}

func TestBufferedResponseRejectsInvalidFinalStatusBeforeStore(t *testing.T) {
	for _, status := range []int{0, 1000} {
		t.Run(fmt.Sprintf("transform-%d", status), func(t *testing.T) {
			transform := newResponseTestPlugin(
				"body-transformer",
				2,
				responseTestConfig{stage: "none", body: true},
			)
			transform.body = func(_ *http.Request, state *base.ResponseState) error {
				state.Status = status
				return nil
			}
			storeCalls := 0
			store := newResponseTestPlugin("proxy-cache", 1, nil)
			store.store = func(*http.Request, base.ResponseState) error {
				storeCalls++
				return nil
			}
			bindings := []Binding{
				checkedResponseBinding(t, "body-transformer", transform, ScopeRoute, "route"),
				checkedResponseBinding(t, "proxy-cache", store, ScopeRoute, "route"),
			}
			response := httptest.NewRecorder()
			serveBufferedTestPipeline(
				t,
				bindings,
				nil,
				newBufferedTestExecutor(t, bindings),
				upstreamTerminal(http.StatusOK, []byte("body")),
				response,
			)
			if storeCalls != 0 || response.Code != http.StatusBadGateway ||
				response.Body.String() != `{"message":"Bad Gateway"}` {
				t.Fatalf("invalid status = stores:%d response:%d/%q", storeCalls, response.Code, response.Body.String())
			}
		})

		t.Run(fmt.Sprintf("cache-hit-%d", status), func(t *testing.T) {
			cache := newResponseTestPlugin("proxy-cache", 1, nil)
			cache.request = func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
				base.CacheHitResponseHolderFromRequest(r).Publish(base.CachedResponseState{Status: status})
				return base.StopRequestWithSource(r, apisixctx.ResponseSourceCacheHit)
			}
			binding := checkedResponseBinding(t, "proxy-cache", cache, ScopeRoute, "route")
			response := httptest.NewRecorder()
			serveBufferedTestPipeline(
				t,
				[]Binding{binding},
				nil,
				newBufferedTestExecutor(t, []Binding{binding}),
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("terminal called") }),
				response,
			)
			if response.Code != http.StatusBadGateway || response.Body.String() != `{"message":"Bad Gateway"}` {
				t.Fatalf("invalid cache status response = %d/%q", response.Code, response.Body.String())
			}
		})
	}
}

func TestBufferedResponseUnknownSourceFailsClosed(t *testing.T) {
	plugin := newResponseTestPlugin("body-transformer", 1, responseTestConfig{stage: "none", body: true})
	binding := checkedResponseBinding(t, "body-transformer", plugin, ScopeRoute, "route")
	response := httptest.NewRecorder()
	lifecycle := serveBufferedTestPipeline(
		t,
		[]Binding{binding},
		nil,
		newBufferedTestExecutor(t, []Binding{binding}),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("unowned")) }),
		response,
	)
	if response.Code != http.StatusInternalServerError ||
		response.Body.String() != `{"message":"Internal Server Error"}` {
		t.Fatalf("response = %d/%q", response.Code, response.Body.String())
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceAPISIX {
		t.Fatalf("source = %q, want apisix", lifecycle.ResponseSource())
	}
}

func TestBufferedResponseInternalFailureSkipsStaticTransforms(t *testing.T) {
	plugin := newResponseTestPlugin("body-transformer", 1, responseTestConfig{stage: "none", body: true})
	transformCalls := 0
	plugin.body = func(_ *http.Request, state *base.ResponseState) error {
		transformCalls++
		state.Body = []byte("rewritten")
		return nil
	}
	binding := checkedResponseBinding(t, "body-transformer", plugin, ScopeRoute, "route")
	response := httptest.NewRecorder()
	serveBufferedTestPipeline(
		t,
		[]Binding{binding},
		func(*http.Request) (ConsumerResolution, error) {
			return ConsumerResolution{}, errors.New("resolver failed")
		},
		newBufferedTestExecutor(t, []Binding{binding}),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("terminal called") }),
		response,
	)
	if transformCalls != 0 || response.Code != http.StatusInternalServerError ||
		response.Body.String() != `{"message":"Internal Server Error"}` {
		t.Fatalf("failure = transforms:%d response:%d/%q", transformCalls, response.Code, response.Body.String())
	}
}

func TestPostResolutionHookRejectsRequestThatLostExecutionContext(t *testing.T) {
	plugin := newResponseTestPlugin("body-transformer", 1, responseTestConfig{stage: "none", body: true})
	transformCalls := 0
	plugin.body = func(*http.Request, *base.ResponseState) error {
		transformCalls++
		return nil
	}
	binding := checkedResponseBinding(t, "body-transformer", plugin, ScopeRoute, "route")
	response := httptest.NewRecorder()
	serveBufferedTestPipeline(
		t,
		[]Binding{binding},
		func(*http.Request) (ConsumerResolution, error) {
			return ConsumerResolution{
				Request:  httptest.NewRequest(http.MethodGet, "/replacement", nil),
				Resolved: true,
			}, nil
		},
		newBufferedTestExecutor(t, []Binding{binding}),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("terminal called") }),
		response,
	)
	if transformCalls != 0 || response.Code != http.StatusInternalServerError ||
		response.Body.String() != `{"message":"Internal Server Error"}` {
		t.Fatalf("failure = transforms:%d response:%d/%q", transformCalls, response.Code, response.Body.String())
	}
}

func TestBufferedResponseLostAuthenticationContextMarksAuthoritativeLifecycleAPISIX(t *testing.T) {
	auth := newExecutorRequestPlugin("auth", 10, func(_ http.ResponseWriter, _ *http.Request) base.RequestPhaseResult {
		return base.ContinueRequest(httptest.NewRequest(http.MethodGet, "/replacement", nil))
	})
	bounded := newResponseTestPlugin("body-transformer", 1, responseTestConfig{stage: "none", body: true})
	bindings := []Binding{
		pipelineBinding("jwt-auth", auth, ScopeRoute, 10),
		checkedResponseBinding(t, "body-transformer", bounded, ScopeRoute, "route"),
	}
	response := httptest.NewRecorder()
	lifecycle := serveBufferedTestPipeline(
		t,
		bindings,
		nil,
		newBufferedTestExecutor(t, bindings),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("terminal called") }),
		response,
	)
	if response.Code != http.StatusInternalServerError ||
		response.Body.String() != `{"message":"Internal Server Error"}` {
		t.Fatalf("response = %d/%q", response.Code, response.Body.String())
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceAPISIX {
		t.Fatalf("authoritative source = %q, want apisix", lifecycle.ResponseSource())
	}
}

func TestBufferedResponseCancellationAbortsBeforeOverflowFallback(t *testing.T) {
	plugin := newResponseTestPlugin("proxy-cache", 1, nil)
	plugin.request = func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		return base.ContinueRequest(r.WithContext(ctx))
	}
	binding := checkedResponseBinding(t, "proxy-cache", plugin, ScopeRoute, "route")
	response := httptest.NewRecorder()
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("panic = %#v, want http.ErrAbortHandler", got)
		}
		if response.Body.Len() != 0 {
			t.Fatalf("response committed after cancellation: %q", response.Body.String())
		}
	}()
	serveBufferedTestPipeline(
		t,
		[]Binding{binding},
		nil,
		newBufferedTestExecutor(t, []Binding{binding}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			_, _ = w.Write(bytes.Repeat([]byte("x"), int(base.DefaultBufferedResponseMaxBytes+1)))
		}),
		response,
	)
}

func TestBufferedResponseBoundedConflictRegistryIsFailClosed(t *testing.T) {
	bounded := newResponseTestPlugin("body-transformer", 2, responseTestConfig{stage: "none", body: true})
	boundedBinding := checkedResponseBinding(t, "body-transformer", bounded, ScopeRoute, "route")

	t.Run("terminal owner", func(t *testing.T) {
		_, err := NewBufferedResponseExecutor(
			[]Binding{boundedBinding},
			TerminalDescriptor{
				Owner:      TerminalOwnerAIRuntime,
				Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "ai-route"},
			},
			base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
		)
		if err == nil || !strings.Contains(err.Error(), "terminal owner") ||
			!strings.Contains(err.Error(), "ai-route") {
			t.Fatalf("NewBufferedResponseExecutor() error = %v", err)
		}
	})

	t.Run("effective identity", func(t *testing.T) {
		streaming := newExecutorPlugin("proxy-buffering", 1, nil)
		streamingBinding, err := BindPluginChecked(
			"proxy-buffering",
			streaming,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceService, ID: "svc-streaming"},
		)
		if err != nil {
			t.Fatalf("BindPluginChecked(proxy-buffering) error = %v", err)
		}
		_, err = NewBufferedResponseExecutor(
			[]Binding{boundedBinding, streamingBinding},
			TerminalDescriptor{Owner: TerminalOwnerOrdinaryProxy},
			base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
		)
		if err == nil || !strings.Contains(err.Error(), "proxy-buffering") ||
			!strings.Contains(err.Error(), "svc-streaming") {
			t.Fatalf("NewBufferedResponseExecutor() error = %v", err)
		}
	})
}
