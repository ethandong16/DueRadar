package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// sessionLifetime keeps the admin console signed in for two weeks without
// weakening the per-request server-side session check.
const sessionLifetime = 15 * 24 * time.Hour

type storedSession struct {
	Key     string    `json:"key"`
	Expires time.Time `json:"expires"`
}

type sessionFile struct {
	Version  int             `json:"version"`
	Sessions []storedSession `json:"sessions"`
}

// sessionKey hashes the cookie value so the data directory never contains a
// usable bearer token.
func sessionKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// EnableSessionPersistence loads previously issued admin sessions from the
// data directory and keeps later logins there, so a restart does not sign the
// user out.
func (s *Server) EnableSessionPersistence(dir string) error {
	s.sessionsPath = filepath.Join(dir, "sessions.json")
	data, err := os.ReadFile(s.sessionsPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var file sessionFile
	if json.Unmarshal(data, &file) != nil || file.Version != 1 {
		// Sessions are disposable; a damaged file must not block startup.
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, stored := range file.Sessions {
		if stored.Key != "" && stored.Expires.After(now) {
			s.sessions[stored.Key] = session{expires: stored.Expires}
		}
	}
	return nil
}

// persistSessionsLocked writes the live session map. Callers hold s.mu.
func (s *Server) persistSessionsLocked() {
	if s.sessionsPath == "" {
		return
	}
	now := time.Now()
	file := sessionFile{Version: 1, Sessions: make([]storedSession, 0, len(s.sessions))}
	for key, value := range s.sessions {
		if value.expires.After(now) {
			file.Sessions = append(file.Sessions, storedSession{Key: key, Expires: value.expires})
		}
	}
	data, err := json.Marshal(file)
	if err != nil {
		log.Printf("session persistence failed: %v", err)
		return
	}
	if err := writeSessionsFile(s.sessionsPath, data); err != nil {
		log.Printf("session persistence failed: %v", err)
	}
}

func writeSessionsFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".sessions-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err == nil {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(name, path)
}
