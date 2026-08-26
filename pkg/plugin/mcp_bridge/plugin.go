package mcp_bridge

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
)

type Plugin struct {
	base.BasePlugin
	config Config

	mu       sync.Mutex
	sessions map[string]*session

	pingInterval time.Duration
}

const (
	priority = 510
	name     = "mcp-bridge"
)

const schema = `
{
  "type": "object",
  "properties": {
    "base_uri": {
      "type": "string",
      "minLength": 1,
      "default": ""
    },
    "command": {
      "type": "string",
      "minLength": 1
    },
    "args": {
      "type": "array",
      "items": {
        "type": "string"
      },
      "minItems": 0
    },
    "max_body_size": {
      "type": "integer",
      "exclusiveMinimum": 0,
      "default": 1048576
    }
  },
  "required": ["command"]
}
`

type Config struct {
	BaseURI     string   `json:"base_uri,omitempty"`
	Command     string   `json:"command"`
	Args        []string `json:"args,omitempty"`
	MaxBodySize int      `json:"max_body_size,omitempty"`
}

type session struct {
	id     string
	stdin  io.WriteCloser
	cancel context.CancelFunc
	events chan sseEvent
	done   chan struct{}
	tasks  *runtime.RequestTaskGroup

	closeOnce     sync.Once
	closeDone     chan struct{}
	closeMu       sync.Mutex
	closePanicked bool
	closePanic    any
}

type sseEvent struct {
	event string
	data  string
}

type stderrNotification struct {
	JSONRPC string                 `json:"jsonrpc"`
	Method  string                 `json:"method"`
	Params  stderrNotificationBody `json:"params"`
}

