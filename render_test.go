package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestWrapText covers East-Asian wide runes and explicit newlines, the
// two cases that would visibly break the reading view if mishandled.
func TestWrapText(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		width  int
		expect []string
	}{
		{"pure_ascii_wrap", "hello", 3, []string{"hel", "lo"}},
		{"cjk_fits", "你好", 4, []string{"你好"}},
		{"cjk_wrap", "你好", 3, []string{"你", "好"}},
		{"mixed_lead_half", "a你好", 4, []string{"a你", "好"}},
		{"honor_newline", "first\nsecond", 10, []string{"first", "second"}},
		{"empty_input", "", 5, []string{""}},
	}
	for _, c := range cases {
		got := wrapText(c.in, c.width)
		if !reflect.DeepEqual(got, c.expect) {
			t.Errorf("%s: wrapText(%q,%d) = %#v, want %#v", c.name, c.in, c.width, got, c.expect)
		}
	}
}

// TestSplitSentences verifies sentence-level splitting: every
// sentence-final punctuation starts a new chunk (short sentences
// included), closing quotes stay attached, decimals are not split,
// and punctuation-only fragments are dropped.
func TestSplitSentences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"basic", "第一句。第二句！", []string{"第一句。", "第二句！"}},
		{"question", "问题？答案。", []string{"问题？", "答案。"}},
		{"semicolon", "前半；后半。", []string{"前半；", "后半。"}},
		{"english", "One. Two! Three?", []string{"One.", "Two!", "Three?"}},
		{"short_sentence", "好。行！", []string{"好。", "行！"}},
		{"quote_closing", "他说：“你好。”然后走了。", []string{"他说：“你好。”", "然后走了。"}},
		{"paren_closing", "（参见附录。）完了。", []string{"（参见附录。）", "完了。"}},
		{"decimal_not_split", "价格是3.14元。", []string{"价格是3.14元。"}},
		{"no_final_punct", "第三章 实验设计", []string{"第三章 实验设计"}},
		{"punct_only_dropped", "前文。　。后文。", []string{"前文。", "后文。"}},
	}
	for _, c := range cases {
		got := splitSentences(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: splitSentences(%q) = %#v, want %#v", c.name, c.in, got, c.want)
		}
	}
}

// TestBuildChunks verifies that lines (titles etc.) become independent
// chunks, sentences inside a line split individually, and the page
// index survives the split.
func TestBuildChunks(t *testing.T) {
	pages := []string{
		"第一章 绪论\n\n本章介绍背景。研究意义显著！\n相关工作综述如下。",
		"第二章 方法",
	}
	chunks := buildChunks(pages, 20)
	if len(chunks) != 5 {
		t.Fatalf("expected 5 sentence-level chunks, got %d: %#v", len(chunks), chunks)
	}
	expect := []string{"第一章 绪论", "本章介绍背景。", "研究意义显著！", "相关工作综述如下。", "第二章 方法"}
	for i, want := range expect {
		if chunks[i].Text != want {
			t.Errorf("chunk %d = %q, want %q", i, chunks[i].Text, want)
		}
	}
	expectPages := []int{1, 1, 1, 1, 2}
	for i, want := range expectPages {
		if chunks[i].Page != want {
			t.Errorf("chunk %d page = %d, want %d", i, chunks[i].Page, want)
		}
	}
}

// TestReadingViewFullPage verifies that readingView renders every chunk
// on the current page (not just the playing one) and that the rendered
// height always matches the requested viewport.
func TestReadingViewFullPage(t *testing.T) {
	// Three short chunks all on page 1, plus one on page 2.
	chunks := []chunkInfo{
		{Text: "第一段内容", Page: 1},
		{Text: "第二段内容", Page: 1},
		{Text: "第三段内容", Page: 1},
		{Text: "另一页的内容", Page: 2},
	}

	m := initialModel("")
	m.chunks = chunks
	m.width = 30
	m.height = 10

	for cur := 0; cur < 3; cur++ {
		m.current = cur
		view := m.readingView()
		lines := strings.Split(view, "\n")

		// height-2 padding to fill viewport exactly.
		if len(lines) != m.height-2 {
			t.Errorf("current=%d: expected %d rendered lines, got %d", cur, m.height-2, len(lines))
		}

		// Every page-1 chunk's text must appear somewhere in the output,
		// regardless of which chunk is currently playing.
		for _, want := range []string{"第一段内容", "第二段内容", "第三段内容"} {
			if !strings.Contains(view, want) {
				t.Errorf("current=%d: page text missing from view: %q", cur, want)
			}
		}
		// Page 2 content must never leak in.
		if strings.Contains(view, "另一页的内容") {
			t.Errorf("current=%d: foreign page content leaked into view", cur)
		}
	}
}
