package route

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/wklken/apisix-go/pkg/plugin/proxy_control"
)

// Buffering is on unless proxy-control explicitly disables it. The memory
// threshold selects storage, while the server's client_max_body_size reader
// remains the authority for rejecting an oversized request.
func bufferRequestBodyIfNeeded(r *http.Request) (func(), error) {
	if !proxy_control.GetRequestBuffering(r) || r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	original := r.Body
	prefix, err := io.ReadAll(io.LimitReader(original, proxy_control.DefaultRequestBufferingLimit+1))
	if err != nil {
		_ = original.Close()
		return nil, err
	}
	if int64(len(prefix)) <= proxy_control.DefaultRequestBufferingLimit {
		if err := original.Close(); err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(prefix))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(prefix)), nil }
		r.ContentLength = int64(len(prefix))
		return nil, nil
	}
	spool, err := os.CreateTemp("", "apisix-request-body-*")
	if err != nil {
		_ = original.Close()
		return nil, errors.New("failed to create request body spool")
	}
	// Unlink before writing request bytes: the request owns the open descriptor
	// and retries use ReadAt sections, so no pathname or file survives the request.
	if err := os.Remove(spool.Name()); err != nil {
		_ = original.Close()
		_ = spool.Close()
		_ = os.Remove(spool.Name())
		return nil, errors.New("failed to unlink request body spool")
	}
	cleanup := func() { _ = spool.Close() }
	if _, err := spool.Write(prefix); err != nil {
		_ = original.Close()
		cleanup()
		return nil, errors.New("failed to write request body spool")
	}
	remaining, err := io.Copy(spool, original)
	if closeErr := original.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return nil, errors.New("failed to write request body spool")
		}
		return nil, err
	}
	size := int64(len(prefix)) + remaining
	replay := func() (io.ReadCloser, error) { return io.NopCloser(io.NewSectionReader(spool, 0, size)), nil }
	r.Body, _ = replay()
	r.GetBody = replay
	r.ContentLength = size
	return cleanup, nil
}
