package stream

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
)

type Runtime struct {
	ctx       context.Context
	cancel    context.CancelFunc
	router    *Router
	listeners []net.Listener
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeDone chan struct{}
}

func NewRuntime(
	ctx context.Context,
	specs []config.TcpListen,
	routes []resource.StreamRoute,
	enabledPlugins []string,
	onResult func(Result),
) (*Runtime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	router, err := NewRouter(routes, enabledPlugins, onResult)
	if err != nil {
		return nil, err
	}
	runtimeCtx, cancel := context.WithCancel(ctx)
	runtime := &Runtime{
		ctx:       runtimeCtx,
		cancel:    cancel,
		router:    router,
		closeDone: make(chan struct{}),
	}

	for _, spec := range specs {
		if spec.Tls {
			runtime.close()
			return nil, fmt.Errorf("TLS stream listeners are not supported")
		}
		address, err := normalizeListenAddr(spec.Addr)
		if err != nil {
			runtime.close()
			return nil, err
		}
		listener, err := net.Listen("tcp", address)
		if err != nil {
			runtime.close()
			return nil, fmt.Errorf("listen stream address %q: %w", address, err)
		}
		runtime.listeners = append(runtime.listeners, listener)
	}

	for _, listener := range runtime.listeners {
		runtime.wg.Add(1)
		go runtime.serveListener(listener)
	}
	return runtime, nil
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

func (r *Runtime) Reload(routes []resource.StreamRoute) error {
	return r.router.Reload(routes)
}

func (r *Runtime) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.closeOnce.Do(func() {
		r.close()
		go func() {
			r.wg.Wait()
			close(r.closeDone)
		}()
	})
	select {
	case <-r.closeDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) close() {
	r.cancel()
	for _, listener := range r.listeners {
		_ = listener.Close()
	}
}

func (r *Runtime) serveListener(listener net.Listener) {
	defer r.wg.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if r.ctx.Err() != nil {
				return
			}
			if temporary, ok := err.(net.Error); ok && temporary.Temporary() {
				continue
			}
			return
		}

		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			_ = r.router.Serve(r.ctx, listener, conn)
		}()
	}
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
