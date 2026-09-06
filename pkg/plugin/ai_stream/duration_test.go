package ai_stream

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
)

func TestDurationLimitDeliversCrossingChunkBeforeStopping(t *testing.T) {
	payload := "data: {malformed"
	response := httptest.NewRecorder()
	reader := LimitDuration(strings.NewReader(payload), time.Now().Add(-time.Second), time.Millisecond)
	_, err := ForwardSSE(response, reader, ai_protocols.OpenAIChat, 0)
	if !errors.Is(err, ErrMaxStreamDuration) {
		t.Fatalf("error=%v", err)
	}
	if response.Body.String() != payload {
		t.Fatalf("body=%q want %q", response.Body.String(), payload)
	}
}

func TestDurationLimitDoesNotReplaceEarlierReadFailure(t *testing.T) {
	reader := LimitDuration(strings.NewReader(""), time.Now().Add(-time.Second), time.Millisecond)
	if _, err := reader.Read(make([]byte, 10)); !errors.Is(err, io.EOF) {
		t.Fatalf("error=%v want EOF", err)
	}
}

func TestDurationLimitDeliversCrossingAWSEventStreamFrameBeforeStopping(t *testing.T) {
	for _, eventType := range []string{"contentBlockDelta", "metadata"} {
		for _, size := range []int{20, 65536} {
			frame := awsEventStreamFrame(
				map[string]string{":message-type": "event", ":event-type": eventType},
				`{"delta":{"text":"`+strings.Repeat("x", size)+`"}}`,
			)
			response := httptest.NewRecorder()
			reader := LimitDuration(bytes.NewReader(frame), time.Now().Add(-time.Second), time.Millisecond)
			_, err := ForwardAWSEventStream(response, reader, 0)
			if !errors.Is(err, ErrMaxStreamDuration) {
				t.Fatalf("%s/%d error=%v", eventType, size, err)
			}
			want := frame
			if len(want) > 32*1024 {
				want = want[:32*1024]
			}
			if !bytes.Equal(response.Body.Bytes(), want) {
				t.Fatalf("%s/%d body length=%d want %d", eventType, size, response.Body.Len(), len(want))
			}
		}
	}
}

func TestDurationLimitForwardsPartialAndMultipleAWSFramesWithoutAnotherRead(t *testing.T) {
	frame := awsEventStreamFrame(
		map[string]string{":message-type": "event", ":event-type": "contentBlockDelta"},
		`{"delta":{"text":"hello"}}`,
	)
	for _, chunk := range [][]byte{frame[:8], append(append([]byte{}, frame...), frame...)} {
		response := httptest.NewRecorder()
		reader := LimitDuration(
			&singleAWSChunkReader{t: t, chunk: chunk},
			time.Now().Add(-time.Second),
			time.Millisecond,
		)
		usage, err := ForwardAWSEventStream(response, reader, 0)
		if !errors.Is(err, ErrMaxStreamDuration) || !bytes.Equal(response.Body.Bytes(), chunk) {
			t.Fatalf("err=%v body=%x want=%x", err, response.Body.Bytes(), chunk)
		}
		_ = usage
	}
}

type singleAWSChunkReader struct {
	t     *testing.T
	chunk []byte
	read  bool
}

func (r *singleAWSChunkReader) Read(p []byte) (int, error) {
	if r.read {
		r.t.Fatal("started another upstream read after crossing duration")
	}
	r.read = true
	return copy(p, r.chunk), nil
}
