package chaitin_waf

import (
	"context"
	"io"
	"net/http"

	"github.com/felixge/httpsnoop"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type responseHeadersKey struct{}

func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	code, body, headers, responseHeaders := p.doAccess(r)
	if len(responseHeaders) > 0 {
		*r = *r.WithContext(context.WithValue(r.Context(), responseHeadersKey{}, responseHeaders))
	}
	if !*p.config.AppendWAFDebugHeader {
		delete(headers, HeaderChaitinWAFError)
		delete(headers, HeaderChaitinWAFServer)
	}
	if *p.config.AppendWAFRespHeader {
		for key, value := range headers {
			w.Header().Set(key, value)
		}
	}
	if code != 0 {
		w.WriteHeader(code)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
		return base.StopRequest(r)
	}
	return base.ContinueRequest(r)
}

func (p *Plugin) RunStreamingHeaderFilter(r *http.Request, state *base.StreamingResponseState) error {
	if r == nil || state == nil {
		return nil
	}
	headers, _ := r.Context().Value(responseHeadersKey{}).(http.Header)
	if len(headers) == 0 {
		return nil
	}
	if state.Header == nil {
		state.Header = make(http.Header)
	}
	for key, values := range headers {
		// t1k's extra-header parser keeps the final occurrence and assigns it.
		if len(values) > 0 {
			state.Header.Set(key, values[len(values)-1])
		}
	}
	return nil
}

// Direct Handler users have no response executor, so preserve the same header
// timing through a writer that retains the underlying optional interfaces.
func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		committed := false
		apply := func() {
			if committed {
				return
			}
			committed = true
			_ = p.RunStreamingHeaderFilter(r, &base.StreamingResponseState{Header: w.Header()})
		}
		wrapped := httpsnoop.Wrap(w, httpsnoop.Hooks{
			WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(code int) {
					if code >= 200 || code == 101 {
						apply()
					}
					next(code)
				}
			},
			Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
				return func(b []byte) (int, error) { apply(); return next(b) }
			},
			WriteString: func(next httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
				return func(s string) (int, error) { apply(); return next(s) }
			},
			ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
				return func(reader io.Reader) (int64, error) { apply(); return next(reader) }
			},
			Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc { return func() { apply(); next() } },
			FlushError: func(next httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
				return func() error { apply(); return next() }
			},
		})
		base.AdaptRequestPhase(p, next).ServeHTTP(wrapped, r)
		apply()
	})
}
