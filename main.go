package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

const (
	statusIdle = iota
	statusPlaying
	statusFetching
	statusPaused
	statusError
	statusDone
)

const (
	// chunkMaxRunes caps each synthesis request well below Edge TTS's
	// 1500-char soft limit so a chunk always renders in a single request
	// and stays readable on a narrow terminal.
	chunkMaxRunes = 450
	// leadAhead is how many chunks past the playhead we keep warm in the
	// prefetch cache. Sentence-level chunks play for only a few seconds
	// each, so three in flight keeps synthesis comfortably ahead.
	leadAhead = 3
)

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	pageStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	currentStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	doneStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241")) // 已读段：淡灰
	normalStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("252")) // 未读段：正常
	hintStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	fetchingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

type model struct {
	chunks    []chunkInfo
	current   int
	width     int
	height    int
	player    *player
	tts       *ttsClient
	filePath  string
	status    int
	errMsg    string
	gPressed  bool
	pageInput string
	// started flips true on the first Enter and stays true: before that
	// the app is silent (browsing mode) and fetched audio is only stashed.
	started bool
	// prefetched holds locally-cached mp3 paths for chunks synthesized
	// ahead of the playhead.
	prefetched map[int]string
	// pending tracks chunk indexes whose fetch is already in flight so
	// ensureFetch is idempotent under concurrent prefetch.
	pending map[int]bool
	// progress persists reading positions between sessions; nil disables
	// resume (tests, or an unusable config location).
	progress *progressStore
	// resumed flags that this open restored a saved position; it shows a
	// notice in the status line until the first key press clears it.
	resumed bool
	// docTitle names the document in the status line once known (a web
	// page's <title>); empty falls back to the file name.
	docTitle string
}

// moveCursor stops playback, moves the cursor to newIdx (clamped), and
// either starts playing the chunk there (once the session has started)
// or just keeps the prefetch window warm around the cursor (browsing
// mode, before the first Enter).
func moveCursor(m model, newIdx int) (model, tea.Cmd) {
	if m.player != nil {
		m.player.stop()
	}
	if newIdx < 0 {
		newIdx = 0
	}
	if newIdx >= len(m.chunks) {
		newIdx = len(m.chunks) - 1
	}
	m.current = newIdx
	// Persist on every move so a crash mid-session still resumes close
	// to where reading stopped.
	m.saveProgress()
	if m.started {
		m.status = statusFetching
		return playOrFetch(m, m.current)
	}
	return m, prefetchAround(m, m.current)
}

// prefetchAround warms the cache for idx and the next leadAhead chunks
// without starting playback. Used while browsing before the first Enter,
// so that pressing Enter later starts instantly from any cursor spot.
func prefetchAround(m model, idx int) tea.Cmd {
	var cmds []tea.Cmd
	for i := 0; i <= leadAhead; i++ {
		if c := ensureFetch(m, idx+i); c != nil {
			cmds = append(cmds, c)
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// indexOfPage parses a page number string and returns the index of the
// first chunk on that page, or -1 if the page has no chunks.
func (m model) indexOfPage(input string) int {
	n := 0
	for _, r := range input {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	for i, c := range m.chunks {
		if c.Page == n {
			return i
		}
	}
	return -1
}

// player controls ffplay via a kill-on-stop protocol, with signal-based
// pause/resume: SIGSTOP freezes the process mid-audio and SIGCONT picks
// it up exactly where it left off.
type player struct {
	cmd     *exec.Cmd
	done    chan struct{}
	stopped bool
	paused  bool
}

func (p *player) play(path string) {
	cmd := exec.Command("ffplay", "-nodisp", "-autoexit", "-loglevel", "quiet", path)
	if err := cmd.Start(); err != nil {
		close(p.done)
		return
	}
	p.cmd = cmd
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
}

func (p *player) stop() {
	p.stopped = true
	if p.cmd != nil && p.cmd.Process != nil {
		// SIGKILL works on a SIGSTOPped process too, so a paused player
		// can still be discarded.
		_ = p.cmd.Process.Kill()
	}
}

func (p *player) pause() {
	p.paused = true
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGSTOP)
	}
}

func (p *player) resume() {
	p.paused = false
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGCONT)
	}
}

