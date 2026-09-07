package ai_stream

import (
	"errors"
	"io"
	"time"
)

var ErrMaxStreamDuration = errors.New("max_stream_duration_ms exceeded")

type durationReader struct {
	reader   io.Reader
	deadline time.Time
	exceeded bool
}

// LimitDuration checks the deadline after each upstream read, as APISIX does
// after processing a chunk. The chunk crossing the deadline remains readable;
// the following read terminates the stream without starting another I/O call.
func LimitDuration(reader io.Reader, started time.Time, maximum time.Duration) io.Reader {
	if maximum <= 0 {
		return reader
	}
	return &durationReader{reader: reader, deadline: started.Add(maximum)}
}

func (r *durationReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.exceeded {
		return 0, ErrMaxStreamDuration
	}
	n, err := r.reader.Read(p)
	if n > 0 && !time.Now().Before(r.deadline) {
		r.exceeded = true
		return n, nil
	}
	return n, err
}