type stderrNotificationBody struct {
	Content string `json:"content"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	p.config.BaseURI = strings.TrimRight(p.config.BaseURI, "/")
	if p.sessions == nil {
		p.sessions = map[string]*session{}
	}
	if p.pingInterval <= 0 {
		p.pingInterval = 30 * time.Second
	}
	if p.config.MaxBodySize <= 0 {
		p.config.MaxBodySize = base.DefaultRequestBodyMaxBytes
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apisixctx.GetRequestLifecycle(r) != nil {
			base.AdaptRequestPhase(p, next).ServeHTTP(w, r)
			return
		}
		p.serve(w, r)
	})
}

// RunRequestPhase owns MCP's local SSE/message gateway. MCP writes its SSE
// bytes directly; it does not install a second response-body wrapper.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceAPISIX)
	p.serve(w, r)
	return base.StopRequestWithSource(r, apisixctx.ResponseSourceAPISIX)
}

func (p *Plugin) serve(w http.ResponseWriter, r *http.Request) {
	action, ok := p.action(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch {
	case action == "sse" && r.Method == http.MethodGet:
		p.handleSSE(w, r)
	case action == "message" && r.Method == http.MethodPost:
		p.handleMessage(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (p *Plugin) handleSSE(w http.ResponseWriter, r *http.Request) {
	// The session is owned by this handler and is explicitly closed on
	// return; WithoutCancel preserves request-scoped values while allowing
	// the handler to drain buffered events before its explicit close joins
	// the child tasks. The stream ends when the child's events are exhausted,
	// a write fails (client disconnect), or the request context cancels after
	// draining any buffered events.
	sessionParent := context.WithoutCancel(r.Context())
	sess, err := p.startSession(sessionParent)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	defer p.closeSession(sess)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	if !writeSSE(w, "endpoint", p.config.BaseURI+"/message?sessionId="+sess.id) {
		return
	}
	pingTicker := time.NewTicker(p.pingInterval)
	defer pingTicker.Stop()
	pingID := 1
	if !writePing(w, pingID) {
		return
	}

	for {
		select {
		case event, ok := <-sess.events:
			if !ok {
				return
			}
			if !writeSSE(w, event.event, event.data) {
				return
			}
		case <-r.Context().Done():
			// The request context cancels when the client disconnects, but
			// it can also fire while the child's final output is still being
			// consumed. Drain whatever the child already produced so the
			// last event is not dropped.
			for {
				select {
				case event, ok := <-sess.events:
					if !ok {
						return
					}
					if !writeSSE(w, event.event, event.data) {
						return
					}
				default:
					return
				}
			}
		case <-pingTicker.C:
			pingID++
			if !writePing(w, pingID) {
				return
			}
		}
	}
}

func (p *Plugin) handleMessage(w http.ResponseWriter, r *http.Request) {
	apisixctx.RegisterSensitiveQueryName(r, "sessionId")
	body, err := base.ReadRequestBodyLimited(r, p.config.MaxBodySize)
	if base.IsBodyTooLarge(err) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil || len(body) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	sess := p.lookupSession(r.URL.Query().Get("sessionId"))
	if sess == nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if _, err := sess.stdin.Write(append(body, '\n')); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

func (p *Plugin) startSession(parent context.Context) (*session, error) {
	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, p.config.Command, p.config.Args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	id, err := newSessionID(crand.Reader)
	if err != nil {
		_ = stdin.Close()
		cancel()
		_ = cmd.Wait()
		return nil, err
	}
	tasks := runtime.NewRequestTaskGroup(ctx, "connection/mcp-bridge")
	sess := &session{
		id:        id,
		stdin:     stdin,
		cancel:    cancel,
		events:    make(chan sseEvent, 16),
		done:      make(chan struct{}),
		tasks:     tasks,
		closeDone: make(chan struct{}),
	}

	stdoutDone := make(chan struct{}, 1)
	stderrDone := make(chan struct{}, 1)
	registered := make(chan struct{})
	accepted := 0
	if err := admitSessionTask(tasks, func(ctx context.Context) error {
		defer func() { stdoutDone <- struct{}{} }()
		scanPipeForSession(ctx, stdout, "message", sess.events)
		return nil
	}); err != nil {
		return nil, p.failStartSession(sess, cmd, stdin, accepted, err)
	}
	accepted++
	if err := admitSessionTask(tasks, func(ctx context.Context) error {
		defer func() { stderrDone <- struct{}{} }()
		scanStderrForSession(ctx, stderr, sess.events)
		return nil
	}); err != nil {
		return nil, p.failStartSession(sess, cmd, stdin, accepted, err)
	}
	accepted++
	if err := admitSessionTask(tasks, func(context.Context) error {
		// Wait for the scanners to consume the pipes to EOF before reaping
		// the command: Cmd.Wait closes the stdout/stderr pipes, which would
		// otherwise discard any buffered output the child produced.
		<-stdoutDone
		<-stderrDone
		_ = cmd.Wait()
		<-registered
		p.removeSession(sess)
		close(sess.events)
		close(sess.done)
		return nil
	}); err != nil {
		return nil, p.failStartSession(sess, cmd, stdin, accepted, err)
	}

	p.mu.Lock()
	p.sessions[id] = sess
	p.mu.Unlock()
	close(registered)

	return sess, nil
}

func (p *Plugin) lookupSession(id string) *session {
	if id == "" {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[id]
}

func (p *Plugin) closeSession(sess *session) {
	if sess == nil {
		return
	}

	p.mu.Lock()
	if current := p.sessions[sess.id]; current == sess {
		delete(p.sessions, sess.id)
	}
	p.mu.Unlock()

	sess.close()
}

func (p *Plugin) removeSession(sess *session) {
	p.mu.Lock()
	if current := p.sessions[sess.id]; current == sess {
		delete(p.sessions, sess.id)
	}
	p.mu.Unlock()
}

func (p *Plugin) closeAll() {
	p.mu.Lock()
	sessions := make([]*session, 0, len(p.sessions))
	for _, sess := range p.sessions {
		sessions = append(sessions, sess)
	}
	p.sessions = map[string]*session{}
	p.mu.Unlock()

	var firstPanic any
	panicked := false
	for _, sess := range sessions {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil && !panicked {
					panicked = true
					firstPanic = recovered
				}
			}()
			sess.close()
		}()
	}
	if panicked {
		panic(firstPanic)
	}
}

func (p *Plugin) failStartSession(
	sess *session,
	cmd *exec.Cmd,
	stdin io.WriteCloser,
	accepted int,
	setupErr error,
) error {
	p.removeSession(sess)
	_ = stdin.Close()
	sess.cancel()
	var firstPanic any
	panicked := false
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				panicked = true
				firstPanic = recovered
			}
		}()
		_ = sess.tasks.Wait()
	}()
	if accepted < 3 {
		_ = cmd.Wait()
		close(sess.events)
		close(sess.done)
	}
	close(sess.closeDone)
	if panicked {
		panic(firstPanic)
	}
	return setupErr
}

func (sess *session) close() {
	sess.closeOnce.Do(func() {
		defer close(sess.closeDone)
		recoverCleanup := func(cleanup func()) (panicked bool, panicValue any) {
			defer func() {
				if recovered := recover(); recovered != nil {
					panicked = true
					panicValue = recovered
				}
			}()
			cleanup()
			return false, nil
		}
		cleanupPanicked, cleanupPanic := recoverCleanup(func() { _ = sess.stdin.Close() })
		if panicked, panicValue := recoverCleanup(sess.cancel); panicked && !cleanupPanicked {
			cleanupPanicked = true
			cleanupPanic = panicValue
		}
		taskPanicked, taskPanic := recoverCleanup(func() { _ = sess.tasks.Wait() })
		if taskPanicked {
			sess.closeMu.Lock()
			sess.closePanicked = true
			sess.closePanic = taskPanic
			sess.closeMu.Unlock()
		} else if cleanupPanicked {
			sess.closeMu.Lock()
			sess.closePanicked = true
			sess.closePanic = cleanupPanic
			sess.closeMu.Unlock()
		}
	})

	<-sess.closeDone
	sess.closeMu.Lock()
	panicked := sess.closePanicked
	panicValue := sess.closePanic
	sess.closeMu.Unlock()
	if panicked {
		panic(panicValue)
	}
}

func (p *Plugin) action(path string) (string, bool) {
	baseURI := p.config.BaseURI
	if baseURI == "" {
		if path == "/sse" {
			return "sse", true
		}
		if path == "/message" {
			return "message", true
		}
		return "", false
	}

	prefix := baseURI + "/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(path, prefix), true
}

var (
	scanPipeForSession   = scanPipe
	scanStderrForSession = scanStderr
	admitSessionTask     = func(tasks *runtime.RequestTaskGroup, run func(context.Context) error) error {
		return tasks.Go(run)
	}
)

func scanPipe(ctx context.Context, pipe io.Reader, event string, events chan<- sseEvent) {
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if !sendEvent(ctx, events, sseEvent{event: event, data: scanner.Text()}) {
			return
		}
	}
}

func scanStderr(ctx context.Context, pipe io.Reader, events chan<- sseEvent) {
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		body, err := json.Marshal(stderrNotification{
			JSONRPC: "2.0",
			Method:  "notifications/stderr",
			Params:  stderrNotificationBody{Content: scanner.Text()},
		})
		if err != nil {
			continue
		}
		if !sendEvent(ctx, events, sseEvent{event: "message", data: string(body)}) {
			return
		}
	}
}

func newSessionID(reader io.Reader) (string, error) {
	id, err := uuid.NewGenWithOptions(uuid.WithRandomReader(reader)).NewV4()
	if err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return id.String(), nil
}

func sendEvent(ctx context.Context, events chan<- sseEvent, event sseEvent) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func writePing(w http.ResponseWriter, id int) bool {
	return writeSSE(w, "message", fmt.Sprintf(`{"jsonrpc":"2.0","method":"ping","id":"ping:%d"}`, id))
}

func writeSSE(w http.ResponseWriter, event string, data string) bool {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return false
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}
