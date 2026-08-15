package proxy

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// progressTimeoutBody enforces an inactivity deadline on body I/O. Each read
// that makes progress re-arms the deadline; a stalled read trips the timer,
// cancels the surrounding request context, and closes the underlying body so
// the reader unblocks.
type progressTimeoutBody struct {
	body     io.ReadCloser
	timeout  time.Duration
	timer    *time.Timer
	once     sync.Once
	timedOut atomic.Bool
	cancel   context.CancelFunc
}

func newProgressTimeoutBody(body io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) *progressTimeoutBody {
	return &progressTimeoutBody{body: body, timeout: timeout, cancel: cancel}
}

func (b *progressTimeoutBody) arm() {
	if b.timeout <= 0 {
		return
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(b.timeout, func() {
			b.timedOut.Store(true)
			b.cancel()
			_ = b.body.Close()
		})
		return
	}
	b.timer.Reset(b.timeout)
}

func (b *progressTimeoutBody) Read(p []byte) (int, error) {
	b.arm()
	n, err := b.body.Read(p)
	if n > 0 {
		b.arm()
	}
	if err != nil {
		b.stop()
	}
	if b.timedOut.Load() {
		return n, context.DeadlineExceeded
	}
	return n, err
}

func (b *progressTimeoutBody) Close() error {
	b.stop()
	return b.body.Close()
}

func (b *progressTimeoutBody) stop() {
	b.once.Do(func() {
		if b.timer != nil {
			b.timer.Stop()
		}
	})
}

type progressTimeoutTransport struct {
	base http.RoundTripper
	send time.Duration
	read time.Duration
}

type responseHeaderTimeoutTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

func newResponseHeaderTimeoutTransport(base http.RoundTripper, timeout time.Duration) http.RoundTripper {
	if timeout <= 0 {
		return base
	}
	return &responseHeaderTimeoutTransport{base: base, timeout: timeout}
}

func (transport *responseHeaderTimeoutTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(request.Context())
	timer := time.AfterFunc(transport.timeout, cancel)
	response, err := transport.base.RoundTrip(request.WithContext(ctx))
	timer.Stop()
	if err != nil {
		cancel()
		if ctx.Err() != nil && request.Context().Err() == nil {
			return response, context.DeadlineExceeded
		}
		return response, err
	}
	if ctx.Err() != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		cancel()
		if request.Context().Err() != nil {
			return nil, request.Context().Err()
		}
		return nil, context.DeadlineExceeded
	}
	if response == nil || response.Body == nil || response.Body == http.NoBody {
		cancel()
		return response, nil
	}
	response.Body = &cancelOnCloseBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type cancelOnCloseBody struct {
	io.ReadCloser
	once   sync.Once
	cancel context.CancelFunc
}

func (body *cancelOnCloseBody) Read(buffer []byte) (int, error) {
	n, err := body.ReadCloser.Read(buffer)
	if err != nil {
		body.once.Do(body.cancel)
	}
	return n, err
}

func (body *cancelOnCloseBody) Close() error {
	err := body.ReadCloser.Close()
	body.once.Do(body.cancel)
	return err
}

// NewProgressTimeoutTransport wraps a transport so request-body sends and
// response-body reads that make no progress for the configured durations
// cancel the request. Zero or negative durations leave the corresponding side
// unbounded. When neither side is configured the base transport is returned
// unchanged.
func NewProgressTimeoutTransport(base http.RoundTripper, send, read time.Duration) http.RoundTripper {
	if send <= 0 && read <= 0 {
		return base
	}
	return &progressTimeoutTransport{base: base, send: send, read: read}
}

func (transport *progressTimeoutTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)

	var sendBody *progressTimeoutBody
	if request.Body != nil && request.Body != http.NoBody && transport.send > 0 {
		sendBody = newProgressTimeoutBody(request.Body, transport.send, cancel)
		request.Body = sendBody
	}

	response, err := transport.base.RoundTrip(request)

	if sendBody != nil {
		sendBody.stop()
	}
	if err != nil {
		cancel()
		return response, err
	}
	if response.Body == nil || response.Body == http.NoBody || transport.read <= 0 {
		cancel()
		return response, nil
	}
	response.Body = newProgressTimeoutBody(response.Body, transport.read, cancel)
	return response, nil
}
