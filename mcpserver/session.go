package mcpserver

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

type Session struct {
	ID        string
	CreatedAt time.Time
	LastActive time.Time

	mu      sync.Mutex
	context map[string]string
}

func (s *Session) touch() {
	s.mu.Lock()
	s.LastActive = time.Now()
	s.mu.Unlock()
}

func (s *Session) Get(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.context[key]
}

func (s *Session) Set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.context[key] = value
}

func (s *Session) snapshot() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := make(map[string]string, len(s.context))
	for k, v := range s.context {
		ctx[k] = v
	}
	return SessionInfo{
		ID:         s.ID,
		CreatedAt:  s.CreatedAt,
		LastActive: s.LastActive,
		Context:    ctx,
	}
}

type SessionInfo struct {
	ID         string            `json:"id"`
	CreatedAt  time.Time         `json:"created_at"`
	LastActive time.Time         `json:"last_active"`
	Context    map[string]string `json:"context"`
}

type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewSessionManager() *SessionManager {
	return &SessionManager{sessions: make(map[string]*Session)}
}

func (m *SessionManager) Create() *Session {
	id := randomID()
	now := time.Now()
	s := &Session{
		ID:         id,
		CreatedAt:  now,
		LastActive: now,
		context:    make(map[string]string),
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	return s
}

func (m *SessionManager) Get(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

func (m *SessionManager) Remove(id string) bool {
	m.mu.Lock()
	_, ok := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	return ok
}

func (m *SessionManager) List() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.snapshot())
	}
	return out
}

func (m *SessionManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
