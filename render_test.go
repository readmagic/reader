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

// TestSplitOnEndings verifies the level-1 (sentence-final) split: every
// 。！？… and ASCII .!? starts a new chunk, closing quotes stay attached,
// decimals are not split, and punctuation-only fragments are dropped.
// The semicolon case confirms ； is NOT a sentence-final mark in Chinese
// — it stays inside the sentence and only gets split later if the line
// is too long (see TestSplitHierarchical).
func TestSplitOnEndings(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"basic", "第一句。第二句！", []string{"第一句。", "第二句！"}},
		{"question", "问题？答案。", []string{"问题？", "答案。"}},
		{"semicolon_kept", "前半；后半。", []string{"前半；后半。"}},
		{"english", "One. Two! Three?", []string{"One.", "Two!", "Three?"}},
		{"short_sentence", "好。行！", []string{"好。", "行！"}},
		{"quote_closing", "他说：“你好。”然后走了。", []string{"他说：“你好。”", "然后走了。"}},
		{"paren_closing", "（参见附录。）完了。", []string{"（参见附录。）", "完了。"}},
		{"decimal_not_split", "价格是3.14元。", []string{"价格是3.14元。"}},
		{"no_final_punct", "第三章 实验设计", []string{"第三章 实验设计"}},
		{"punct_only_dropped", "前文。　。后文。", []string{"前文。", "后文。"}},
	}
	for _, c := range cases {
		got := splitOnEndings(c.in, endingsByLevel[levelSentence])
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: splitOnEndings(%q) = %#v, want %#v", c.name, c.in, got, c.want)
		}
	}
}

// TestSplitHierarchical verifies the layered fallback: a short line splits
// only on 。！？, a too-long line descends to ；：, then to ，、, and a
// punctuation-free over-long line falls back to width-based hard split.
func TestSplitHierarchical(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		max     int
		want    []string
	}{
		{"l1_short_no_semicolon_split", "前半；后半。", 100, []string{"前半；后半。"}},
		{"l2_descend_to_semicolon", "前半部分内容介绍；后半部分内容总结。", 10, []string{"前半部分内容介绍；", "后半部分内容总结。"}},
		{"l3_descend_to_comma", "片段一，片段二，片段三。", 4, []string{"片段一，", "片段二，", "片段三。"}},
		{"fallback_width", "abcdefghij", 4, []string{"abcd", "efgh", "ij"}},
	}
	for _, c := range cases {
		got := splitAtLevel(c.in, levelSentence, c.max)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: splitAtLevel(%q, %d) = %#v, want %#v", c.name, c.in, c.max, got, c.want)
		}
	}
}

// TestBuildChunks verifies the paragraph-level grouping: lines in the
// same paragraph fold into one chunk, sentence-final line ends start a
// new paragraph, and the page index survives the split.
func TestBuildChunks(t *testing.T) {
	pages := []string{
		"第一章 绪论\n\n本章介绍背景。研究意义显著！\n相关工作综述如下。",
		"第二章 方法",
	}
	chunks := buildChunks(pages, 20)
	if len(chunks) != 4 {
		t.Fatalf("expected 4 paragraph-level chunks, got %d: %#v", len(chunks), chunks)
	}
	expect := []string{
		"第一章绪论",
		"本章介绍背景。研究意义显著！",
		"相关工作综述如下。",
		"第二章方法",
	}
	for i, want := range expect {
		if chunks[i].Text != want {
			t.Errorf("chunk %d = %q, want %q", i, chunks[i].Text, want)
		}
	}
	expectPages := []int{1, 1, 1, 2}
	for i, want := range expectPages {
		if chunks[i].Page != want {
			t.Errorf("chunk %d page = %d, want %d", i, chunks[i].Page, want)
		}
	}
}

// TestSplitParagraphs covers the paragraph heuristics: blank-line breaks,
// sentence-final line endings, and soft-wrap folding.
func TestSplitParagraphs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"blank_line_break", "段一\n\n段二", []string{"段一", "段二"}},
		{"sentence_final_ends_para", "第一句。\n第二句！", []string{"第一句。", "第二句！"}},
		{"mid_line_punct_kept", "第一句。第二句！", []string{"第一句。第二句！"}},
		{"soft_wrap_folded", "软换行\n继续。", []string{"软换行继续。"}},
		{"no_final_punct_holds", "标题\n第二行", []string{"标题第二行"}},
		{"cjk_space_squeezed", "本身成了战 略工具。", []string{"本身成了战略工具。"}},
		{"pdf_layout_padding", "制造吸引力—— 邀请制、排队机制。", []string{"制造吸引力——邀请制、排队机制。"}},
		{"latin_spaces_kept", "the quick fox runs.", []string{"the quick fox runs."}},
		{"number_cjk_squeezed", "3.8 排期。", []string{"3.8排期。"}},
		{"latin_run_collapsed", "word    wrap 好。", []string{"word wrap好。"}},
		{"empty_input", "", nil},
		{"windows_crlf", "段一\r\n\r\n段二", []string{"段一", "段二"}},
	}
	for _, c := range cases {
		got := splitParagraphs(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: splitParagraphs(%q) = %#v, want %#v", c.name, c.in, got, c.want)
		}
	}
}

// TestSplitChunksParagraph tests that a paragraph shorter than maxRunes
// stays one chunk, while a paragraph longer than maxRunes descends into
// the hierarchical sentence split.
func TestSplitChunksParagraph(t *testing.T) {
	t.Run("short_para_one_chunk", func(t *testing.T) {
		got := splitChunks("短句一。短句二！短句三？", 100)
		if want := []string{"短句一。短句二！短句三？"}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("long_para_descends_to_sentences", func(t *testing.T) {
		got := splitChunks("第一句。第二句！第三句？", 5)
		if want := []string{"第一句。", "第二句！", "第三句？"}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("paragraphs_not_merged", func(t *testing.T) {
		got := splitChunks("段一。\n\n段二。", 100)
		if want := []string{"段一。", "段二。"}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	// The exact shape pdftotext produces: hard-wrapped CJK lines plus
	// layout padding spaces, e.g. 战 略 split across a line break.
	t.Run("pdf_hard_wrap_and_padding_squeezed", func(t *testing.T) {
		got := splitChunks("本身成了战\n略工具。稀缺可以制造吸引\n力—— 邀请制。", 100)
		if want := []string{"本身成了战略工具。稀缺可以制造吸引力——邀请制。"}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})
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
