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

	"github.com/wklken/apisix-go/pkg/runtime"
)

const defaultIdleTimeout = 60 * time.Second

type directionResult struct {
	direction   string
	err         error
	eof         bool
	destination net.Conn
	completed   bool
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

	pumpCtx, cancel := context.WithCancel(ctx)
	tasks := runtime.NewRequestTaskGroup(pumpCtx, "connection/stream-bridge")
	stopContextClose := make(chan struct{})
	results := make(chan directionResult, 2)
	admissionFailure := func(admissionErr error) error {
		cancel()
		cleanupPanic := recoverPanic(func() { closeBoth(left, right) })
		close(stopContextClose)
		waitPanic, _ := waitTaskGroup(tasks)
		if waitPanic != nil {
			panic(waitPanic)
		}
		if cleanupPanic != nil {
			panic(cleanupPanic)
		}
		return admissionErr
	}
	if err := tasks.Go(func(context.Context) error {
		select {
		case <-ctx.Done():
			closeBoth(left, right)
		case <-stopContextClose:
		}
		return nil
	}); err != nil {
		return admissionFailure(err)
	}
	if err := tasks.Go(func(context.Context) error {
		copyDirection(left, right, leftReader, idle, "left-to-right", results)
		return nil
	}); err != nil {
		return admissionFailure(err)
	}
	if err := tasks.Go(func(context.Context) error {
		copyDirection(right, left, nil, idle, "right-to-left", results)
		return nil
	}); err != nil {
		return admissionFailure(err)
	}

	var cleanupPanic any
	cleanupDone := false
	cleanup := func() {
		if cleanupDone {
			return
		}
		cleanupDone = true
		cleanupPanic = recoverPanic(func() { closeBoth(left, right) })
	}
	finishWithPanic := func(result error, ownerPanic any) error {
		cancel()
		cleanup()
		close(stopContextClose)
		waitPanic, waitErr := waitTaskGroup(tasks)
		if waitPanic != nil {
			panic(waitPanic)
		}
		if ownerPanic != nil {
			panic(ownerPanic)
		}
		if cleanupPanic != nil {
			panic(cleanupPanic)
		}
		if result != nil {
			return result
		}
		return waitErr
	}
	finish := func(result error) error {
		return finishWithPanic(result, nil)
	}

	first := <-results
	if !first.completed {
		cleanup()
		second := <-results
		if ctxErr := ctx.Err(); ctxErr != nil {
			return finish(ctxErr)
		}
		if second.completed && !second.eof {
			return finish(normalizeCopyError(second.err))
		}
		return finish(nil)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return finish(ctxErr)
	}
	if first.eof {
		panicValue, err := halfCloseWriteResult(first.destination)
		if panicValue != nil {
			cleanup()
			<-results
			return finishWithPanic(nil, panicValue)
		}
		if err != nil {
			cleanup()
			<-results
			return finish(err)
		}
		second := <-results
		if ctxErr := ctx.Err(); ctxErr != nil {
			return finish(ctxErr)
		}
		if !second.completed {
			return finish(nil)
		}
		if second.eof {
			panicValue, err := halfCloseWriteResult(second.destination)
			if panicValue != nil {
				return finishWithPanic(nil, panicValue)
			}
			if err != nil {
				return finish(err)
			}
			return finish(nil)
		}
		return finish(normalizeCopyError(second.err))
	}

	cleanup()
	second := <-results
	if ctxErr := ctx.Err(); ctxErr != nil {
		return finish(ctxErr)
	}
	if err := normalizeCopyError(first.err); err != nil {
		return finish(err)
	}
	return finish(normalizeCopyError(second.err))
}

func copyDirection(
	src, dst net.Conn,
	reader io.Reader,
	idle time.Duration,
	direction string,
	results chan<- directionResult,
) {
	result := directionResult{direction: direction}
	defer func() { results <- result }()
	result = copyWithIdleDeadline(src, dst, reader, idle)
	result.direction = direction
	result.completed = true
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

func halfCloseWriteResult(conn net.Conn) (panicValue any, err error) {
	defer func() { panicValue = recover() }()
	err = halfCloseWrite(conn)
	return nil, err
}

func closeBoth(left, right net.Conn) {
	var firstPanic any
	closeOne := func(conn net.Conn) {
		defer func() {
			if recovered := recover(); recovered != nil && firstPanic == nil {
				firstPanic = recovered
			}
		}()
		_ = conn.Close()
	}
	closeOne(left)
	closeOne(right)
	if firstPanic != nil {
		panic(firstPanic)
	}
}

func recoverPanic(run func()) (value any) {
	defer func() { value = recover() }()
	run()
	return nil
}

func waitTaskGroup(tasks *runtime.RequestTaskGroup) (panicValue any, err error) {
	defer func() { panicValue = recover() }()
	return nil, tasks.Wait()
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
