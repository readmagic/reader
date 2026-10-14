package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// fileProgress records where reading stopped in one document: the chunk
// index plus the chunk count at save time, so a later open can sanity
// check the index against a possibly changed document.
type fileProgress struct {
	Current int `json:"current"`
	Total   int `json:"total"`
}

// progressStore persists reading positions for every document the user
// has opened, in one JSON map keyed by absolute file path.
type progressStore struct {
	// path is the JSON file the map is written to; empty means the
	// location was unusable and the store is memory-only.
	path    string
	entries map[string]fileProgress
}

// newProgressStore opens the store at the OS config location
// (~/.config/reader/progress.json on Linux).
func newProgressStore() *progressStore {
	dir, err := os.UserConfigDir()
	if err != nil {
		return &progressStore{entries: map[string]fileProgress{}}
	}
	return openProgressStore(filepath.Join(dir, "reader", "progress.json"))
}

// openProgressStore loads (or starts) the store at path. A directory
// that cannot be created or a file that cannot be parsed both degrade
// to an empty store rather than an error: unreadable progress must
// never keep a document from opening.
func openProgressStore(path string) *progressStore {
	s := &progressStore{path: path, entries: map[string]fileProgress{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.path = "" // memory-only
		return s
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.entries)
	}
	if s.entries == nil {
		s.entries = map[string]fileProgress{}
	}
	return s
}

func (s *progressStore) get(filePath string) (fileProgress, bool) {
	p, ok := s.entries[filePath]
	return p, ok
}

// set records the position and writes through to disk atomically
// (write a tmp file, then rename) so a crash mid-save cannot corrupt
// the store. Progress is best-effort: write failures are silent.
// bubbletea processes messages on one goroutine, so set is never
// called concurrently.
func (s *progressStore) set(filePath string, p fileProgress) {
	s.entries[filePath] = p
	if s.path == "" {
		return
	}
	data, err := json.Marshal(s.entries)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// saveProgress persists the cursor position for this document so the
// next open resumes where reading stopped.
func (m model) saveProgress() {
	if m.progress == nil || len(m.chunks) == 0 {
		return
	}
	m.progress.set(m.filePath, fileProgress{Current: m.current, Total: len(m.chunks)})
}

// savedProgress returns the stored position for this document, if any.
func (m model) savedProgress() (fileProgress, bool) {
	if m.progress == nil {
		return fileProgress{}, false
	}
	return m.progress.get(m.filePath)
}
