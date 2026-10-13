package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// supportedExtensions is the whitelist checked at startup so unknown
// types fail fast in the terminal instead of inside the TUI.
var supportedExtensions = map[string]bool{
	".pdf": true, ".txt": true, ".text": true, ".md": true, ".markdown": true, ".log": true,
	".docx": true, ".odt": true, ".epub": true,
}

// linesPerPage sets the pseudo-page size for formats without real
// pages, so the page indicator and jump-to-page stay meaningful.
const linesPerPage = 40

// extractText returns the document's text as one string per page. PDFs
// have real pages; every other supported format is paginated into
// linesPerPage pseudo-pages.
func extractText(path string) ([]string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".pdf":
		return extractPDF(path)
	case ".txt", ".text", ".md", ".markdown", ".log":
		return extractPlain(path)
	case ".docx":
		return extractOfficeXML(path, "word/document.xml", docxSpec)
	case ".odt":
		return extractOfficeXML(path, "content.xml", odtSpec)
	case ".epub":
		return extractEpub(path)
	case ".doc", ".rtf":
		return nil, fmt.Errorf("暂不支持旧版 %s 格式，请先用 Word 另存为 .docx", ext)
	}
	return nil, fmt.Errorf("不支持的文件类型 %q（支持 pdf/txt/md/docx/odt/epub）", ext)
}

// extractPDF shells out to pdftotext (poppler), which handles CJK subset
// fonts far better than pure-Go PDF parsers.
func extractPDF(path string) ([]string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("pdftotext", "-layout", "-enc", "UTF-8", path, "-")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftotext: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.Split(stdout.String(), "\f"), nil
}

// extractPlain reads a text file, transparently handling UTF-8/UTF-16
// BOMs and GB18030 (superset of GBK), then pseudo-paginates.
func extractPlain(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return paginate(decodeText(raw)), nil
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// decodeText converts raw bytes to UTF-8: BOM sniffing first, then
// UTF-8 validation, falling back to GB18030 for legacy Chinese files.
func decodeText(raw []byte) string {
	switch {
	case bytes.HasPrefix(raw, utf8BOM):
		return string(raw[len(utf8BOM):])
	case bytes.HasPrefix(raw, []byte{0xFF, 0xFE}):
		return decodeUTF16(raw[2:], false)
	case bytes.HasPrefix(raw, []byte{0xFE, 0xFF}):
		return decodeUTF16(raw[2:], true)
	}
	if utf8.Valid(raw) {
		return string(raw)
	}
	if s, err := simplifiedchinese.GB18030.NewDecoder().Bytes(raw); err == nil {
		return string(s)
	}
	return string(raw)
}

// decodeUTF16 decodes UTF-16 code units (surrogate pairs included via
// utf16.Decode) in either byte order.
func decodeUTF16(b []byte, bigEndian bool) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return string(utf16.Decode(u))
}

// paginate splits single-flow text into pages of linesPerPage lines.
func paginate(text string) []string {
	lines := strings.Split(text, "\n")
	var pages []string
	for start := 0; start < len(lines); start += linesPerPage {
		end := start + linesPerPage
		if end > len(lines) {
			end = len(lines)
		}
		pages = append(pages, strings.Join(lines[start:end], "\n"))
	}
	return pages
}

// xmlTextSpec maps format-specific XML element local names onto text
// roles, so one token-stream parser serves both docx and odt.
type xmlTextSpec struct {
	line map[string]bool // closing one of these ends a line
	tab  map[string]bool // rendered as \t
	br   map[string]bool // rendered as \n
	text map[string]bool // only chardata inside these counts (nil: anywhere)
}

var docxSpec = xmlTextSpec{
	line: map[string]bool{"p": true},
	tab:  map[string]bool{"tab": true},
	br:   map[string]bool{"br": true, "cr": true},
	text: map[string]bool{"t": true},
}

var odtSpec = xmlTextSpec{
	line: map[string]bool{"p": true, "h": true},
	tab:  map[string]bool{"tab": true},
	br:   map[string]bool{"line-break": true},
	// Restricting chardata to paragraphs also drops the indentation
	// whitespace between elements, which would otherwise become noise.
	text: map[string]bool{"p": true, "h": true},
}

// extractOfficeXML reads one XML part from a docx/odt zip container and
// reconstitutes its text per the format's element names.
func extractOfficeXML(path, entry string, spec xmlTextSpec) ([]string, error) {
	raw, err := readZipEntry(path, entry)
	if err != nil {
		return nil, err
	}
	return paginate(parseXMLText(raw, spec)), nil
}

// parseXMLText rebuilds plain text from a docx/odt XML part using the
// token stream: chardata inside spec.text elements is kept, tab/br
// elements become whitespace, and paragraph closes become newlines.
func parseXMLText(data []byte, spec xmlTextSpec) string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var sb strings.Builder
	textDepth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break // tolerate a malformed tail, keep what we have
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "Fallback" {
				// Word duplicates content in Choice/Fallback pairs for
				// compatibility; taking only the Choice copy avoids
				// reading the same text twice.
				dec.Skip()
				continue
			}
			if spec.tab[t.Name.Local] {
				sb.WriteByte('\t')
			} else if spec.br[t.Name.Local] {
				sb.WriteByte('\n')
			}
			if spec.text[t.Name.Local] {
				textDepth++
			}
		case xml.EndElement:
			if spec.text[t.Name.Local] {
				textDepth--
			}
			if spec.line[t.Name.Local] {
				sb.WriteByte('\n')
			}
		case xml.CharData:
			if spec.text == nil || textDepth > 0 {
				sb.Write(t)
			}
		}
	}
	return sb.String()
}

