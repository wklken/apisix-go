package stream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
	taskruntime "github.com/wklken/apisix-go/pkg/runtime"
)

const (
	acceptRetryInitial = 5 * time.Millisecond
	acceptRetryMax     = time.Second
)

type Runtime struct {
	ctx       context.Context
	cancel    context.CancelFunc
	source    RouterSource
	listeners []net.Listener
	connMu    sync.Mutex
	conns     map[net.Conn]struct{}
	tasks     *taskruntime.TaskRegistry
	owner     *taskruntime.TaskOwner
	closeOnce sync.Once
}

type runtimeCloseError struct {
	residuals []taskruntime.TaskResidual
	err       error
}

func (e *runtimeCloseError) Error() string {
	return "stream runtime close did not complete"
}

func (e *runtimeCloseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func NewRuntime(
	ctx context.Context,
	specs []config.TcpListen,
	source RouterSource,
) (*Runtime, error) {
	if err := validateListenerSpecs(specs); err != nil {
		return nil, err
	}
	return newRuntime(ctx, specs, source)
}

func newRuntime(
	ctx context.Context,
	specs []config.TcpListen,
	source RouterSource,
) (*Runtime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if source == nil {
		return nil, fmt.Errorf("stream runtime requires a router source")
	}
	runtimeCtx, cancel := context.WithCancel(ctx)
	tasks := taskruntime.NewTaskRegistry(runtimeCtx, nil)
	owner, err := taskruntime.NewTaskOwner(tasks, "core/stream-runtime", taskruntime.TaskCore)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create stream runtime task owner: %w", err)
	}
	runtime := &Runtime{
		ctx:    runtimeCtx,
		cancel: cancel,
		source: source,
		conns:  make(map[net.Conn]struct{}),
		tasks:  tasks,
		owner:  owner,
	}

	for _, spec := range specs {
		address, err := normalizeListenAddr(spec.Addr)
		if err != nil {
			runtime.initiateClose()
			_, _ = tasks.Stop(context.Background())
			return nil, err
		}
		listener, err := net.Listen("tcp", address)
		if err != nil {
			runtime.initiateClose()
			_, _ = tasks.Stop(context.Background())
			return nil, fmt.Errorf("listen stream address %q: %w", address, err)
		}
		runtime.listeners = append(runtime.listeners, listener)
	}

	for _, listener := range runtime.listeners {
		if err := owner.Go("listener", func(taskCtx context.Context) error {
			runtime.serveListener(taskCtx, listener)
			return nil
		}); err != nil {
			runtime.initiateClose()
			_, stopErr := tasks.Stop(context.Background())
			return nil, errors.Join(fmt.Errorf("start stream listener task: %w", err), stopErr)
		}
	}
	return runtime, nil
}

func validateListenerSpecs(specs []config.TcpListen) error {
	if len(specs) == 0 {
		return fmt.Errorf("stream runtime requires at least one TCP listener")
	}
	for _, spec := range specs {
		if spec.Tls {
			return fmt.Errorf("TLS stream listeners are not supported")
		}
	}
	return nil
}

func (r *Runtime) Addresses() []string {
	addresses := make([]string, 0, len(r.listeners))
	for _, listener := range r.listeners {
		if listener.Addr() != nil {
			addresses = append(addresses, listener.Addr().String())
		}
	}
	return addresses
}

func (r *Runtime) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.initiateClose()
	residuals, err := r.tasks.Stop(ctx)
	if err != nil || len(residuals) != 0 {
		return &runtimeCloseError{residuals: residuals, err: err}
	}
	return nil
}

func (r *Runtime) initiateClose() {
	r.closeOnce.Do(func() {
		r.close()
	})
}

func (r *Runtime) close() {
	r.cancel()
	for _, listener := range r.listeners {
		_ = listener.Close()
	}
	r.connMu.Lock()
	connections := make([]net.Conn, 0, len(r.conns))
	for conn := range r.conns {
		connections = append(connections, conn)
	}
	r.connMu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (r *Runtime) serveListener(ctx context.Context, listener net.Listener) {
	retryDelay := acceptRetryInitial
	for {
		conn, err := listener.Accept()
		if err != nil {
			if r.ctx.Err() != nil {
				return
			}
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if shouldRetryAccept(err) {
				timer := time.NewTimer(retryDelay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return
				}
				if retryDelay < acceptRetryMax {
					retryDelay *= 2
					if retryDelay > acceptRetryMax {
						retryDelay = acceptRetryMax
					}
				}
				continue
			}

			logger.Errorf("stream listener %q accept failed: %v", listenerAddress(listener), err)
			r.initiateClose()
			return
		}
		retryDelay = acceptRetryInitial

		if !r.trackConnection(conn) {
			_ = conn.Close()
			return
		}
		lease, ok := r.source()
		if !ok || lease.Router == nil || lease.Release == nil {
			_ = conn.Close()
			r.untrackConnection(conn)
			if ok && lease.Release != nil {
				lease.Release()
			}
			continue
		}
		if err := r.owner.Go("connection", func(taskCtx context.Context) error {
			defer lease.Release()
			defer r.untrackConnection(conn)
			defer func() { _ = conn.Close() }()
			_ = lease.Router.Serve(taskCtx, listener, conn)
			return nil
		}); err != nil {
			lease.Release()
			r.untrackConnection(conn)
			_ = conn.Close()
			r.initiateClose()
			return
		}
	}
}

func (r *Runtime) trackConnection(conn net.Conn) bool {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	if r.ctx.Err() != nil {
		return false
	}
	if r.conns == nil {
		r.conns = make(map[net.Conn]struct{})
	}
	r.conns[conn] = struct{}{}
	return true
}

func (r *Runtime) untrackConnection(conn net.Conn) {
	r.connMu.Lock()
	delete(r.conns, conn)
	r.connMu.Unlock()
}

func shouldRetryAccept(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOBUFS) ||
		errors.Is(err, syscall.ENOMEM)
}

func listenerAddress(listener net.Listener) string {
	if listener == nil || listener.Addr() == nil {
		return "<unknown>"
	}
	return listener.Addr().String()
}

func normalizeListenAddr(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", fmt.Errorf("stream listener address is empty")
	}
	if port, err := strconv.Atoi(address); err == nil {
		if port < 0 || port > 65535 {
			return "", fmt.Errorf("stream listener port %d is invalid", port)
		}
		return net.JoinHostPort("0.0.0.0", strconv.Itoa(port)), nil
	}
	if !strings.Contains(address, ":") {
		return "", fmt.Errorf("stream listener address %q must include a port", address)
	}
	return address, nil
}
