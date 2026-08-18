package function_upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/shared"
	"go.uber.org/zap"
)

type Processor func(*http.Request, Config)

type Plugin struct {
	base.BasePlugin
	Config    Config
	Processor Processor
	Client    *http.Client

	clientRelease func()
	stopOnce      sync.Once
}

type Config struct {
	FunctionURI      string `json:"function_uri"`
	Timeout          int    `json:"timeout,omitempty"`
	SSLVerify        *bool  `json:"ssl_verify,omitempty"`
	Keepalive        *bool  `json:"keepalive,omitempty"`
	KeepaliveTimeout int    `json:"keepalive_timeout,omitempty"`
	KeepalivePool    int    `json:"keepalive_pool,omitempty"`
}

func (p *Plugin) PostInit() error {
	if p.Config.Timeout == 0 {
		p.Config.Timeout = 3000
	}
	if p.Config.KeepaliveTimeout == 0 {
		p.Config.KeepaliveTimeout = 60000
	}
	if p.Config.KeepalivePool == 0 {
		p.Config.KeepalivePool = 5
	}
	if p.Config.SSLVerify == nil {
		value := true
		p.Config.SSLVerify = &value
	}
	if p.Config.Keepalive == nil {
		value := true
		p.Config.Keepalive = &value
	}
	if p.Client == nil {
		uid := shared.NewConfigUID()
		uid.Add(
			p.Config.Timeout,
			*p.Config.SSLVerify,
			*p.Config.Keepalive,
			p.Config.KeepaliveTimeout,
			p.Config.KeepalivePool,
		)
		value, release, err := shared.AcquireClient(
			shared.ClientKey("function-upstream", uid),
			func() (any, error) { return p.newClient(), nil },
			func(value any) { value.(*http.Client).CloseIdleConnections() },
		)
		if err != nil {
			return fmt.Errorf("acquire function upstream client: %w", err)
		}
		p.Client = value.(*http.Client)
		p.clientRelease = release
	}

	return nil
}

func (p *Plugin) newClient() *http.Client {
	timeout := time.Duration(p.Config.Timeout) * time.Millisecond
	return &http.Client{
		Timeout:   0,
		Transport: proxy.NewProgressTimeoutTransport(p.transport(), timeout, timeout),
	}
}

func (p *Plugin) transport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	timeout := time.Duration(p.Config.Timeout) * time.Millisecond
	transport.DialContext = (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = timeout
	transport.DisableKeepAlives = !*p.Config.Keepalive
	transport.IdleConnTimeout = time.Duration(p.Config.KeepaliveTimeout) * time.Millisecond
	transport.MaxIdleConnsPerHost = p.Config.KeepalivePool
	if !*p.Config.SSLVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return transport
}

func (p *Plugin) Stop() {
	p.stopOnce.Do(func() {
		if p.clientRelease != nil {
			p.clientRelease()
		}
	})
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apisixctx.GetRequestLifecycle(r) != nil {
			base.AdaptRequestPhase(p, next).ServeHTTP(w, r)
			return
		}
		p.serve(w, r)
	})
}

// RunRequestPhase owns the external function response. The source is
// published before the first response byte so the strict request pipeline can
// classify both successful and failed function calls as upstream responses.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
	p.serve(w, r)
	return base.StopRequestWithSource(r, apisixctx.ResponseSourceUpstream)
}

func (p *Plugin) serve(w http.ResponseWriter, r *http.Request) {
	upstreamReq, err := p.buildRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if p.Processor != nil {
		p.Processor(upstreamReq, p.Config)
	}

	res, err := p.Client.Do(upstreamReq)
	if err != nil {
		p.recordFailure(r, classifyRequestFailure(r.Context(), err))
		http.Error(w, "failed to process "+p.Name, http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = res.Body.Close() }()

	reason, copyErr := writeResponse(w, res, r.ProtoMajor >= 2, r.Context())
	if copyErr == nil {
		return
	}
	p.recordFailure(r, reason)
	panic(http.ErrAbortHandler)
}

func (p *Plugin) buildRequest(r *http.Request) (*http.Request, error) {
	target, err := url.Parse(p.Config.FunctionURI)
	if err != nil {
		return nil, fmt.Errorf("invalid function_uri: %w", err)
	}

	body, err := base.ReadRequestBody(r)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}

	extension := chi.URLParam(r, "ext")
	if extension == "" {
		extension = chi.URLParam(r, "*")
	}
	if extension != "" {
		target.Path = path.Clean(appendExtensionPath(target.Path, extension))
	}
	target.RawQuery = r.URL.RawQuery
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	upstreamReq.Header = r.Header.Clone()
	upstreamReq.Host = target.Host
	upstreamReq.Header.Set("Host", target.Host)

	return upstreamReq, nil
}

func appendExtensionPath(basePath string, extension string) string {
	if basePath == "" {
		basePath = "/"
	}
	if strings.HasSuffix(basePath, "/") || strings.HasPrefix(extension, "/") {
		return basePath + extension
	}
	return basePath + "/" + extension
}

func writeResponse(
	w http.ResponseWriter,
	res *http.Response,
	http2 bool,
	requestContext context.Context,
) (apisixctx.ResponseFailureReason, error) {
	if http2 {
		base.RemoveHTTP2ConnectionHeaders(res.Header)
	}
	for field, values := range res.Header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(res.StatusCode)
	return copyResponseBody(w, res.Body, requestContext)
}

func copyResponseBody(
	destination io.Writer,
	source io.Reader,
	requestContext context.Context,
) (apisixctx.ResponseFailureReason, error) {
	buffer := make([]byte, 32*1024)
	for {
		read, readErr := source.Read(buffer)
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			if writeErr != nil {
				return apisixctx.ResponseFailureClientWriteError, writeErr
			}
			if written != read {
				return apisixctx.ResponseFailureClientWriteError, io.ErrShortWrite
			}
		}
		switch {
		case readErr == nil:
			continue
		case errors.Is(readErr, io.EOF):
			return "", nil
		case requestContext != nil && requestContext.Err() != nil:
			return apisixctx.ResponseFailureClientCanceled, requestContext.Err()
		case errors.Is(readErr, context.DeadlineExceeded):
			return apisixctx.ResponseFailureUpstreamIdleTimeout, readErr
		default:
			return apisixctx.ResponseFailureUpstreamCopyError, readErr
		}
	}
}

func classifyRequestFailure(
	requestContext context.Context,
	err error,
) apisixctx.ResponseFailureReason {
	if requestContext != nil && requestContext.Err() != nil {
		return apisixctx.ResponseFailureClientCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return apisixctx.ResponseFailureUpstreamHeaderTimeout
	}
	return apisixctx.ResponseFailureUpstreamRequestError
}

func (p *Plugin) recordFailure(r *http.Request, reason apisixctx.ResponseFailureReason) {
	if !apisixctx.ValidResponseFailureReason(reason) {
		return
	}
	if capture, ok := base.ResponseCaptureFromRequest(r); ok {
		capture.RecordFailure(reason)
	}
	apisixctx.RegisterRequestVar(r, "$upstream_failure_reason", string(reason))
	metrics.RecordFunctionUpstreamFailure(p.Name, string(reason))
	logger.Warn(
		"function upstream request failed",
		zap.String("plugin", p.Name),
		zap.String("reason", string(reason)),
	)
}