// readZipEntry reads a single named file from a zip archive.
func readZipEntry(path, name string) ([]byte, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("打开 %s 失败: %w", filepath.Base(path), err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == name {
			return readZipFile(f)
		}
	}
	return nil, fmt.Errorf("%s 中找不到 %s", filepath.Base(path), name)
}

func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// extractEpub concatenates the chapters of an EPUB in spine order (the
// declared reading order), not filename order.
func extractEpub(path string) ([]string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("打开 %s 失败: %w", filepath.Base(path), err)
	}
	defer zr.Close()
	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[f.Name] = f
	}
	cf := files["META-INF/container.xml"]
	if cf == nil {
		return nil, fmt.Errorf("%s 缺少 META-INF/container.xml", filepath.Base(path))
	}
	rootfile, err := epubRootfile(cf)
	if err != nil {
		return nil, err
	}
	pf := files[rootfile]
	if pf == nil {
		return nil, fmt.Errorf("%s 缺少 %s", filepath.Base(path), rootfile)
	}
	chapters := epubSpine(pf, files)
	var text strings.Builder
	for _, f := range chapters {
		data, err := readZipFile(f)
		if err != nil {
			continue
		}
		text.WriteString(stripHTML(data))
		text.WriteByte('\n')
	}
	return paginate(text.String()), nil
}

// epubRootfile pulls the OPF package path out of container.xml.
func epubRootfile(f *zip.File) (string, error) {
	data, err := readZipFile(f)
	if err != nil {
		return "", err
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("解析 container.xml 失败: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "rootfile" {
			for _, a := range se.Attr {
				if a.Name.Local == "full-path" {
					return a.Value, nil
				}
			}
		}
	}
}

// epubSpine resolves the spine's idrefs through the manifest and
// returns the chapter zip entries in reading order. Manifest and spine
// are collected first and joined afterwards, so document order within
// the OPF does not matter.
func epubSpine(opf *zip.File, files map[string]*zip.File) []*zip.File {
	data, err := readZipFile(opf)
	if err != nil {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	href := map[string]string{}
	var order []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "item":
			id, h := "", ""
			for _, a := range se.Attr {
				switch a.Name.Local {
				case "id":
					id = a.Value
				case "href":
					h = a.Value
				}
			}
			href[id] = resolveZipPath(zipDir(opf.Name), h)
		case "itemref":
			for _, a := range se.Attr {
				if a.Name.Local == "idref" {
					order = append(order, a.Value)
				}
			}
		}
	}
	var chapters []*zip.File
	for _, id := range order {
		if f := files[href[id]]; f != nil {
			chapters = append(chapters, f)
		}
	}
	return chapters
}

func zipDir(path string) string {
	d := filepath.Dir(path)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

// resolveZipPath turns a manifest href (relative to the OPF, possibly
// URL-escaped) into an archive member name.
func resolveZipPath(dir, href string) string {
	if unescaped, err := url.PathUnescape(href); err == nil {
		href = unescaped
	}
	if dir == "" || strings.HasPrefix(href, "/") {
		return strings.TrimPrefix(href, "/")
	}
	return dir + "/" + href
}

var blockElems = map[string]bool{
	"p": true, "div": true, "li": true, "ul": true, "ol": true,
	"table": true, "tr": true, "td": true, "th": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"blockquote": true, "section": true, "article": true,
	"header": true, "footer": true, "nav": true, "aside": true,
}

// stripHTML extracts readable text from an XHTML chapter: chardata with
// newlines at block boundaries, head/script/style skipped, entities
// resolved.
func stripHTML(data []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity
	var sb strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break // tolerate a malformed tail, keep what we have
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "script", "style", "head":
				dec.Skip()
				continue
			case "br", "hr":
				sb.WriteByte('\n')
				continue
			}
			if blockElems[t.Name.Local] {
				sb.WriteByte('\n')
			}
		case xml.EndElement:
			if blockElems[t.Name.Local] {
				sb.WriteByte('\n')
			}
		case xml.CharData:
			sb.Write(t)
		}
	}
	return html.UnescapeString(sb.String())
}

// 中文标点按语义分三层：先按句末切分，块仍超长才降到句中标点，
// 再超长才降到短停顿，最后按宽度兜底。这样短句里的分号/逗号
// 不会被无谓地切碎，只有真正长到不好念的句子才会换气。
const (
	levelSentence = iota + 1 // 句末：。！？… 和 ASCII .!?
	levelClause              // 句中较大停顿：；： 和 ASCII ;:
	levelPhrase              // 句中短停顿：，、 和 ASCII ,
)

