package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

const defaultIdleTimeout = 60 * time.Second

type directionResult struct {
	err         error
	eof         bool
	destination net.Conn
}

// Pump forwards bytes in both directions until both directions finish. A
// clean EOF half-closes the destination write side so the peer can still
// return a response. Context cancellation and hard copy failures close both
// connections to unblock the other direction.
func Pump(ctx context.Context, left, right net.Conn, leftReader io.Reader, idle time.Duration) error {
	if left == nil || right == nil {
		if left != nil {
			_ = left.Close()
		}
		if right != nil {
			_ = right.Close()
		}
		return fmt.Errorf("stream bridge connection is nil")
	}
	if ctx == nil {
		_ = left.Close()
		_ = right.Close()
		return fmt.Errorf("stream bridge context is nil")
	}
	if idle <= 0 {
		idle = defaultIdleTimeout
	}

	stopContextClose := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = left.Close()
			_ = right.Close()
		case <-stopContextClose:
		}
	}()
	defer close(stopContextClose)
	defer func() { _ = left.Close() }()
	defer func() { _ = right.Close() }()

	results := make(chan directionResult, 2)
	go copyDirection(left, right, leftReader, idle, results)
	go copyDirection(right, left, nil, idle, results)

	first := <-results
	if ctxErr := ctx.Err(); ctxErr != nil {
		closeBoth(left, right)
		<-results
		return ctxErr
	}
	if first.eof {
		if err := halfCloseWrite(first.destination); err != nil {
			closeBoth(left, right)
			<-results
			return err
		}
		second := <-results
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if second.eof {
			if err := halfCloseWrite(second.destination); err != nil {
				return err
			}
			return nil
		}
		return normalizeCopyError(second.err)
	}

	closeBoth(left, right)
	second := <-results
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err := normalizeCopyError(first.err); err != nil {
		return err
	}
	return normalizeCopyError(second.err)
}

func copyDirection(src, dst net.Conn, reader io.Reader, idle time.Duration, results chan<- directionResult) {
	results <- copyWithIdleDeadline(src, dst, reader, idle)
}

func copyWithIdleDeadline(src, dst net.Conn, reader io.Reader, idle time.Duration) directionResult {
	if reader == nil {
		reader = src
	}
	buffer := make([]byte, 32*1024)
	for {
		if err := src.SetReadDeadline(time.Now().Add(idle)); err != nil {
			return directionResult{err: err, destination: dst}
		}
		read, readErr := reader.Read(buffer)
		if read > 0 {
			if err := dst.SetWriteDeadline(time.Now().Add(idle)); err != nil {
				return directionResult{err: err, destination: dst}
			}
			written, writeErr := dst.Write(buffer[:read])
			if writeErr != nil {
				return directionResult{err: writeErr, destination: dst}
			}
			if written != read {
				return directionResult{err: io.ErrShortWrite, destination: dst}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return directionResult{eof: true, destination: dst}
			}
			return directionResult{err: readErr, destination: dst}
		}
	}
}

func halfCloseWrite(conn net.Conn) error {
	if writer, ok := conn.(interface{ CloseWrite() error }); ok {
		return writer.CloseWrite()
	}
	return nil
}

func closeBoth(left, right net.Conn) {
	_ = left.Close()
	_ = right.Close()
}

func normalizeCopyError(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrDeadlineExceeded) {
		return nil
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "closed pipe") || strings.Contains(message, "use of closed network connection") {
		return nil
	}
	return err
}
