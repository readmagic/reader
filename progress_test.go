package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"
)

// TestProgressRoundTrip verifies set writes through to disk and a store
// freshly opened on the same path sees the saved position.
func TestProgressRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "progress.json")
	s := openProgressStore(path)
	s.set("/books/a.pdf", fileProgress{Current: 7, Total: 20})

	s2 := openProgressStore(path)
	p, ok := s2.get("/books/a.pdf")
	if !ok || p.Current != 7 || p.Total != 20 {
		t.Fatalf("get after reopen = %+v, %v; want {Current:7 Total:20}, true", p, ok)
	}
	// The write must go through the atomic rename, leaving no tmp file.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("leftover tmp file: %v", err)
	}
}

// TestProgressOverwrite verifies a second set for the same key replaces
// the position instead of accumulating entries.
func TestProgressOverwrite(t *testing.T) {
	s := openProgressStore(filepath.Join(t.TempDir(), "progress.json"))
	s.set("/a.pdf", fileProgress{Current: 1, Total: 5})
	s.set("/a.pdf", fileProgress{Current: 4, Total: 5})
	if p, _ := s.get("/a.pdf"); p.Current != 4 {
		t.Fatalf("get = %+v, want Current 4", p)
	}
}

// TestProgressUnwritableStore verifies a store whose directory cannot be
// created degrades to memory-only: set must neither panic nor write.
func TestProgressUnwritableStore(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openProgressStore(filepath.Join(blocker, "nested", "progress.json"))
	if s.path != "" {
		t.Fatalf("expected memory-only store, got path %q", s.path)
	}
	s.set("/a.pdf", fileProgress{Current: 1, Total: 2})
	if p, ok := s.get("/a.pdf"); !ok || p.Current != 1 {
		t.Fatalf("memory-only get = %+v, %v; want {Current:1}, true", p, ok)
	}
}

// TestProgressCorruptFile verifies an unparseable progress file degrades
// to an empty store instead of failing the open.
func TestProgressCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openProgressStore(path)
	if _, ok := s.get("/a.pdf"); ok {
		t.Fatal("corrupt store should expose no entries")
	}
}

// TestLoadedMsgResumesPosition verifies the open path: a stored position
// in bounds moves the cursor and raises the resume notice, while a zero
// or out-of-bounds position is ignored.
func TestLoadedMsgResumesPosition(t *testing.T) {
	chunks := []chunkInfo{{Page: 1}, {Page: 2}, {Page: 2}, {Page: 3}}
	cases := []struct {
		name        string
		saved       fileProgress
		wantCursor  int
		wantResumed bool
	}{
		{"in_bounds", fileProgress{Current: 2, Total: 4}, 2, true},
		{"zero_is_the_top", fileProgress{Current: 0, Total: 4}, 0, false},
		{"out_of_bounds_ignored", fileProgress{Current: 9, Total: 4}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openProgressStore(filepath.Join(t.TempDir(), "progress.json"))
			s.set("/book.pdf", c.saved)
			m := initialModel("/book.pdf")
			m.progress = s

			updated, _ := m.Update(loadedMsg{chunks: chunks})
			um := updated.(model)
			if um.current != c.wantCursor {
				t.Errorf("current = %d, want %d", um.current, c.wantCursor)
			}
			if um.resumed != c.wantResumed {
				t.Errorf("resumed = %v, want %v", um.resumed, c.wantResumed)
			}
		})
	}
}

// TestMoveCursorSavesProgress verifies every cursor move writes the
// position through, so progress survives a crash between key presses.
func TestMoveCursorSavesProgress(t *testing.T) {
	s := openProgressStore(filepath.Join(t.TempDir(), "progress.json"))
	m := initialModel("/book.pdf")
	m.progress = s
	m.chunks = []chunkInfo{{Page: 1}, {Page: 1}, {Page: 1}}

	_, _ = moveCursor(m, 2)
	p, ok := s.get("/book.pdf")
	if !ok || p.Current != 2 || p.Total != 3 {
		t.Fatalf("progress after move = %+v, %v; want {Current:2 Total:3}, true", p, ok)
	}
}

// TestQuitSavesProgress verifies the quit path persists the cursor even
// when it has not moved since load.
func TestQuitSavesProgress(t *testing.T) {
	s := openProgressStore(filepath.Join(t.TempDir(), "progress.json"))
	m := initialModel("/book.pdf")
	m.progress = s
	m.chunks = []chunkInfo{{Page: 1}, {Page: 2}}
	m.current = 1

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q should quit")
	}
	_ = updated
	p, ok := s.get("/book.pdf")
	if !ok || p.Current != 1 || p.Total != 2 {
		t.Fatalf("progress after quit = %+v, %v; want {Current:1 Total:2}, true", p, ok)
	}
}

// TestResumeNoticeClearsOnKey verifies the "已恢复上次位置" notice shows
// after resuming and any subsequent key press clears it.
func TestResumeNoticeClearsOnKey(t *testing.T) {
	m := initialModel("/book.pdf")
	m.chunks = []chunkInfo{{Page: 1}, {Page: 2}}
	m.current = 1
	m.resumed = true

	view := m.statusLine()
	if !strings.Contains(view, "已恢复上次位置") {
		t.Fatalf("status line should carry the resume notice: %q", view)
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	um := updated.(model)
	if um.resumed {
		t.Fatal("any key press should clear the resume notice")
	}
	if strings.Contains(um.statusLine(), "已恢复上次位置") {
		t.Fatalf("notice should be gone from the status line: %q", um.statusLine())
	}
}

// TestSaveProgressWithoutStore verifies the no-store (tests) and
// no-chunks paths are silent no-ops.
func TestSaveProgressWithoutStore(t *testing.T) {
	m := initialModel("/book.pdf")
	m.saveProgress() // progress == nil: must not panic

	s := openProgressStore(filepath.Join(t.TempDir(), "progress.json"))
	m.progress = s
	m.saveProgress() // chunks empty: nothing recorded
	if _, ok := s.get("/book.pdf"); ok {
		t.Fatal("empty document should not write progress")
	}
}