var endingsByLevel = [][]rune{
	levelSentence: {'。', '！', '？', '…', '.', '!', '?'},
	levelClause:   {'；', '：', ';', ':'},
	levelPhrase:   {'，', '、', ','},
}

// splitChunks breaks text into TTS chunks. Each paragraph is one chunk;
// only when a paragraph exceeds maxRunes do we descend into the
// hierarchical punctuation split. Soft newlines inside a paragraph are
// folded into spaces so a source-line wrap does not fragment the chunk.
func splitChunks(text string, maxRunes int) []string {
	if maxRunes <= 0 {
		maxRunes = 450
	}
	var chunks []string
	for _, para := range splitParagraphs(text) {
		if utf8.RuneCountInString(para) > maxRunes {
			chunks = append(chunks, splitAtLevel(para, levelSentence, maxRunes)...)
		} else {
			chunks = append(chunks, para)
		}
	}
	return chunks
}

// splitParagraphs splits text into paragraphs. A blank line always ends
// the paragraph; otherwise a line whose last rune is a sentence-final
// mark (。！？… .!?) also ends it. Other newlines are soft wraps and get
// folded into the paragraph as spaces, so pdftotext's hard-wrapped lines
// stay in the same chunk.
func splitParagraphs(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var paras []string
	var cur strings.Builder
	flush := func() {
		if s := squeezeCJKSpaces(strings.TrimSpace(cur.String())); s != "" {
			paras = append(paras, s)
		}
		cur.Reset()
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if cur.Len() > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(strings.TrimSpace(line))
		if endsSentence(cur.String()) {
			flush()
		}
	}
	flush()
	return paras
}

// squeezeCJKSpaces removes spaces adjacent to CJK runes. pdftotext pads
// Chinese lines with alignment spaces and splitParagraphs folds soft
// newlines into spaces, leaving gaps like 战 略 that mean nothing to a
// TTS engine. Spaces whose both neighbors are Latin are kept so English
// still segments into words.
func squeezeCJKSpaces(s string) string {
	runes := []rune(s)
	var out []rune
	for i := 0; i < len(runes); i++ {
		if !unicode.IsSpace(runes[i]) {
			out = append(out, runes[i])
			continue
		}
		j := i + 1
		for j < len(runes) && unicode.IsSpace(runes[j]) {
			j++
		}
		if (i > 0 && isCJK(runes[i-1])) || (j < len(runes) && isCJK(runes[j])) {
			i = j - 1
			continue
		}
		out = append(out, ' ') // collapse, e.g. "word    wrap" -> "word wrap"
		i = j - 1
	}
	return string(out)
}

// isCJK reports whether r is a CJK rune: Han characters, CJK/fullwidth
// punctuation, curly quotes, em dashes and ellipses — the runes next to
// which a space is layout noise rather than a word boundary.
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		(r >= 0x3000 && r <= 0x303F) || // CJK symbols & punct: 、。「」
		(r >= 0xFF01 && r <= 0xFF64) || // fullwidth: ，！？（）
		(r >= 0x2018 && r <= 0x201D) || // curly quotes ‘’“”
		r == '—' || r == '…' // ——、……
}

// endsSentence reports whether the last rune of s is a sentence-final
// mark — the same set splitOnEndings treats as level-1 boundaries.
func endsSentence(s string) bool {
	r := []rune(s)
	if len(r) == 0 {
		return false
	}
	switch r[len(r)-1] {
	case '。', '！', '？', '…', '.', '!', '?':
		return true
	}
	return false
}

// splitAtLevel segments s on the given level's punctuation. Any part that
// still exceeds maxRunes is recursed into the next level; past the last
// level we fall back to a width-based hard split.
func splitAtLevel(s string, level, maxRunes int) []string {
	if level > levelPhrase {
		return splitByWidth(s, maxRunes)
	}
	var out []string
	for _, part := range splitOnEndings(s, endingsByLevel[level]) {
		if utf8.RuneCountInString(part) > maxRunes {
			out = append(out, splitAtLevel(part, level+1, maxRunes)...)
		} else {
			out = append(out, part)
		}
	}
	return out
}

// splitOnEndings cuts a line on every rune in endings, attaching any
// trailing closing quotes/brackets so quoted sentences stay together. A
// '.' between two digits (3.14) is a decimal point, not a sentence end.
// Punctuation-only results are dropped: they would synthesize silence.
func splitOnEndings(line string, endings []rune) []string {
	endingsSet := make(map[rune]bool, len(endings))
	for _, r := range endings {
		endingsSet[r] = true
	}
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
		if !endingsSet[runes[i]] {
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

// splitByWidth is the final fallback: cut a punctuation-free (or
// otherwise unsplittable) string into maxRunes-wide pieces.
func splitByWidth(s string, maxRunes int) []string {
	runes := []rune(s)
	var out []string
	for i := 0; i < len(runes); i += maxRunes {
		end := i + maxRunes
		if end > len(runes) {
			end = len(runes)
		}
		if seg := strings.TrimSpace(string(runes[i:end])); seg != "" {
			out = append(out, seg)
		}
	}
	return out
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
