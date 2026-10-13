package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"unicode"
	"unicode/utf8"
)

// extractText shells out to pdftotext (poppler), which handles CJK subset
// fonts far better than pure-Go PDF parsers.
func extractText(path string) ([]string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("pdftotext", "-layout", "-enc", "UTF-8", path, "-")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftotext: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.Split(stdout.String(), "\f"), nil
}

// splitChunks breaks text into sentence-level chunks for TTS.
// Every line (from the PDF's own line breaks: titles, list items,
// soft-wrapped lines) forms its own chunk group; inside a line, every
// sentence-final punctuation (。！？…；plus ASCII equivalents) starts a
// new chunk. Overly long sentences are hard-split as a fallback.
func splitChunks(text string, maxRunes int) []string {
	if maxRunes <= 0 {
		maxRunes = 450
	}
	var chunks []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, sentence := range splitSentences(line) {
			if utf8.RuneCountInString(sentence) > maxRunes {
				chunks = append(chunks, hardSplit(sentence, maxRunes)...)
				continue
			}
			chunks = append(chunks, sentence)
		}
	}
	return chunks
}

// splitSentences cuts a line on EVERY sentence-final punctuation —
// short sentences become their own chunks too — and attaches trailing
// closing quotes/brackets so quoted sentences stay together. A '.' that
// sits between digits (3.14) is a decimal point, not a sentence end.
// Punctuation-only results are dropped: they would synthesize silence.
func splitSentences(line string) []string {
	endings := map[rune]bool{'。': true, '！': true, '？': true, '；': true, '…': true, '.': true, '!': true, '?': true, ';': true}
	closing := map[rune]bool{'"': true, '\'': true, '”': true, '’': true, '」': true, '』': true, '）': true, ')': true}

	runes := []rune(line)
	var parts []string
	var cur []rune
	for i := 0; i < len(runes); i++ {
		if runes[i] == '.' && i > 0 && i+1 < len(runes) &&
			unicode.IsDigit(runes[i-1]) && unicode.IsDigit(runes[i+1]) {
			cur = append(cur, runes[i])
			continue // decimal point, not a sentence end
		}
		cur = append(cur, runes[i])
		if !endings[runes[i]] {
			continue
		}
		// attach trailing closing quotes: “……。”) stays with the sentence
		for i+1 < len(runes) && closing[runes[i+1]] {
			i++
			cur = append(cur, runes[i])
		}
		if s := strings.TrimSpace(string(cur)); hasContent(s) {
			parts = append(parts, s)
		}
		cur = cur[:0]
	}
	if s := strings.TrimSpace(string(cur)); hasContent(s) {
		parts = append(parts, s)
	}
	return parts
}

// hasContent reports whether s contains anything besides punctuation and
// whitespace.
func hasContent(s string) bool {
	for _, r := range s {
		if !unicode.IsPunct(r) && !unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

// hardSplit cuts a single long sentence on commas, then by width.
func hardSplit(s string, maxRunes int) []string {
	var out []string
	runes := []rune(s)
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.TrimSpace(string(cur)))
			cur = cur[:0]
		}
	}
	for _, r := range runes {
		cur = append(cur, r)
		if (r == '，' || r == ',' || r == '、') && len(cur) >= maxRunes/2 {
			flush()
		} else if len(cur) >= maxRunes {
			flush()
		}
	}
	flush()
	return out
}

// chunkInfo carries chunk text plus which source page it came from.
type chunkInfo struct {
	Text string
	Page int
}

// buildChunks converts pages of text into indexed chunks with page numbers.
func buildChunks(pages []string, maxRunes int) []chunkInfo {
	var chunks []chunkInfo
	for i, page := range pages {
		for _, c := range splitChunks(page, maxRunes) {
			chunks = append(chunks, chunkInfo{Text: c, Page: i + 1})
		}
	}
	return chunks
}
