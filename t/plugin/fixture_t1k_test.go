package pluginintegration

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestT1KFixtureDecodesHTTPRequestFrames(t *testing.T) {
	var wire bytes.Buffer
	for _, frame := range []t1kFixtureFrame{
		{tag: 0x41, payload: []byte("POST /hello HTTP/1.1\r\nHost: waf.example\r\nContent-Length: 3\r\n\r\n")},
		{tag: 0x02, payload: []byte("abc")},
		{tag: 0x20, payload: []byte("Proto:2\n")},
		{tag: 0x83, payload: []byte("UUID:fixture\n")},
	} {
		if err := writeT1KFixtureFrame(&wire, frame); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}

	request, extra, err := readT1KFixtureRequest(&wire)
	if err != nil {
		t.Fatalf("readT1KFixtureRequest() error = %v", err)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	if request.Method != http.MethodPost || request.URL.RequestURI() != "/hello" || string(body) != "abc" {
		t.Fatalf("decoded request = %s %s %q", request.Method, request.URL.RequestURI(), body)
	}
	if got := extra["UUID"]; got != "fixture" {
		t.Fatalf("decoded EXTRA UUID = %q, want fixture", got)
	}
}

func TestT1KFixtureWritesPassAndRejectDecisions(t *testing.T) {
	for _, test := range []struct {
		name       string
		response   HTTPResponse
		wantAction string
		wantStatus string
		wantEvent  string
	}{
		{name: "pass", response: HTTPResponse{Status: http.StatusOK, Body: `{"status":200}`}, wantAction: "."},
		{
			name:       "reject",
			response:   HTTPResponse{Status: http.StatusForbidden, Body: `{"status":403,"event_id":"event-123"}`},
			wantAction: "?",
			wantStatus: "403",
			wantEvent:  "event-123",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var wire bytes.Buffer
			if err := writeT1KFixtureDecision(&wire, test.response); err != nil {
				t.Fatalf("writeT1KFixtureDecision() error = %v", err)
			}
			frames, err := readT1KFixtureFrames(&wire)
			if err != nil {
				t.Fatalf("read response frames: %v", err)
			}
			if got := string(frames[0].payload); got != test.wantAction {
				t.Fatalf("action = %q, want %q", got, test.wantAction)
			}
			if test.wantStatus == "" {
				if len(frames) != 2 || frames[1].tag != 0xa5 {
					t.Fatalf("pass frames = %#v, want HEAD plus terminal metadata", frames)
				}
				return
			}
			if len(frames) != 3 || string(frames[1].payload) != test.wantStatus ||
				!bytes.Contains(frames[2].payload, []byte(test.wantEvent)) {
				t.Fatalf("reject frames = %#v, want status %s and event %s", frames, test.wantStatus, test.wantEvent)
			}
		})
	}
}

type t1kFixtureFrame struct {
	tag     byte
	payload []byte
}

type t1kFixture struct {
	listener  net.Listener
	respond   []HTTPResponse
	requests  chan capturedRequest
	errors    chan error
	done      chan struct{}
	closeOnce sync.Once
	sequence  sync.Mutex
	next      int
	wg        sync.WaitGroup
}

func startT1KFixture(spec FixtureSpec) (namedFixture, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen T1K fixture: %w", err)
	}
	fixture := &t1kFixture{
		listener: listener,
		respond:  spec.Respond,
		requests: make(chan capturedRequest, len(spec.Respond)+len(spec.Expect)+1),
		errors:   make(chan error, len(spec.Respond)+1),
		done:     make(chan struct{}),
	}
	fixture.wg.Add(1)
	go fixture.serve()
	return fixture, nil
}

func (f *t1kFixture) serve() {
	defer f.wg.Done()
	for {
		connection, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.done:
				return
			default:
			}
			f.reportError(fmt.Errorf("accept T1K fixture connection: %w", err))
			return
		}
		f.wg.Go(func() { f.handleConnection(connection) })
	}
}

func (f *t1kFixture) handleConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	request, extra, err := readT1KFixtureRequest(connection)
	if err != nil {
		f.reportError(err)
		return
	}
	captured, err := captureFixtureRequest(request)
	if err != nil {
		f.reportError(fmt.Errorf("capture T1K fixture request: %w", err))
		return
	}
	captured.t1kExtra = extra
	f.requests <- captured

	f.sequence.Lock()
	responseIndex := f.next
	if f.next < len(f.respond)-1 {
		f.next++
	}
	f.sequence.Unlock()
	response := f.respond[responseIndex]
	if response.Delay > 0 {
		time.Sleep(response.Delay)
	}
	if err := writeT1KFixtureDecision(connection, response); err != nil {
		f.reportError(err)
	}
}

func readT1KFixtureRequest(reader io.Reader) (*http.Request, map[string]string, error) {
	frames, err := readT1KFixtureFrames(reader)
	if err != nil {
		return nil, nil, fmt.Errorf("read T1K fixture request: %w", err)
	}
	if len(frames) < 3 || frames[0].tag != 0x41 || frames[len(frames)-1].tag != 0x83 {
		return nil, nil, fmt.Errorf("invalid T1K request frame sequence")
	}
	bodyIndex := -1
	versionIndex := 1
	if len(frames) == 4 {
		if frames[1].tag != 0x02 {
			return nil, nil, fmt.Errorf("invalid T1K BODY frame tag 0x%02x", frames[1].tag)
		}
		bodyIndex = 1
		versionIndex = 2
	}
	if frames[versionIndex].tag != 0x20 || string(frames[versionIndex].payload) != "Proto:2\n" {
		return nil, nil, fmt.Errorf("invalid T1K protocol version frame")
	}
	request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(frames[0].payload)))
	if err != nil {
		return nil, nil, fmt.Errorf("parse T1K HEAD request: %w", err)
	}
	extra, err := parseT1KFixtureExtra(frames[len(frames)-1].payload)
	if err != nil {
		return nil, nil, err
	}
	body := []byte(nil)
	if bodyIndex >= 0 {
		body = frames[bodyIndex].payload
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	return request, extra, nil
}