type (
	tickMsg     struct{}
	playDoneMsg struct{ player *player }
	errMsgT     struct{ err string }
	fetchedMsg  struct {
		idx  int
		path string
		err  error
	}
	loadedMsg struct {
		chunks []chunkInfo
		title  string
	}
)

func initialModel(filePath string) model {
	// Fallback size in case the terminal never reports its dimensions
	// (e.g. when run under `script` or other minimal ptys).
	return model{
		filePath:   filePath,
		status:     statusIdle,
		width:      80,
		height:     24,
		prefetched: map[int]string{},
		pending:    map[int]bool{},
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(loadDoc(m.filePath), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

// loadDoc extracts text (from a local file or a web address) and warms
// the cache around the first chunk.
func loadDoc(path string) tea.Cmd {
	return func() tea.Msg {
		pages, title, err := extractDocument(path)
		if err != nil {
			return errMsgT{err: err.Error()}
		}
		chunks := buildChunks(pages, chunkMaxRunes)
		if len(chunks) == 0 {
			return errMsgT{err: "未找到可朗读的文本"}
		}
		return loadedMsg{chunks: chunks, title: title}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Some minimal ptys report a 0x0 window; keep the fallback instead.
		if msg.Width > 0 && msg.Height > 0 {
			m.width, m.height = msg.Width, msg.Height
		}

	case loadedMsg:
		m.chunks = msg.chunks
		m.docTitle = msg.title
		m.current = 0
		// Resume at the saved position if it still points inside the
		// document; a truncated or replaced file just falls through to
		// the top. Current == 0 is the top already, so no notice.
		if p, ok := m.savedProgress(); ok && p.Current > 0 && p.Current < len(m.chunks) {
			m.current = p.Current
			m.resumed = true
		}
		m.status = statusIdle
		// Stay silent: warm the cache around the cursor so the first
		// Enter starts instantly, but do not auto-play.
		return m, prefetchAround(m, m.current)

	case fetchedMsg:
		delete(m.pending, msg.idx)
		if msg.err != nil {
			// A failed prefetch is not fatal: it retries when reached.
			// In browsing mode errors stay quiet — they surface when the
			// user actually tries to play the chunk.
			if msg.idx == m.current && m.started {
				m.status = statusError
				m.errMsg = msg.err.Error()
			}
			return m, nil
		}
		// Auto-play on arrival only after the session has started; in
		// browsing mode the audio is merely stashed for later.
		if msg.idx == m.current && m.started {
			return m.playCurrent(msg.path)
		}
		m.prefetched[msg.idx] = msg.path
		return m, nil

	case playDoneMsg:
		// Only the current player's completion is meaningful; results
		// from stale players (replaced by a newer playback) are dropped.
		if m.player == nil || msg.player != m.player {
			return m, nil
		}
		m.player = nil
		// A player that was interrupted (rather than finishing on its
		// own) must not advance the cursor — whoever interrupted it has
		// already arranged the next step.
		if msg.player.stopped {
			return m, nil
		}
		if m.current+1 < len(m.chunks) {
			m.current++
			m.saveProgress()
			m.status = statusFetching
			return playOrFetch(m, m.current)
		}
		m.status = statusDone
		return m, nil

	case errMsgT:
		m.status = statusError
		m.errMsg = msg.err
		return m, nil

	case tickMsg:
		return m, tickCmd()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Any key acknowledges the resume notice.
	m.resumed = false

	// Page-input mode intercepts everything except Enter/Esc/Backspace.
	if m.pageInput != "" {
		switch msg.String() {
		case "enter":
			idx := m.indexOfPage(m.pageInput)
			m.pageInput = ""
			if idx < 0 {
				m.status = statusError
				m.errMsg = "没有这一页"
				return m, nil
			}
			return moveCursor(m, idx)
		case "esc", "ctrl+c":
			m.pageInput = ""
			return m, nil
		case "backspace":
			if len(m.pageInput) > 1 {
				m.pageInput = m.pageInput[:len(m.pageInput)-1]
			} else {
				m.pageInput = ""
			}
			return m, nil
		}
		if len(msg.Runes) == 1 && msg.Runes[0] >= '0' && msg.Runes[0] <= '9' && len(m.pageInput) < 5 {
			m.pageInput += string(msg.Runes)
		}
		return m, nil
	}

	// A leading digit starts page-input mode.
	if len(msg.Runes) == 1 && msg.Runes[0] >= '1' && msg.Runes[0] <= '9' {
		m.pageInput = string(msg.Runes)
		return m, nil
	}

	// 'gg' is typed as two g presses in a row.
	if msg.String() == "g" {
		m.gPressed = true
		return m, nil
	}
	gg := m.gPressed
	m.gPressed = false

	if gg && msg.String() == "g" { // gg -> first chunk
		return moveCursor(m, 0)
	}

	switch msg.String() {
	case "q", "ctrl+c", "esc":
		if m.player != nil {
			m.player.stop()
		}
		m.saveProgress()
		return m, tea.Quit

	case "enter":
		// First Enter starts the session from the cursor position; later
		// Enters replay the current chunk. playCurrent interrupts any
		// in-flight playback first, so audio never overlaps.
		if len(m.chunks) > 0 {
			m.started = true
			m.status = statusFetching
			return playOrFetch(m, m.current)
		}

	case " ", "p":
		// Pause/resume toggle. Note bubbletea reports the space key as
		// " " (not "space"). Requires an active player; in browsing mode
		// there is nothing to pause.
		if m.player != nil {
			if m.player.paused {
				m.player.resume()
				m.status = statusPlaying
			} else {
				m.player.pause()
				m.status = statusPaused
			}
		}

	case "n", "l", "right", "j", "down":
		if m.current < len(m.chunks)-1 {
			return moveCursor(m, m.current+1)
		}

	case "b", "h", "left", "k", "up":
		if m.current > 0 {
			return moveCursor(m, m.current-1)
		}

	case "G":
		return moveCursor(m, len(m.chunks)-1)
	}
	return m, nil
}

// ensureFetch synthesizes chunk idx if it isn't already cached or in
// flight. Idempotent: callers can invoke it freely on overlapping idx
// ranges without spawning duplicate requests.
func ensureFetch(m model, idx int) tea.Cmd {
	if idx < 0 || idx >= len(m.chunks) {
		return nil
	}
	if _, ok := m.prefetched[idx]; ok {
		return nil
	}
	if m.pending[idx] {
		return nil
	}
	m.pending[idx] = true
	tts := m.tts
	text := m.chunks[idx].Text
	return func() tea.Msg {
		path, err := tts.synthesize(text)
		return fetchedMsg{idx: idx, path: path, err: err}
	}
}

// playOrFetch starts playing chunk idx if its audio is already prefetched,
// otherwise synthesizes it; in both cases it warms up the next chunk so
// the lead-ahead window is maintained. It returns the updated model
// because playback assignment mutates m.player.
func playOrFetch(m model, idx int) (model, tea.Cmd) {
	var head tea.Cmd
	if path, ok := m.prefetched[idx]; ok {
		delete(m.prefetched, idx)
		m, head = m.playCurrent(path)
	} else {
		head = ensureFetch(m, idx)
	}
	// Make sure idx+1 is on its way regardless of whether idx was warm,
	// so playback never stalls at the boundary when idx finishes.
	tail := ensureFetch(m, idx+1)
	return m, tea.Batch(head, tail)
}

// playCurrent starts playback of path and warms the lead-ahead window
// so the next leadAhead chunks are fetched while this one plays.
// Any still-running player is interrupted first so old and new audio
// never overlap. Returns the updated model — callers must propagate it.
func (m model) playCurrent(path string) (model, tea.Cmd) {
	// Interrupt any in-flight playback before starting a new one.
	if m.player != nil {
		m.player.stop()
	}
	p := &player{done: make(chan struct{})}
	m.player = p
	p.play(path)
	m.status = statusPlaying

	cmds := []tea.Cmd{waitPlayDone(p)}
	for i := 1; i <= leadAhead; i++ {
		if c := ensureFetch(m, m.current+i); c != nil {
			cmds = append(cmds, c)
		}
	}
	return m, tea.Batch(cmds...)
}

func waitPlayDone(p *player) tea.Cmd {
	return func() tea.Msg {
		<-p.done
		return playDoneMsg{player: p}
	}
}

func (m model) View() string {
	if m.width == 0 {
		return "loading..."
	}
	statusLine := m.statusLine()
	hint := hintStyle.Render("  q 退出 · 回车 播放/重播 · 空格 暂停/继续 · j/k 或 ↑↓ 移动 · gg/G 首尾 · 数字+回车 跳页")
	if m.pageInput != "" {
		body := m.readingView()
		prompt := fetchingStyle.Render("  跳转到页码: " + m.pageInput + "_ (回车确认, Esc 取消)")
		return statusLine + "\n" + body + "\n" + prompt
	}
	if len(m.chunks) == 0 {
		return statusLine + "\n" + hint
	}
	return statusLine + "\n" + m.readingView() + "\n" + hint
}

func (m model) statusLine() string {
	title := titleStyle.Render(m.docName())
	if len(m.chunks) == 0 {
		if m.status == statusError {
			return title + "  " + errorStyle.Render("✗ "+m.errMsg)
		}
		return title + "  准备中..."
	}
	var st string
	switch m.status {
	case statusIdle:
		st = "⏸ 待播（回车开始）"
	case statusPlaying:
		st = "▶ 播放中"
	case statusFetching:
		st = fetchingStyle.Render("↺ 获取语音中")
	case statusPaused:
		st = fetchingStyle.Render("⏸ 已暂停（空格继续）")
	case statusError:
		st = errorStyle.Render("✗ " + m.errMsg)
	case statusDone:
		st = "✓ 朗读完毕"
	}
	page := pageStyle.Render(fmt.Sprintf("第 %d 页", m.chunks[m.current].Page))
	progress := pageStyle.Render(fmt.Sprintf("[%d/%d]", m.current+1, len(m.chunks)))
	line := fmt.Sprintf("%s  %s  %s  %s", title, st, page, progress)
	if m.resumed {
		line += "  " + fetchingStyle.Render("↩ 已恢复上次位置")
	}
	return line
}

// docName is what the status line calls the document: the web page
// title when known, otherwise the file name; truncated so long titles
// cannot push the status fields off screen.
func (m model) docName() string {
	name := m.docTitle
	if name == "" {
		name = filepath.Base(m.filePath)
	}
	return truncateWide(name, 48)
}

// truncateWide shortens s to max display columns, appending an
// ellipsis when it had to cut.
func truncateWide(s string, max int) string {
	if runewidth.StringWidth(s) <= max {
		return s
	}
	return runewidth.Truncate(s, max, "…")
}

// readingView renders the FULL PAGE the current chunk belongs to:
// every chunk on that page is shown (wrapped to terminal width), with
// the playing chunk highlighted, already-read chunks dimmed, and unread
// chunks in normal color. The view scrolls so the current chunk stays
// vertically centered, and the whole region is padded to fill the
// terminal height so the layout never jumps.
func (m model) readingView() string {
	width := m.width - 2 // 1-char gutter on each side
	if width < 1 {
		width = 1
	}
	height := m.height - 2 // status + hint
	if height < 1 {
		height = 1
	}

	curIdx := m.current
	curPage := m.chunks[curIdx].Page

	// Indices of every chunk on the current page. buildChunks appends
	// per page so they're contiguous, but filtering is robust anyway.
	var pageIdx []int
	for i, c := range m.chunks {
		if c.Page == curPage {
			pageIdx = append(pageIdx, i)
		}
	}

	// Wrap each page chunk into display lines and remember where each
	// chunk starts in the global line stream. A blank separator line
	// is inserted between chunks for visual breathing room.
	type block struct {
		lines []string
		style lipgloss.Style
	}
	blocks := make([]block, len(pageIdx))
	startLines := make([]int, len(pageIdx))
	curPos := 0
	total := 0
	for i, idx := range pageIdx {
		if idx == curIdx {
			curPos = i
		}
		var st lipgloss.Style
		switch {
		case idx == curIdx:
			st = currentStyle
		case idx < curIdx:
			st = doneStyle
		default:
			st = normalStyle
		}
		lines := wrapText(m.chunks[idx].Text, width)
		blocks[i] = block{lines: lines, style: st}
		startLines[i] = total
		total += len(lines)
		if i < len(pageIdx)-1 {
			total++ // separator blank line
		}
	}

	// Choose a viewing window that keeps the current chunk roughly in
	// the middle of the viewport when the page is taller than it.
	curStart := startLines[curPos]
	curHeight := len(blocks[curPos].lines)
	viewStart := curStart - (height-curHeight)/2
	if viewStart < 0 {
		viewStart = 0
	}
	viewEnd := viewStart + height
	if viewEnd > total {
		viewEnd = total
		viewStart = total - height
		if viewStart < 0 {
			viewStart = 0
		}
	}

	// Render only the lines that fall inside [viewStart, viewEnd).
	var out []string
	lineNo := 0
	for bi, b := range blocks {
		for _, ln := range b.lines {
			if lineNo >= viewStart && lineNo < viewEnd {
				out = append(out, b.style.Render("  "+ln))
			}
			lineNo++
		}
		if bi < len(blocks)-1 {
			if lineNo >= viewStart && lineNo < viewEnd {
				out = append(out, "")
			}
			lineNo++
		}
	}

	// Pad to full height so the bottom hint stays anchored.
	for len(out) < height {
		out = append(out, "")
	}
	if len(out) > height {
		out = out[:height]
	}
	return strings.Join(out, "\n")
}

// wrapText breaks text into lines no wider than width, accounting for
// East-Asian wide runes. Existing newlines are honored.
func wrapText(text string, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	var lines []string
	var cur strings.Builder
	curWidth := 0
	for _, r := range text {
		if r == '\n' {
			lines = append(lines, cur.String())
			cur.Reset()
			curWidth = 0
			continue
		}
		rw := runewidth.RuneWidth(r)
		if rw <= 0 {
			rw = 1
		}
		if curWidth+rw > width && cur.Len() > 0 {
			lines = append(lines, cur.String())
			cur.Reset()
			curWidth = 0
		}
		cur.WriteRune(r)
		curWidth += rw
	}
	lines = append(lines, cur.String())
	return lines
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: reader <pdf/txt/md/docx/odt/epub 文件路径 或 http(s) 网页地址>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}
	filePath := flag.Arg(0)
	if !isURL(filePath) {
		// A web address has nothing to stat or validate locally; the
		// fetch and extraction happen inside loadDoc.
		if _, err := os.Stat(filePath); err != nil {
			fmt.Fprintf(os.Stderr, "无法读取 %s: %v\n", filePath, err)
			os.Exit(1)
		}
		if ext := strings.ToLower(filepath.Ext(filePath)); !supportedExtensions[ext] {
			fmt.Fprintf(os.Stderr, "不支持的文件类型 %s（支持 pdf/txt/md/docx/odt/epub；.doc/.rtf 请先另存为 docx）\n", ext)
			os.Exit(1)
		}
	}

	// New endpoint = new voice signature; partition the cache so we
	// never play stale mp3s from the previous text-to-speech.cn backend.
	cacheDir := filepath.Join(os.TempDir(), "reader-tts-cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建缓存目录失败: %v\n", err)
		os.Exit(1)
	}

	// Progress is keyed by absolute path so re-opening the same document
	// from a different working directory still resumes to the same spot.
	// URLs stay as-is: filepath.Abs would mangle one into a bogus local
	// path.
	if !isURL(filePath) {
		if abs, err := filepath.Abs(filePath); err == nil {
			filePath = abs
		}
	}

	m := initialModel(filePath)
	m.tts = newTTSClient(cacheDir)
	m.progress = newProgressStore()

	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "运行出错: %v\n", err)
		os.Exit(1)
	}
}
