package main

import (
	"testing"

	"github.com/charmbracelet/bubbletea"
)

// TestPlayCurrentSetsPlayer is the regression test for the value-receiver
// bug: playCurrent used to assign m.player on a copy, so the real model
// never saw the player — every stop() guard was skipped (overlapping
// audio) and every playDoneMsg was filtered out (playback stalled).
func TestPlayCurrentSetsPlayer(t *testing.T) {
	m := initialModel("")
	m.chunks = []chunkInfo{{Text: "第一段", Page: 1}, {Text: "第二段", Page: 1}}
	m.tts = newTTSClient(t.TempDir())

	// fake path: ffplay starts, fails instantly, closes done — safe in tests
	m1, _ := m.playCurrent("/nonexistent-a.mp3")
	if m1.player == nil {
		t.Fatal("playCurrent did not set m.player — value-receiver regression")
	}

	// A second playCurrent must interrupt the first player and replace it.
	m2, _ := m1.playCurrent("/nonexistent-b.mp3")
	if m2.player == nil || m2.player == m1.player {
		t.Fatal("second playCurrent should install a new player")
	}
	if !m1.player.stopped {
		t.Fatal("old player was not stopped — old and new audio would overlap")
	}
}

// TestPlayDoneInterruptedDoesNotAdvance verifies that a playDoneMsg from a
// player we interrupted does not advance the cursor: the interrupter
// (skip/seek/replay) has already arranged the next step.
func TestPlayDoneInterruptedDoesNotAdvance(t *testing.T) {
	m := initialModel("")
	m.chunks = []chunkInfo{{Text: "a", Page: 1}, {Text: "b", Page: 1}}
	m.current = 0

	p := &player{done: make(chan struct{})}
	p.stopped = true // interrupted, not finished
	m.player = p

	updated, _ := m.Update(playDoneMsg{player: p})
	um := updated.(model)
	if um.current != 0 {
		t.Fatalf("interrupted player advanced the cursor: current=%d", um.current)
	}
	if um.player != nil {
		t.Fatal("interrupted player was not cleared from m.player")
	}
}

// TestPlayDoneNaturalAdvances verifies a player that finished on its own
// advances to the next chunk.
func TestPlayDoneNaturalAdvances(t *testing.T) {
	m := initialModel("")
	m.chunks = []chunkInfo{{Text: "a", Page: 1}, {Text: "b", Page: 1}}
	m.current = 0
	m.tts = newTTSClient(t.TempDir())

	p := &player{done: make(chan struct{})} // stopped=false → natural end
	m.player = p

	updated, _ := m.Update(playDoneMsg{player: p})
	um := updated.(model)
	if um.current != 1 {
		t.Fatalf("natural completion should advance to next chunk, got %d", um.current)
	}
}

