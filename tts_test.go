package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEdgeTTSSynthesize verifies the new Edge TTS path produces a
// non-trivial mp3 file for a short Chinese sentence. Run with:
//
//	go test -run TestEdgeTTSSynthesize -v -timeout 90s
func TestEdgeTTSSynthesize(t *testing.T) {
	cacheDir, err := os.MkdirTemp("", "pdftts-edge-test-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(cacheDir)

	c := newTTSClient(cacheDir)
	text := "你好，这是 pdftts 切换到 Edge TTS 接口后的语音合成测试。"

	path, err := c.synthesize(text)
	if err != nil {
		t.Fatalf("synthesize failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat cached file: %v", err)
	}
	if info.Size() < 512 {
		t.Fatalf("cached mp3 unexpectedly small: %d bytes", info.Size())
	}
	abs, _ := filepath.Abs(path)
	t.Logf("OK: %s (%d bytes)", abs, info.Size())

	// Second call should hit the cache (same path, no network).
	if _, err := c.synthesize(text); err != nil {
		t.Fatalf("second synthesize (cache hit) failed: %v", err)
	}
}