func parseT1KFixtureExtra(payload []byte) (map[string]string, error) {
	values := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSuffix(string(payload), "\n"), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("parse T1K EXTRA line %q", line)
		}
		if _, duplicate := values[key]; duplicate {
			return nil, fmt.Errorf("parse T1K EXTRA duplicate field %q", key)
		}
		values[key] = value
	}
	return values, nil
}

func readT1KFixtureFrames(reader io.Reader) ([]t1kFixtureFrame, error) {
	frames := make([]t1kFixtureFrame, 0, 4)
	for len(frames) < 8 {
		var header [5]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return nil, fmt.Errorf("read T1K frame header: %w", err)
		}
		length := binary.LittleEndian.Uint32(header[1:])
		if length > 1<<20 {
			return nil, fmt.Errorf("T1K frame length %d exceeds fixture limit", length)
		}
		payload := make([]byte, int(length))
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil, fmt.Errorf("read T1K frame payload: %w", err)
		}
		frames = append(frames, t1kFixtureFrame{tag: header[0], payload: payload})
		if header[0]&0x80 != 0 {
			return frames, nil
		}
	}
	return nil, fmt.Errorf("T1K request did not terminate within 8 frames")
}

func writeT1KFixtureDecision(writer io.Writer, response HTTPResponse) error {
	decision := struct {
		Status  int    `json:"status"`
		EventID string `json:"event_id"`
	}{Status: response.Status}
	if response.Body != "" {
		if err := json.Unmarshal([]byte(response.Body), &decision); err != nil {
			return fmt.Errorf("decode T1K fixture response: %w", err)
		}
	}
	if decision.Status == 0 {
		decision.Status = http.StatusOK
	}
	if decision.Status == http.StatusOK {
		if err := writeT1KFixtureFrame(writer, t1kFixtureFrame{tag: 0x41, payload: []byte(".")}); err != nil {
			return err
		}
		return writeT1KFixtureFrame(writer, t1kFixtureFrame{
			tag: 0xa5, payload: []byte(`{"event_id":"fixture","request_hit_whitelist":false}`),
		})
	}
	if decision.EventID == "" {
		return fmt.Errorf("T1K reject fixture response requires event_id")
	}
	for _, frame := range []t1kFixtureFrame{
		{tag: 0x41, payload: []byte("?")},
		{tag: 0x02, payload: fmt.Appendf(nil, "%d", decision.Status)},
		{tag: 0xa4, payload: []byte("<!-- event_id: " + decision.EventID + " -->")},
	} {
		if err := writeT1KFixtureFrame(writer, frame); err != nil {
			return err
		}
	}
	return nil
}

func writeT1KFixtureFrame(writer io.Writer, frame t1kFixtureFrame) error {
	var header [5]byte
	header[0] = frame.tag
	binary.LittleEndian.PutUint32(header[1:], uint32(len(frame.payload)))
	for _, data := range [][]byte{header[:], frame.payload} {
		for len(data) > 0 {
			written, err := writer.Write(data)
			if err != nil {
				return fmt.Errorf("write T1K fixture frame: %w", err)
			}
			if written == 0 {
				return io.ErrShortWrite
			}
			data = data[written:]
		}
	}
	return nil
}

func (f *t1kFixture) address() string { return f.listener.Addr().String() }
func (f *t1kFixture) url() string     { return "t1k://" + f.address() }

func (f *t1kFixture) host() string {
	host, _, err := net.SplitHostPort(f.address())
	if err != nil {
		return ""
	}
	return host
}

func (f *t1kFixture) port() string {
	_, port, err := net.SplitHostPort(f.address())
	if err != nil {
		return ""
	}
	return port
}

func (f *t1kFixture) close() {
	f.closeOnce.Do(func() {
		close(f.done)
		_ = f.listener.Close()
		f.wg.Wait()
	})
}

func (f *t1kFixture) reportError(err error) {
	select {
	case f.errors <- err:
	case <-f.done:
	default:
	}
}

func (f *t1kFixture) assert(t *testing.T, spec FixtureSpec) {
	t.Helper()
	for index, expected := range spec.Expect {
		select {
		case received := <-f.requests:
			assertUpstreamRequest(t, expected, received)
		case <-time.After(2 * time.Second):
			t.Errorf("fixture %s did not receive expected request %d", spec.Name, index+1)
		}
	}
	select {
	case err := <-f.errors:
		t.Errorf("fixture %s: %v", spec.Name, err)
	default:
	}
	if len(spec.Expect) > 0 || spec.ExpectRequests != nil {
		select {
		case extra := <-f.requests:
			t.Errorf("fixture %s received unexpected request %s %s", spec.Name, extra.method, extra.path)
		default:
		}
	}
}

var _ namedFixture = (*t1kFixture)(nil)