// TestEnterReplaysCurrentChunk: in browsing mode (not started), pressing
// Enter begins the session and re-triggers fetch+play for the cursor
// chunk without moving it.
func TestEnterReplaysCurrentChunk(t *testing.T) {
	m := initialModel("")
	m.chunks = []chunkInfo{{Text: "a", Page: 1}, {Text: "b", Page: 1}}
	m.current = 1
	m.tts = newTTSClient(t.TempDir()) // must be set before Update: the fetch closure captures it

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	um := updated.(model)
	if cmd == nil {
		t.Fatal("Enter should trigger a replay command")
	}
	// current must not move: replay = same chunk
	if um.current != 1 {
		t.Fatalf("Enter replay moved the cursor: %d", um.current)
	}
	if !um.started {
		t.Fatal("Enter should start the playback session")
	}
	// The command, when executed, must be a fetch of chunk 1 (no audio in
	// prefetched yet).
	msg := cmd()
	if fm, ok := msg.(fetchedMsg); ok {
		if fm.idx != 1 {
			t.Fatalf("Enter should fetch current chunk 1, got %d", fm.idx)
		}
		if fm.err != nil {
			t.Fatalf("fetch failed: %v", fm.err)
		}
	} else if bm, ok := msg.(tea.BatchMsg); ok {
		// Batch of commands — find the fetchedMsg among them.
		found := false
		for _, c := range bm {
			if c == nil {
				continue
			}
			if sub := c(); sub != nil {
				if fm, ok := sub.(fetchedMsg); ok && fm.idx == 1 {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("Enter replay did not fetch chunk 1: %#v", msg)
		}
	} else {
		t.Fatalf("unexpected message from Enter: %#v", msg)
	}
}

// TestIdleDoesNotAutoplay: after loading, the app must stay silent —
// fetched audio for the cursor chunk is stashed, not played. Browsing
// with j moves the cursor without starting playback, and the first
// Enter then plays from wherever the cursor is.
func TestIdleDoesNotAutoplay(t *testing.T) {
	m := initialModel("")
	chunks := []chunkInfo{{Text: "a", Page: 1}, {Text: "b", Page: 1}}
	m.tts = newTTSClient(t.TempDir())

	// Load: idle, prefetch around 0, no playback.
	up, _ := m.Update(loadedMsg{chunks: chunks})
	um := up.(model)
	if um.status != statusIdle {
		t.Fatalf("expected idle after load, got status=%d", um.status)
	}

	// Fetch for chunk 0 completes while in browsing mode: stash only.
	up, _ = um.Update(fetchedMsg{idx: 0, path: "/nonexistent-0.mp3"})
	um = up.(model)
	if um.player != nil {
		t.Fatal("idle: fetched audio for cursor chunk must not auto-play")
	}
	if um.prefetched[0] != "/nonexistent-0.mp3" {
		t.Fatal("idle: fetched audio was not stashed in prefetched")
	}

	// Browse with j: cursor moves, still silent.
	up, _ = um.Update(tea.KeyMsg{Type: tea.KeyDown})
	um = up.(model)
	if um.current != 1 {
		t.Fatalf("j should move the cursor in browsing mode, got %d", um.current)
	}
	if um.player != nil {
		t.Fatal("j must not start playback in browsing mode")
	}

	// First Enter plays from the cursor (chunk 1), not from chunk 0.
	um.prefetched[1] = "/nonexistent-1.mp3"
	up, _ = um.Update(tea.KeyMsg{Type: tea.KeyEnter})
	um = up.(model)
	if !um.started {
		t.Fatal("Enter should start the session")
	}
	if um.current != 1 {
		t.Fatalf("playback should start from the cursor, got %d", um.current)
	}
	if um.player == nil {
		t.Fatal("Enter should install a player")
	}
}

// TestPauseResumeToggle: space pauses a playing chunk (SIGSTOP) and
// resumes it (SIGCONT); bubbletea reports the space key as " ".
func TestPauseResumeToggle(t *testing.T) {
	m := initialModel("")
	m.chunks = []chunkInfo{{Text: "a", Page: 1}}
	m.tts = newTTSClient(t.TempDir())
	m.started = true
	m.prefetched[0] = "/nonexistent.mp3"

	up, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	um := up.(model)
	if um.player == nil {
		t.Fatal("Enter should start playback")
	}

	// Space → paused.
	up, _ = um.Update(tea.KeyMsg{Type: tea.KeySpace})
	um = up.(model)
	if um.player == nil || !um.player.paused {
		t.Fatal("space should pause the active player")
	}
	if um.status != statusPaused {
		t.Fatalf("expected statusPaused, got %d", um.status)
	}

	// Space again → resumed.
	up, _ = um.Update(tea.KeyMsg{Type: tea.KeySpace})
	um = up.(model)
	if um.player == nil || um.player.paused {
		t.Fatal("second space should resume the player")
	}
	if um.status != statusPlaying {
		t.Fatalf("expected statusPlaying after resume, got %d", um.status)
	}
}
