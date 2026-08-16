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
	timerMu  sync.Mutex
	stopped  bool
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
	b.timerMu.Lock()
	defer b.timerMu.Unlock()
	if b.stopped {
		return
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(b.timeout, b.timeoutExpired)
		return
	}
	b.timer.Reset(b.timeout)
}

func (b *progressTimeoutBody) timeoutExpired() {
	b.timerMu.Lock()
	if b.stopped {
		b.timerMu.Unlock()
		return
	}
	b.stopped = true
	b.timedOut.Store(true)
	b.timerMu.Unlock()

	b.cancel()
	_ = b.body.Close()
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
	b.timerMu.Lock()
	if !b.stopped {
		b.stopped = true
		if b.timer != nil {
			b.timer.Stop()
		}
	}
	b.timerMu.Unlock()
}

// progressTimeoutDuplexBody keeps the optional write side that net/http uses
// for a 101 Switching Protocols tunnel. The non-duplex wrapper intentionally
// remains read-only so ordinary response bodies do not gain a misleading
// io.Writer capability.
type progressTimeoutDuplexBody struct {
	*progressTimeoutBody
	duplex        io.ReadWriteCloser
	writeTimeout  time.Duration
	writeTimer    *time.Timer
	writeTimerMu  sync.Mutex
	writeStopped  bool
	writeTimedOut atomic.Bool
}

func (b *progressTimeoutDuplexBody) armWrite() {
	if b.writeTimeout <= 0 {
		return
	}
	b.writeTimerMu.Lock()
	defer b.writeTimerMu.Unlock()
	if b.writeStopped {
		return
	}
	if b.writeTimer == nil {
		b.writeTimer = time.AfterFunc(b.writeTimeout, b.writeTimeoutExpired)
		return
	}
	b.writeTimer.Reset(b.writeTimeout)
}

func (b *progressTimeoutDuplexBody) writeTimeoutExpired() {
	b.writeTimerMu.Lock()
	if b.writeStopped {
		b.writeTimerMu.Unlock()
		return
	}
	b.writeStopped = true
	b.writeTimedOut.Store(true)
	b.writeTimerMu.Unlock()

	b.cancel()
	_ = b.body.Close()
}

func (b *progressTimeoutDuplexBody) Write(payload []byte) (int, error) {
	b.armWrite()
	n, err := b.duplex.Write(payload)
	if n > 0 {
		b.armWrite()
	}
	if err != nil {
		b.stop()
	}
	if b.timedOut.Load() || b.writeTimedOut.Load() {
		return n, context.DeadlineExceeded
	}
	return n, err
}

func (b *progressTimeoutDuplexBody) Read(payload []byte) (int, error) {
	n, err := b.progressTimeoutBody.Read(payload)
	if err != nil {
		b.stop()
	}
	return n, err
}

func (b *progressTimeoutDuplexBody) Close() error {
	b.stop()
	return b.body.Close()
}

func (b *progressTimeoutDuplexBody) stop() {
	b.progressTimeoutBody.stop()
	b.writeTimerMu.Lock()
	if !b.writeStopped {
		b.writeStopped = true
		if b.writeTimer != nil {
			b.writeTimer.Stop()
		}
	}
	b.writeTimerMu.Unlock()
}

func wrapProgressTimeoutBody(
	body io.ReadCloser,
	readTimeout time.Duration,
	writeTimeout time.Duration,
	cancel context.CancelFunc,
) io.ReadCloser {
	progress := newProgressTimeoutBody(body, readTimeout, cancel)
	duplex, ok := body.(io.ReadWriteCloser)
	if !ok {
		return progress
	}
	return &progressTimeoutDuplexBody{
		progressTimeoutBody: progress,
		duplex:              duplex,
		writeTimeout:        writeTimeout,
	}
}

type progressTimeoutTransport struct {
	base http.RoundTripper
	send time.Duration
	read time.Duration
}

func (transport *progressTimeoutTransport) CloseIdleConnections() {
	if closer, ok := transport.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
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
	response.Body = wrapCancelOnCloseBody(response.Body, cancel)
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

type cancelOnCloseDuplexBody struct {
	*cancelOnCloseBody
	duplex io.ReadWriteCloser
}

func (body *cancelOnCloseDuplexBody) Write(payload []byte) (int, error) {
	n, err := body.duplex.Write(payload)
	if err != nil {
		body.once.Do(body.cancel)
	}
	return n, err
}

func wrapCancelOnCloseBody(body io.ReadCloser, cancel context.CancelFunc) io.ReadCloser {
	cancelBody := &cancelOnCloseBody{ReadCloser: body, cancel: cancel}
	duplex, ok := body.(io.ReadWriteCloser)
	if !ok {
		return cancelBody
	}
	return &cancelOnCloseDuplexBody{cancelOnCloseBody: cancelBody, duplex: duplex}
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
	if response.Body == nil || response.Body == http.NoBody {
		cancel()
		return response, nil
	}
	if transport.read <= 0 {
		if _, duplex := response.Body.(io.ReadWriteCloser); !duplex || transport.send <= 0 {
			cancel()
			return response, nil
		}
		response.Body = wrapProgressTimeoutBody(response.Body, 0, transport.send, cancel)
		return response, nil
	}
	response.Body = wrapProgressTimeoutBody(response.Body, transport.read, transport.send, cancel)
	return response, nil
}
