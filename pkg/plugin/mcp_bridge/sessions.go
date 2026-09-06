package mcp_bridge

import "sync"

// SessionRegistry lets requests in successor generations reach an existing SSE
// session. The SSE request still owns the child process and its task group.
// The compiler shares this registry only within its owned HTTP process domain.
type SessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*session
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{sessions: make(map[string]*session)}
}

func (p *Plugin) SetSessionRegistry(state *SessionRegistry) {
	p.state = state
}

func (state *SessionRegistry) Close() {
	state.mu.Lock()
	sessions := make([]*session, 0, len(state.sessions))
	for _, sess := range state.sessions {
		sessions = append(sessions, sess)
	}
	state.sessions = map[string]*session{}
	state.mu.Unlock()

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
