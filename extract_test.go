package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDecodeText covers the encoding sniffing matrix for plain text:
// BOM detection for UTF-8/UTF-16 in both byte orders, and GB18030 for
// legacy Chinese files without a BOM.
func TestDecodeText(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"utf8_plain", []byte("你好"), "你好"},
		{"utf8_bom", append([]byte{0xEF, 0xBB, 0xBF}, []byte("你好")...), "你好"},
		{"utf16le_bom", []byte{0xFF, 0xFE, 0x60, 0x4F, 0x7D, 0x59}, "你好"},
		{"utf16be_bom", []byte{0xFE, 0xFF, 0x4F, 0x60, 0x59, 0x7D}, "你好"},
		{"gb18030", []byte{0xC4, 0xE3, 0xBA, 0xC3}, "你好"},
	}
	for _, c := range cases {
		if got := decodeText(c.in); got != c.want {
			t.Errorf("%s: decodeText = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestPaginate checks pseudo-page splitting at the 40-line boundary.
func TestPaginate(t *testing.T) {
	lines := make([]string, linesPerPage*2+5)
	for i := range lines {
		lines[i] = "行"
	}
	pages := paginate(strings.Join(lines, "\n"))
	if len(pages) != 3 {
		t.Fatalf("expected 3 pages, got %d", len(pages))
	}
	if got := strings.Count(pages[0], "行"); got != linesPerPage {
		t.Errorf("page 0 has %d lines, want %d", got, linesPerPage)
	}
	if got := strings.Count(pages[2], "行"); got != 5 {
		t.Errorf("last page has %d lines, want 5", got)
	}
}

// TestParseXMLTextDocx mirrors a minimal word/document.xml: tabs, run
// breaks, the xml:space-preserved trailing space, and an empty
// self-closing paragraph.
func TestParseXMLTextDocx(t *testing.T) {
	src := `<?xml version="1.0"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>
<w:p><w:r><w:t>第一段</w:t></w:r><w:r><w:tab/></w:r><w:r><w:t>after tab</w:t></w:r></w:p>
<w:p><w:r><w:t xml:space="preserve">第二段 </w:t></w:r><w:r><w:br/></w:r><w:r><w:t>br 后</w:t></w:r></w:p>
<w:p/>
</w:body></w:document>`
	got := parseXMLText([]byte(src), docxSpec)
	want := "第一段\tafter tab\n第二段 \nbr 后\n\n"
	if got != want {
		t.Errorf("docx parse:\n got %q\nwant %q", got, want)
	}
}

// TestParseXMLTextOdt mirrors a minimal content.xml: headings, nested
// spans, line breaks and tabs inside paragraphs. Chardata outside
// paragraphs (the indentation between tags) must be ignored.
func TestParseXMLTextOdt(t *testing.T) {
	src := `<?xml version="1.0"?>
<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0">
  <office:body>
    <office:text>
      <text:h>标题</text:h>
      <text:p>第一<text:span>段</text:span>内容<text:line-break/>换行<text:tab/>结束</text:p>
    </office:text>
  </office:body>
</office:document-content>`
	got := parseXMLText([]byte(src), odtSpec)
	want := "标题\n第一段内容\n换行\t结束\n"
	if got != want {
		t.Errorf("odt parse:\n got %q\nwant %q", got, want)
	}
}

// TestExtractOfficeAndEpub builds real zip containers and drives the
// full extractText dispatch, including epub spine order being respected
// over filename order and head/style content being skipped.
func TestExtractOfficeAndEpub(t *testing.T) {
	dir := t.TempDir()

	docx := filepath.Join(dir, "样本.docx")
	writeZip(t, docx, map[string]string{
		"[Content_Types].xml": "<Types/>",
		"word/document.xml":   `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>你好 docx。</w:t></w:r></w:p></w:body></w:document>`,
	})

	odt := filepath.Join(dir, "样本.odt")
	writeZip(t, odt, map[string]string{
		"mimetype": "application/vnd.oasis.opendocument.text",
		"content.xml": `<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"><office:body><office:text><text:p>你好 odt。</text:p></office:text></office:body></office:document-content>`,
	})

	// Spine order deliberately differs from filename order.
	epub := filepath.Join(dir, "样本.epub")
	writeZip(t, epub, map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": `<container><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`,
		"OEBPS/content.opf":      `<package xmlns="http://www.idpf.org/2007/opf"><manifest><item id="c1" href="chap1.xhtml" media-type="application/xhtml+xml"/><item id="c2" href="chap2.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="c2"/><itemref idref="c1"/></spine></package>`,
		"OEBPS/chap1.xhtml":      `<html><head><title>ignore</title><style>p{}</style></head><body><p>第一章</p></body></html>`,
		"OEBPS/chap2.xhtml":      `<html><body><p>第二章</p></body></html>`,
	})

	pages, err := extractText(docx)
	if err != nil || !strings.Contains(strings.Join(pages, "\n"), "你好 docx。") {
		t.Errorf("docx: pages=%q err=%v", pages, err)
	}
	pages, err = extractText(odt)
	if err != nil || !strings.Contains(strings.Join(pages, "\n"), "你好 odt。") {
		t.Errorf("odt: pages=%q err=%v", pages, err)
	}
	pages, err = extractText(epub)
	if err != nil {
		t.Fatalf("epub: %v", err)
	}
	all := strings.Join(pages, "\n")
	if strings.Index(all, "第二章") > strings.Index(all, "第一章") {
		t.Errorf("epub spine order not respected: %q", all)
	}
	if strings.Contains(all, "ignore") || strings.Contains(all, "{}") {
		t.Errorf("epub head/style content leaked: %q", all)
	}
}

// TestExtractPlainDispatch covers .txt dispatch end-to-end with a
// GB18030-encoded file, plus the unsupported-extension error path.
func TestExtractPlainDispatch(t *testing.T) {
	dir := t.TempDir()
	txt := filepath.Join(dir, "gb.txt")
	// "你好。\n重新。" encoded in GB18030.
	if err := os.WriteFile(txt, []byte{0xC4, 0xE3, 0xBA, 0xC3, 0xA1, 0xA3, '\n', 0xD6, 0xD8, 0xD0, 0xC2, 0xA1, 0xA3}, 0o644); err != nil {
		t.Fatal(err)
	}
	pages, err := extractText(txt)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(pages, "\n"); got != "你好。\n重新。" {
		t.Errorf("txt extract = %q, want 你好。重新。", got)
	}

	bad := filepath.Join(dir, "x.xyz")
	if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractText(bad); err == nil {
		t.Error("expected error for unsupported extension")
	}

	doc := filepath.Join(dir, "x.doc")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractText(doc); err == nil {
		t.Error("expected error for legacy .doc")
	}
}

func writeZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
