package base

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

var ErrRequestBodyTooLarge = errors.New("request body too large")

const DefaultRequestBodySnapshotMemoryLimit int64 = 1024 * 1024

type requestBodySnapshotKey struct{}

type RequestBodySnapshot struct {
	mu     sync.RWMutex
	memory []byte
	path   string
	source io.Closer
	size   int64
	digest [sha256.Size]byte
	closed bool
}

func EnsureRequestBodySnapshot(
	request *http.Request,
	maxSize int64,
	memoryLimit int64,
	tempDir string,
) (*RequestBodySnapshot, error) {
	if request == nil {
		return nil, errors.New("request body snapshot: request is required")
	}
	if existing := RequestBodySnapshotFrom(request); existing != nil {
		if existing.Size() > maxSize {
			return existing, ErrRequestBodyTooLarge
		}
		if err := AttachRequestBodySnapshot(
			request,
			existing,
			apisixctx.GetRequestLifecycle(request) == nil,
		); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if maxSize <= 0 || memoryLimit <= 0 {
		return nil, errors.New("request body snapshot: positive limits are required")
	}
	snapshot, err := captureRequestBodySnapshot(request.Body, maxSize, memoryLimit, tempDir)
	if err != nil {
		return nil, err
	}
	if lifecycle := apisixctx.GetRequestLifecycle(request); lifecycle != nil {
		if !lifecycle.AddFinalizer("request-body-snapshot", snapshot.Close) {
			_ = snapshot.Close()
			return nil, errors.New("request body snapshot: lifecycle is closed")
		}
	}
	if err := AttachRequestBodySnapshot(request, snapshot, apisixctx.GetRequestLifecycle(request) == nil); err != nil {
		_ = snapshot.Close()
		return nil, err
	}
	return snapshot, nil
}

func captureRequestBodySnapshot(
	body io.ReadCloser,
	maxSize int64,
	memoryLimit int64,
	tempDir string,
) (*RequestBodySnapshot, error) {
	if body == nil || body == http.NoBody {
		return &RequestBodySnapshot{digest: sha256.Sum256(nil)}, nil
	}
	hash := sha256.New()
	memory := bytes.NewBuffer(make([]byte, 0, min(memoryLimit, 32*1024)))
	var file *os.File
	var path string
	cleanup := func() {
		if file != nil {
			_ = file.Close()
		}
		if path != "" {
			_ = os.Remove(path)
		}
		_ = body.Close()
	}
	var total int64
	buffer := make([]byte, 32*1024)
	for {
		read, err := body.Read(buffer)
		if read > 0 {
			total += int64(read)
			if total > maxSize {
				cleanup()
				return nil, ErrRequestBodyTooLarge
			}
			_, _ = hash.Write(buffer[:read])
			if file == nil && total > memoryLimit {
				file, err = os.CreateTemp(tempDir, "apisix-go-request-body-*")
				if err != nil {
					cleanup()
					return nil, fmt.Errorf("request body snapshot: create spill file: %w", err)
				}
				path = file.Name()
				if _, err = file.Write(memory.Bytes()); err != nil {
					cleanup()
					return nil, fmt.Errorf("request body snapshot: write spill file: %w", err)
				}
				memory.Reset()
			}
			if file != nil {
				if _, err = file.Write(buffer[:read]); err != nil {
					cleanup()
					return nil, fmt.Errorf("request body snapshot: write spill file: %w", err)
				}
			} else {
				_, _ = memory.Write(buffer[:read])
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				cleanup()
				var maxBytesError *http.MaxBytesError
				if errors.As(err, &maxBytesError) {
					return nil, ErrRequestBodyTooLarge
				}
				return nil, err
			}
			break
		}
	}
	if file != nil {
		if err := file.Close(); err != nil {
			cleanup()
			return nil, fmt.Errorf("request body snapshot: close spill file: %w", err)
		}
		file = nil
	}
	snapshot := &RequestBodySnapshot{
		memory: bytes.Clone(memory.Bytes()),
		path:   path,
		source: body,
		size:   total,
	}
	copy(snapshot.digest[:], hash.Sum(nil))
	return snapshot, nil
}

func RequestBodySnapshotFrom(request *http.Request) *RequestBodySnapshot {
	if request == nil {
		return nil
	}
	snapshot, _ := request.Context().Value(requestBodySnapshotKey{}).(*RequestBodySnapshot)
	return snapshot
}

func AttachRequestBodySnapshot(
	request *http.Request,
	snapshot *RequestBodySnapshot,
	closeSnapshot bool,
) error {
	if request == nil || snapshot == nil {
		return errors.New("request body snapshot: request and snapshot are required")
	}
	reader, err := snapshot.Open()
	if err != nil {
		return err
	}
	requestWithSnapshot := request.WithContext(
		contextWithRequestBodySnapshot(request, snapshot),
	)
	*request = *requestWithSnapshot
	request.Body = &snapshotReadCloser{ReadCloser: reader, snapshot: snapshot, closeSnapshot: closeSnapshot}
	request.GetBody = snapshot.Open
	return nil
}

func contextWithRequestBodySnapshot(request *http.Request, snapshot *RequestBodySnapshot) context.Context {
	return context.WithValue(request.Context(), requestBodySnapshotKey{}, snapshot)
}

type snapshotReadCloser struct {
	io.ReadCloser
	snapshot      *RequestBodySnapshot
	closeSnapshot bool
}

func (reader *snapshotReadCloser) Close() error {
	err := reader.ReadCloser.Close()
	if reader.closeSnapshot {
		err = errors.Join(err, reader.snapshot.Close())
	}
	return err
}

func (snapshot *RequestBodySnapshot) Size() int64 {
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	return snapshot.size
}

func (snapshot *RequestBodySnapshot) SHA256() [sha256.Size]byte {
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	return snapshot.digest
}

func (snapshot *RequestBodySnapshot) Open() (io.ReadCloser, error) {
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	if snapshot.closed {
		return nil, errors.New("request body snapshot: closed")
	}
	if snapshot.path != "" {
		return os.Open(snapshot.path)
	}
	return io.NopCloser(bytes.NewReader(snapshot.memory)), nil
}

func (snapshot *RequestBodySnapshot) Close() error {
	snapshot.mu.Lock()
	defer snapshot.mu.Unlock()
	if snapshot.closed {
		return nil
	}
	snapshot.closed = true
	clear(snapshot.memory)
	snapshot.memory = nil
	var err error
	if snapshot.source != nil {
		err = snapshot.source.Close()
		snapshot.source = nil
	}
	if snapshot.path == "" {
		return err
	}
	err = errors.Join(err, os.Remove(snapshot.path))
	snapshot.path = ""
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
