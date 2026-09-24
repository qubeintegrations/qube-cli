// Package config holds what `qube login` stores: one session per host, the host to use by
// default, and a default app per session. Tokens live in the user's config directory with
// 0600 permissions -- never in the repo, never printed.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Session struct {
	Token          string `json:"token"`
	ExpiresAt      string `json:"expires_at"`
	Scope          string `json:"scope"`
	UserEmail      string `json:"user_email"`
	Organization   string `json:"organization"`
	DefaultApp     string `json:"default_app,omitempty"`
	DefaultAppName string `json:"default_app_name,omitempty"`
}

// Expired reports whether the server would refuse this session's token (best effort: an
// unparsable expiry counts as not expired and the server has the last word).
func (s Session) Expired() bool {
	t, err := time.Parse(time.RFC3339, s.ExpiresAt)
	return err == nil && time.Now().After(t)
}

type File struct {
	Version     int                `json:"version"`
	CurrentHost string             `json:"current_host,omitempty"` // the host used when --host is absent
	Sessions    map[string]Session `json:"sessions"`               // keyed by host, e.g. https://qubesync.com
}

const DefaultHost = "https://qubesync.com"

func Path() (string, error) {
	if p := os.Getenv("QUBE_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "qube", "credentials.json"), nil
}

func Load() (*File, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	f := &File{Version: 1, Sessions: map[string]Session{}}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, err
	}
	if f.Sessions == nil {
		f.Sessions = map[string]Session{}
	}
	return f, nil
}

// Save writes the file atomically (temp file + rename) with owner-only permissions, so an
// interrupted write never leaves a half-written credentials file behind.
func (f *File) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(p, data, 0o600)
}

// WriteFileAtomic writes data to a temporary file next to path and renames it into place.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(mode); err != nil && !isWindows() {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func isWindows() bool { return os.PathSeparator == '\\' }

// ResolveHost picks the host for this invocation: an explicit --host or QUBE_HOST wins;
// then the host of the last login (or the one chosen with `qube use --host`); then the
// only stored session; then production.
func (f *File) ResolveHost(explicit string) string {
	if explicit != "" {
		return NormalizeHost(explicit)
	}
	if f.CurrentHost != "" {
		if _, ok := f.Sessions[f.CurrentHost]; ok {
			return f.CurrentHost
		}
	}
	if len(f.Sessions) == 1 {
		for h := range f.Sessions {
			return h
		}
	}
	return DefaultHost
}

// NormalizeHost turns "dev.qubesync.com" or "https://dev.qubesync.com/" into "https://dev.qubesync.com".
func NormalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return DefaultHost
	}
	if !strings.HasPrefix(h, "http://") && !strings.HasPrefix(h, "https://") {
		h = "https://" + h
	}
	return strings.TrimRight(h, "/")
}
