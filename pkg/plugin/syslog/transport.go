package syslog

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
)

type syslogTransport struct {
	config Config
	idle   chan net.Conn

	mu      sync.Mutex
	buffer  []byte
	closed  bool
	dropped int
}

type transportStats struct {
	Buffered int
	Dropped  int
}

func newSyslogTransport(config Config) (*syslogTransport, error) {
	if config.Timeout == 0 {
		config.Timeout = 3000
	}
	if config.FlushLimit == 0 {
		config.FlushLimit = 4096
	}
	if config.DropLimit == 0 {
		config.DropLimit = 1048576
	}
	if config.PoolSize == 0 {
		config.PoolSize = 5
	}
	if config.SockType == "" {
		config.SockType = "tcp"
	}
	if config.FlushLimit >= config.DropLimit {
		return nil, errors.New(`"flush_limit" should be < "drop_limit"`)
	}
	if config.addr == "" {
		config.addr = net.JoinHostPort(config.Host, fmt.Sprint(config.Port))
	}
	return &syslogTransport{
		config: config,
		idle:   make(chan net.Conn, config.PoolSize),
		buffer: make([]byte, 0, config.FlushLimit),
	}, nil
}

func (t *syslogTransport) Log(message []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return 0, errors.New("syslog transport is closed")
	}

	total := len(t.buffer) + len(message)
	switch {
	case total < t.config.FlushLimit:
		t.buffer = append(t.buffer, message...)
		return len(message), nil
	case total <= t.config.DropLimit:
		t.buffer = append(t.buffer, message...)
		return len(message), t.flushLocked()
	default:
		buffered := len(t.buffer)
		err := t.flushLocked()
		t.dropped++
		logger.Warn(fmt.Sprintf(
			"syslog buffer is full, dropping %d-byte message: buffered [%d], drop_limit [%d]",
			len(message),
			buffered,
			t.config.DropLimit,
		))
		return 0, err
	}
}

func (t *syslogTransport) Flush() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}
	return t.flushLocked()
}

func (t *syslogTransport) Stats() transportStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return transportStats{
		Buffered: len(t.buffer),
		Dropped:  t.dropped,
	}
}

func (t *syslogTransport) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return
	}
	if err := t.flushLocked(); err != nil {
		logger.Errorf("failed to flush syslog transport during shutdown: %s", err)
	}
	t.closed = true
	for {
		select {
		case connection := <-t.idle:
			_ = connection.Close()
		default:
			return
		}
	}
}

func (t *syslogTransport) flushLocked() error {
	if len(t.buffer) == 0 {
		return nil
	}
	payload := append([]byte(nil), t.buffer...)
	t.buffer = t.buffer[:0]
	return t.write(payload)
}

func (t *syslogTransport) write(payload []byte) error {
	connection, err := t.connection()
	if err != nil {
		return fmt.Errorf(
			"failed to connect to syslog server: host[%s] port[%d]: %w",
			t.config.Host,
			t.config.Port,
			err,
		)
	}

	deadline := time.Now().Add(time.Duration(t.config.Timeout) * time.Millisecond)
	if err = connection.SetWriteDeadline(deadline); err != nil {
		_ = connection.Close()
		return fmt.Errorf("failed to set syslog write deadline: %w", err)
	}
	for len(payload) > 0 {
		var written int
		written, err = connection.Write(payload)
		if err != nil {
			_ = connection.Close()
			return fmt.Errorf("failed to send log message: %s in syslog", err)
		}
		payload = payload[written:]
	}
	if err = connection.SetWriteDeadline(time.Time{}); err != nil {
		_ = connection.Close()
		return fmt.Errorf("failed to clear syslog write deadline: %w", err)
	}
	t.release(connection)
	return nil
}

func (t *syslogTransport) connection() (net.Conn, error) {
	if t.config.SockType == "tcp" {
		select {
		case connection := <-t.idle:
			return connection, nil
		default:
		}
	}

	dialer := &net.Dialer{Timeout: time.Duration(t.config.Timeout) * time.Millisecond}
	if !t.config.TLS {
		return dialer.Dial(t.config.SockType, t.config.addr)
	}
	return tls.DialWithDialer(dialer, "tcp", t.config.addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // APISIX syslog TLS does not verify the peer certificate
	})
}

func (t *syslogTransport) release(connection net.Conn) {
	if t.config.SockType != "tcp" {
		_ = connection.Close()
		return
	}
	select {
	case t.idle <- connection:
	default:
		_ = connection.Close()
	}
}
