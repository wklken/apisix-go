package syslog

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
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
		config.addr = net.JoinHostPort(config.Host, strconv.Itoa(config.Port))
	}
	return &syslogTransport{
		config: config,
		idle:   make(chan net.Conn, config.PoolSize),
		buffer: make([]byte, 0, config.FlushLimit),
	}, nil
}

// Log returns len(message) when the transport owns the message. On a flush
// failure the trigger message is handed back to the caller: a dial failure
// restores the prior buffer so the complete frame batch can be retried, while
// an ambiguous partial write discards the whole batch so an orphan suffix never
// survives onto a new connection.
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
		previousBuffered := len(t.buffer)
		t.buffer = append(t.buffer, message...)
		written, err := t.flushLocked()
		if err != nil {
			if written == 0 {
				// Nothing reached the peer: restore the prior buffer so the
				// complete frame batch can be retried, and hand the trigger
				// message back to the caller. A partial write has already
				// discarded the whole batch in flushLocked.
				t.buffer = t.buffer[:previousBuffered]
			}
			return 0, err
		}
		return len(message), nil
	default:
		buffered := len(t.buffer)
		_, err := t.flushLocked()
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
	_, err := t.flushLocked()
	return err
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
	if _, err := t.flushLocked(); err != nil {
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

func (t *syslogTransport) flushLocked() (int, error) {
	if len(t.buffer) == 0 {
		return 0, nil
	}
	written, err := t.write(t.buffer)
	if err != nil && written > 0 {
		// A partial write makes the byte boundary unknowable at the peer;
		// discard the whole batch so an orphan suffix never survives onto a
		// new frame. Retrying the already accepted prefix may duplicate it.
		t.buffer = t.buffer[:0]
		return written, err
	}
	if written > 0 {
		copy(t.buffer, t.buffer[written:])
		t.buffer = t.buffer[:len(t.buffer)-written]
	}
	return written, err
}

// write returns the payload prefix acknowledged by net.Conn.Write. A positive
// byte count is authoritative even when the same call also returns an error; the
// caller decides whether to retry the prefix or discard the ambiguous batch.
func (t *syslogTransport) write(payload []byte) (int, error) {
	connection, err := t.connection()
	if err != nil {
		return 0, fmt.Errorf(
			"failed to connect to syslog server: host[%s] port[%d]: %w",
			t.config.Host,
			t.config.Port,
			err,
		)
	}

	deadline := time.Now().Add(time.Duration(t.config.Timeout) * time.Millisecond)
	if err = connection.SetWriteDeadline(deadline); err != nil {
		_ = connection.Close()
		return 0, fmt.Errorf("failed to set syslog write deadline: %w", err)
	}
	totalWritten := 0
	for len(payload) > 0 {
		written, writeErr := connection.Write(payload)
		if written < 0 || written > len(payload) {
			_ = connection.Close()
			return totalWritten, fmt.Errorf("failed to send log message: %w in syslog", io.ErrShortWrite)
		}
		totalWritten += written
		payload = payload[written:]
		if writeErr != nil {
			_ = connection.Close()
			return totalWritten, fmt.Errorf("failed to send log message: %w in syslog", writeErr)
		}
		if written == 0 {
			_ = connection.Close()
			return totalWritten, fmt.Errorf("failed to send log message: %w in syslog", io.ErrNoProgress)
		}
	}
	if err = connection.SetWriteDeadline(time.Time{}); err != nil {
		_ = connection.Close()
		return totalWritten, fmt.Errorf("failed to clear syslog write deadline: %w", err)
	}
	t.release(connection)
	return totalWritten, nil
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
