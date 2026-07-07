package mcp_bridge

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"

	"github.com/gofrs/uuid"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config

	mu       sync.Mutex
	sessions map[string]*session
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
    }
  },
  "required": ["command"]
}
`

type Config struct {
	BaseURI string   `json:"base_uri,omitempty"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

type session struct {
	id     string
	stdin  io.WriteCloser
	cancel context.CancelFunc
	events chan sseEvent
	done   chan struct{}
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

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})
}

func (p *Plugin) handleSSE(w http.ResponseWriter, r *http.Request) {
	sess, err := p.startSession(r.Context())
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	defer p.closeSession(sess.id)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	if !writeSSE(w, "endpoint", p.config.BaseURI+"/message?sessionId="+sess.id) {
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
			return
		}
	}
}

func (p *Plugin) handleMessage(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
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

	id := uuid.Must(uuid.NewV4()).String()
	sess := &session{
		id:     id,
		stdin:  stdin,
		cancel: cancel,
		events: make(chan sseEvent, 16),
		done:   make(chan struct{}),
	}

	p.mu.Lock()
	p.sessions[id] = sess
	p.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanPipe(stdout, "message", sess.events)
	}()
	go func() {
		defer wg.Done()
		scanStderr(stderr, sess.events)
	}()
	go func() {
		_ = cmd.Wait()
		wg.Wait()
		p.removeSession(id)
		close(sess.events)
		close(sess.done)
	}()

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

func (p *Plugin) closeSession(id string) {
	p.mu.Lock()
	sess := p.sessions[id]
	delete(p.sessions, id)
	p.mu.Unlock()

	if sess != nil {
		_ = sess.stdin.Close()
		sess.cancel()
	}
}

func (p *Plugin) removeSession(id string) {
	p.mu.Lock()
	delete(p.sessions, id)
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

	for _, sess := range sessions {
		_ = sess.stdin.Close()
		sess.cancel()
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

func scanPipe(pipe io.Reader, event string, events chan<- sseEvent) {
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		events <- sseEvent{event: event, data: scanner.Text()}
	}
}

func scanStderr(pipe io.Reader, events chan<- sseEvent) {
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
		events <- sseEvent{event: "message", data: string(body)}
	}
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
