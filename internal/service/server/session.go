package server

import (
	"net/http"
	"strings"
	"time"

	"moonbridge/internal/foundation/session"
	"moonbridge/internal/service/api"
)

type serverSession struct {
	sess     *session.Session
	lastUsed time.Time
}

// sessionTTL returns the configured session TTL duration.
// Defaults to 24 hours if unset or unparseable.
func (server *Server) sessionTTL() time.Duration {
	raw := server.currentConfig().SessionTTL
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return 24 * time.Hour
}

// maxSessions returns the configured maximum number of active sessions.
// 0 means unlimited.
func (server *Server) maxSessions() int {
	return server.currentConfig().MaxSessions
}

func (server *Server) ListSessions() []api.SessionInfo {
	server.sessionsMu.Lock()
	defer server.sessionsMu.Unlock()
	result := make([]api.SessionInfo, 0, len(server.sessions))
	for key, entry := range server.sessions {
		result = append(result, api.SessionInfo{
			Key:       key,
			CreatedAt: entry.sess.CreatedAt.Format(time.RFC3339),
			LastUsed:  entry.lastUsed.Format(time.RFC3339),
		})
	}
	return result
}

func (server *Server) sessionForRequest(request *http.Request) *session.Session {
	key := sessionKeyFromRequest(request)
	if key == "" {
		sess := session.New()
		sess.InitExtensions(nil)
		return sess
	}

	now := time.Now()
	server.sessionsMu.Lock()
	defer server.sessionsMu.Unlock()

	server.pruneSessionsLocked(now)
	if entry, ok := server.sessions[key]; ok {
		entry.lastUsed = now
		server.sessions[key] = entry
		return entry.sess
	}

	// Enforce max sessions limit when creating a new session.
	if maxSessions := server.maxSessions(); maxSessions > 0 && len(server.sessions) >= maxSessions {
		// Return an ephemeral session (not stored) when the limit is reached.
		sess := session.NewWithID(key)
		sess.InitExtensions(nil)
		return sess
	}

	sess := session.NewWithID(key)
	sess.InitExtensions(nil)
	server.sessions[key] = serverSession{sess: sess, lastUsed: now}
	return sess
}

func (server *Server) pruneSessionsLocked(now time.Time) {
	ttl := server.sessionTTL()
	for key, entry := range server.sessions {
		if now.Sub(entry.lastUsed) > ttl {
			delete(server.sessions, key)
		}
	}
}

func sessionKeyFromRequest(request *http.Request) string {
	if value := strings.TrimSpace(request.Header.Get("Session_id")); value != "" {
		return "session:" + value
	}
	if value := strings.TrimSpace(request.Header.Get("X-Codex-Window-Id")); value != "" {
		return "codex-window:" + value
	}
	return ""
}

func (server *Server) startSessionPruning() {
	ticker := time.NewTicker(time.Hour) // prune every hour
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			server.pruneSessions()
		case <-server.sessionPruneStop:
			return
		}
	}
}

func (server *Server) pruneSessions() {
	now := time.Now()
	server.sessionsMu.Lock()
	defer server.sessionsMu.Unlock()
	ttl := server.sessionTTL()
	for key, entry := range server.sessions {
		if now.Sub(entry.lastUsed) > ttl {
			delete(server.sessions, key)
		}
	}
}
