package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"golang.org/x/net/html"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func parseHTML(t *testing.T, s string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}
	return doc
}

func TestIsURL(t *testing.T) {
	cases := map[string]bool{
		"https://example.com/a": true,
		"http://example.com":    true,
		"example.com/a":         false,
		"/local/path.pdf":       false,
		"file.pdf":              false,
		"ftp://example.com":     false,
	}
	for in, want := range cases {
		if got := isURL(in); got != want {
			t.Errorf("isURL(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestRenderTextBlocksAndSpaces verifies block elements separate runs
// with newlines while inline runs keep word-separating spaces.
func TestRenderTextBlocksAndSpaces(t *testing.T) {
	doc := parseHTML(t, `<article><p>click <a>here</a> to read</p><ul><li>甲</li><li>乙</li></ul></article>`)
	articles := findAllByTag(doc, "article")
	if len(articles) != 1 {
		t.Fatalf("expected 1 article, got %d", len(articles))
	}
	got := renderText(articles[0])
	if want := "click here to read\n甲\n乙"; got != want {
		t.Errorf("renderText = %q, want %q", got, want)
	}
}

// TestWebBodyTextPrefersArticle verifies the article wins even when the
// rest of the body holds more text than the article itself.
func TestWebBodyTextPrefersArticle(t *testing.T) {
	doc := parseHTML(t, `<body><div>侧栏文字侧栏文字侧栏文字侧栏文字侧栏文字侧栏文字</div><article><p>正文内容。</p></article></body>`)
	got := webBodyText(doc)
	if !strings.Contains(got, "正文内容") {
		t.Errorf("article content missing: %q", got)
	}
	if strings.Contains(got, "侧栏") {
		t.Errorf("non-article body content leaked into article pick: %q", got)
	}
}

// TestWebBodyTextFallbacks verifies pages without <article> fall back
// to <main>, and pages with neither render the whole <body>.
func TestWebBodyTextFallbacks(t *testing.T) {
	doc := parseHTML(t, `<body><main><p>来自 main 的正文。</p></main></body>`)
	if got := webBodyText(doc); !strings.Contains(got, "来自 main 的正文") {
		t.Errorf("main fallback lost content: %q", got)
	}

	doc = parseHTML(t, `<body><nav>导航</nav><div><p>直接写在 body 里的正文。</p></div></body>`)
	got := webBodyText(doc)
	if !strings.Contains(got, "直接写在 body 里的正文") {
		t.Errorf("body fallback lost content: %q", got)
	}
	if strings.Contains(got, "导航") {
		t.Errorf("body fallback kept noise: %q", got)
	}
}

// TestExtractWebArticle exercises the full fetch → parse → distill →
// paginate pipeline against a local httptest server.
func TestExtractWebArticle(t *testing.T) {
	page := `<!doctype html><html><head><title>  测试文章 - 某站点 </title></head>
<body><header>站点导航 登录 注册</header><nav><a>首页</a> <a>文章</a></nav>
<article><h1>标题</h1><p>第一段正文内容，讲述一些事情。</p><p>第二段正文，继续讲述。</p></article>
<footer>版权所有 © 2026</footer><script>var x=1;</script><style>.a{color:red}</style></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	pages, title, err := extractWeb(srv.URL)
	if err != nil {
		t.Fatalf("extractWeb: %v", err)
	}
	if title != "测试文章 - 某站点" {
		t.Errorf("title = %q, want %q", title, "测试文章 - 某站点")
	}
	text := strings.Join(pages, "\n")
	if !strings.Contains(text, "第一段正文内容") || !strings.Contains(text, "第二段正文") {
		t.Errorf("article text missing: %q", text)
	}
	for _, noise := range []string{"站点导航", "首页", "版权所有", "var x", "color:red"} {
		if strings.Contains(text, noise) {
			t.Errorf("noise leaked into extracted text: %q", noise)
		}
	}
}

// TestExtractWebGBK verifies a legacy GBK page is decoded via its meta
// charset declaration.
func TestExtractWebGBK(t *testing.T) {
	page := `<html><head><meta charset="gbk"><title>中文标题</title></head><body><article><p>中文正文内容，GBK 编码。</p></article></body></html>`
	gbk, err := simplifiedchinese.GBK.NewEncoder().String(page)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Real legacy sites declare the charset in the response header
		// (httptest would otherwise sniff and label GBK bytes as utf-8).
		w.Header().Set("Content-Type", "text/html; charset=gbk")
		w.Write([]byte(gbk))
	}))
	defer srv.Close()

	pages, title, err := extractWeb(srv.URL)
	if err != nil {
		t.Fatalf("extractWeb: %v", err)
	}
	if title != "中文标题" {
		t.Errorf("title = %q, want %q", title, "中文标题")
	}
	if text := strings.Join(pages, "\n"); !strings.Contains(text, "中文正文内容") {
		t.Errorf("GBK text not decoded: %q", text)
	}
}

// TestExtractWebHTTPError verifies non-200 responses surface as errors.
func TestExtractWebHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, _, err := extractWeb(srv.URL); err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
}

// TestExtractDocumentDispatch verifies local paths fall through to
// extractText (and its errors) while URLs take the web path.
func TestExtractDocumentDispatch(t *testing.T) {
	_, title, err := extractDocument("/nonexistent.txt")
	if err == nil {
		t.Fatal("local path should surface extractText's error")
	}
	if title != "" {
		t.Errorf("local documents carry no title, got %q", title)
	}
}

// TestDocNamePrefersTitle verifies the status-line name prefers the
// document title over the file name and truncates wide titles.
func TestDocNamePrefersTitle(t *testing.T) {
	m := initialModel("/books/a.pdf")
	if got := m.docName(); got != "a.pdf" {
		t.Errorf("docName without title = %q, want %q", got, "a.pdf")
	}
	m.docTitle = "文章标题 - 站点名称"
	if got := m.docName(); got != "文章标题 - 站点名称" {
		t.Errorf("docName with title = %q", got)
	}
	m.docTitle = strings.Repeat("标题", 40) // 80 columns
	got := m.docName()
	if w := runewidth.StringWidth(got); w > 48 {
		t.Errorf("docName width = %d, want <= 48", w)
	}
}
