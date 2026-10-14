package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// isURL reports whether target is a web address rather than a local
// file path.
func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// extractDocument is the source-agnostic entry point behind loadDoc:
// web addresses are fetched and distilled to their main text, local
// files go through extractText.
func extractDocument(path string) (pages []string, title string, err error) {
	if isURL(path) {
		return extractWeb(path)
	}
	pages, err = extractText(path)
	return pages, "", err
}

const (
	// maxHTMLBytes bounds how much of a page we are willing to read;
	// real articles are far below this.
	maxHTMLBytes = 10 << 20
	// browserUA: many sites answer "Go-http-client" with a 403, so the
	// fetch presents itself as an ordinary browser.
	browserUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// extractWeb downloads url and returns its article text pseudo-paginated,
// plus the page title for the status line.
func extractWeb(url string) (pages []string, title string, err error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("无效网址 %s: %w", url, err)
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("获取 %s 失败: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("获取 %s 失败: HTTP %s", url, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxHTMLBytes))
	if err != nil {
		return nil, "", fmt.Errorf("读取 %s 失败: %w", url, err)
	}
	// Read into memory first so a charset sniff failure can fall back to
	// the raw bytes instead of a half-drained body.
	r, err := charset.NewReader(bytes.NewReader(raw), resp.Header.Get("Content-Type"))
	if err != nil {
		r = bytes.NewReader(raw)
	}
	doc, err := html.Parse(r)
	if err != nil {
		return nil, "", fmt.Errorf("解析 %s 失败: %w", url, err)
	}
	title = pageTitle(doc)
	text := webBodyText(doc)
	if text == "" {
		return nil, title, fmt.Errorf("网页中没有找到可朗读的正文")
	}
	return paginate(text), title, nil
}

// noiseTags are elements whose whole subtree is navigation, control or
// layout chrome rather than reading content.
var noiseTags = map[string]bool{
	"script": true, "style": true, "head": true, "noscript": true,
	"template": true, "svg": true, "iframe": true, "form": true,
	"button": true, "select": true, "nav": true, "footer": true,
	"aside": true, "header": true, "figure": true,
}

// webBlockTags extends the shared blockElems with tags common on web
// pages but absent from EPUB chapters.
var webBlockTags = func() map[string]bool {
	m := make(map[string]bool, len(blockElems)+5)
	for k := range blockElems {
		m[k] = true
	}
	for _, k := range []string{"pre", "dl", "dt", "dd", "figcaption", "main"} {
		m[k] = true
	}
	return m
}()

// webBodyText returns the page's main reading content. Article pages
// usually mark it up as <article> (or <main>); pages with neither fall
// back to the whole <body>, which still skips noise tags during
// rendering. Among several candidates the textually richest wins.
func webBodyText(doc *html.Node) string {
	for _, tag := range []string{"article", "main", "body"} {
		best := ""
		for _, n := range findAllByTag(doc, tag) {
			if s := renderText(n); len(s) > len(best) {
				best = s
			}
		}
		if best != "" {
			return best
		}
	}
	return ""
}

// renderText walks the subtree depth-first collecting readable text:
// noise subtrees are skipped, block boundaries become newlines, and
// adjacent inline runs are joined with a single space (English words
// split by inline tags stay separated; squeezeCJKSpaces later removes
// the spaces CJK text does not want).
func renderText(n *html.Node) string {
	var r textRenderer
	r.walk(n)
	return r.sb.String()
}

type textRenderer struct {
	sb      strings.Builder
	pending bool // a block boundary waiting to become a newline
}

func (r *textRenderer) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		r.text(strings.Join(strings.Fields(n.Data), " "))
	case html.ElementNode:
		if noiseTags[n.Data] {
			return
		}
		if n.Data == "br" {
			r.pending = true
			return
		}
		block := webBlockTags[n.Data]
		if block {
			r.pending = true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			r.walk(c)
		}
		if block {
			r.pending = true
		}
	}
}

// text appends s: one pending block boundary becomes a newline,
// otherwise the run is separated from the previous one by a space.
func (r *textRenderer) text(s string) {
	if s == "" {
		return
	}
	switch {
	case r.pending:
		if r.sb.Len() > 0 {
			r.sb.WriteByte('\n')
		}
		r.pending = false
	case r.sb.Len() > 0:
		r.sb.WriteByte(' ')
	}
	r.sb.WriteString(s)
}

// pageTitle returns the page's <title>, whitespace-collapsed.
func pageTitle(doc *html.Node) string {
	for _, n := range findAllByTag(doc, "title") {
		if c := n.FirstChild; c != nil && c.Type == html.TextNode {
			return strings.Join(strings.Fields(c.Data), " ")
		}
	}
	return ""
}

func findAllByTag(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == tag {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}
