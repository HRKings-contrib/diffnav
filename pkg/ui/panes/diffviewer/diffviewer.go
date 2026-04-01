package diffviewer

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"charm.land/log/v2"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/charmbracelet/x/ansi"

	"github.com/dlvhdr/diffnav/pkg/filenode"
	"github.com/dlvhdr/diffnav/pkg/icons"
	"github.com/dlvhdr/diffnav/pkg/ui/common"
	"github.com/dlvhdr/diffnav/pkg/utils"
)

const dirHeaderHeight = 3

// DisplayMode controls how diffs are rendered.
type DisplayMode int

const (
	DisplayInline             DisplayMode = iota // unified / inline
	DisplaySideBySide                            // side-by-side
	DisplaySideBySideShowBoth                    // side-by-side-show-both (difft only)
)

// DifftDisplayString returns the DFT_DISPLAY value for difftastic.
func (d DisplayMode) DifftDisplayString() string {
	switch d {
	case DisplaySideBySide:
		return "side-by-side"
	case DisplaySideBySideShowBoth:
		return "side-by-side-show-both"
	default:
		return "inline"
	}
}

func (d DisplayMode) IsSideBySide() bool {
	return d != DisplayInline
}

// ParseDisplayMode converts a string to a DisplayMode.
// Accepts: "inline", "side-by-side", "side-by-side-show-both".
func ParseDisplayMode(s string) DisplayMode {
	switch s {
	case "side-by-side":
		return DisplaySideBySide
	case "side-by-side-show-both":
		return DisplaySideBySideShowBoth
	default:
		return DisplayInline
	}
}

type cachedNode struct {
	path      string
	files     []*gitdiff.File
	additions int64
	deletions int64
	diff      string
}

type nodeCache map[string]*cachedNode

func cacheKey(path string, display DisplayMode) string {
	switch display {
	case DisplaySideBySide:
		return path + ":sbs"
	case DisplaySideBySideShowBoth:
		return path + ":sbsb"
	default:
		return path
	}
}

// difftChunk is a piece of streamed difft output.
type difftChunk struct {
	text string
	err  error
}

// difftStreamMsg carries a chunk of streamed difft output.
type difftStreamMsg struct {
	ch       <-chan difftChunk // channel for subsequent chunks
	cacheKey string
	chunk    string           // text for this chunk
	done     bool             // true when stream finished
	buf      *strings.Builder // accumulated output (owned by this stream)
}

type Model struct {
	common.Common
	vp             viewport.Model
	file           *cachedNode
	dir            *cachedNode
	cache          nodeCache
	display        DisplayMode
	preamble       string
	useDifft       bool
	gitCmd         string
	loading        bool
	loadingKey     string          // cache key of the current view's diff
	streaming      bool            // true while difft chunks are still arriving for current view
	inflight       map[string]bool // keys with active streams (prevents duplicate spawns)
	spinner        spinner.Model
	difftFileCache map[string]string // file path → difft output section (from root)

	// Incremental root stream parsing state
	rootParsed     int    // bytes of root buffer already scanned for headers
	rootLastPath   string // file path from the most recent header found
	rootLastOffset int    // byte offset where the current section starts
}

// SetPreamble stores the preamble text (e.g. commit metadata from git show).
func (m *Model) SetPreamble(preamble string) {
	m.preamble = preamble
}

// SetDifft configures the model to use difftastic instead of delta for rendering.
func (m *Model) SetDifft(gitCmd string) {
	m.useDifft = true
	m.gitCmd = gitCmd
}

func New(display DisplayMode) Model {
	s := spinner.New(
		spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("63"))),
	)
	return Model{
		vp:             viewport.Model{},
		display:        display,
		cache:          map[string]*cachedNode{},
		spinner:        s,
		inflight:       map[string]bool{},
		difftFileCache: map[string]string{},
	}
}

func (m Model) Init() tea.Cmd {
	return nil
}

func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	cmds := make([]tea.Cmd, 0)
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "down", "j", "n":
			break
		case "up", "k", "N", "p":
			break
		default:
			vp, vpCmd := m.vp.Update(msg)
			cmds = append(cmds, vpCmd)
			m.vp = vp
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)

	case diffContentMsg:
		delete(m.inflight, msg.cacheKey)
		m.loading = false
		diff := truncateLines(msg.text, m.vp.Width())
		if _, ok := m.cache[msg.cacheKey]; ok {
			m.cache[msg.cacheKey].diff = diff
		}
		m.vp.SetContent(diff)

	case difftStreamMsg:
		isCurrentView := m.loadingKey == msg.cacheKey
		isRootStream := m.useDifft && strings.HasPrefix(msg.cacheKey, "/")

		if msg.done {
			delete(m.inflight, msg.cacheKey)
			text := msg.buf.String()
			diff := truncateLines(text, m.vp.Width())
			// Always populate the node cache regardless of current view
			if node, ok := m.cache[msg.cacheKey]; ok {
				node.diff = diff
			}
			// Flush last file section from root stream
			if isRootStream {
				m.flushRootLastSection(text)
				// If a non-root view is waiting, render it from the fresh cache
				if !isCurrentView && m.loading {
					m.renderFromFileCache()
				}
			}
			if isCurrentView {
				m.loading = false
				m.streaming = false
				m.vp.SetContent(diff)
			}
			return m, nil
		}

		// Accumulate into the stream's own buffer
		msg.buf.WriteString(msg.chunk)

		if isCurrentView {
			m.loading = false   // show content instead of spinner
			m.streaming = true  // indicate more data is coming
			m.vp.SetContent(msg.buf.String())
		}

		// Incrementally parse root stream for per-file sections
		if isRootStream {
			if m.rootParsed == 0 {
				// Log first few lines to diagnose header format
				sample := msg.chunk
				if len(sample) > 500 {
					sample = sample[:500]
				}
				log.Debug("root stream first chunk (raw)", "sample", sample)
				log.Debug("root stream first chunk (stripped)", "sample", ansi.Strip(sample))
			}
			m.parseRootChunk(msg.buf.String())
			// Try to render current waiting view from newly cached files
			if !isCurrentView && m.loading {
				m.renderFromFileCache()
			}
		}
		// Always drain the channel to avoid goroutine leaks
		cmds = append(cmds, readNextChunk(msg.ch, msg.cacheKey, msg.buf))
	}

	return m, tea.Batch(cmds...)
}

const scrollbarWidth = 3 // 1 space + 1 scrollbar character + 1 padding

func (m Model) View() string {
	if m.loading {
		loadingText := m.spinner.View() + " Loading diff..."
		centered := lipgloss.Place(
			m.Width, m.Height,
			lipgloss.Center, lipgloss.Center,
			loadingText,
		)
		return centered
	}
	vpView := m.vp.View()
	scrollbar := common.RenderScrollbar(m.vp.Height(), m.vp.TotalLineCount(), m.vp.YOffset())
	if scrollbar != "" {
		vpView = lipgloss.JoinHorizontal(lipgloss.Top, vpView, " ", scrollbar)
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.headerView(), vpView)
}

func (m *Model) SetSize(width, height int) tea.Cmd {
	m.Width = width
	m.Height = height
	m.vp.SetWidth(m.contentWidth())
	m.vp.SetHeight(m.Height - dirHeaderHeight)
	m.ClearCache()
	// Skip re-rendering if a diff is already in flight — the next
	// SetFilePatch/SetDirPatch call will use the updated dimensions.
	if m.loading {
		return nil
	}
	return m.diff()
}

func (m Model) contentWidth() int {
	return m.Width - scrollbarWidth
}

func (m *Model) withLoading(key string, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	m.loading = true
	m.loadingKey = key
	m.inflight[key] = true
	return tea.Batch(cmd, m.spinner.Tick)
}

func (m *Model) diff() tea.Cmd {
	if m.file != nil {
		key := cacheKey(m.file.path, m.display)
		if cached, ok := m.cache[key]; ok && cached.diff != "" {
			m.file = cached
			m.vp.SetContent(cached.diff)
			return nil
		}
		// Already loading this exact key — don't spawn a duplicate.
		if m.loading && m.loadingKey == key {
			return nil
		}
		node := &cachedNode{
			path:      m.file.path,
			files:     m.file.files,
			additions: m.file.additions,
			deletions: m.file.deletions,
		}
		m.file = node
		m.cache[key] = node
		if m.useDifft {
			return m.withLoading(key, diffFileDifft(node, m.contentWidth(), m.display, m.gitCmd))
		}
		return m.withLoading(key, diffFile(node, m.contentWidth(), m.display))
	} else if m.dir != nil {
		key := cacheKey(m.dir.path, m.display)
		if cached, ok := m.cache[key]; ok && cached.diff != "" {
			m.dir = cached
			m.vp.SetContent(cached.diff)
			return nil
		}
		// Already loading this exact key — don't spawn a duplicate.
		if m.loading && m.loadingKey == key {
			return nil
		}
		node := &cachedNode{
			path:      m.dir.path,
			files:     m.dir.files,
			additions: m.dir.additions,
			deletions: m.dir.deletions,
		}
		m.dir = node
		m.cache[key] = node
		preamble := ""
		if m.dir.path == "/" {
			preamble = m.preamble
		}
		if m.useDifft {
			return m.withLoading(key, diffDirDifft(node, m.contentWidth(), m.display, m.gitCmd, preamble))
		}
		return m.withLoading(key, diffDir(node, m.contentWidth(), m.display, preamble))
	}

	return nil
}

// IsStreaming returns true if any difft stream is still in progress.
func (m Model) IsStreaming() bool {
	return len(m.inflight) > 0
}

// SpinnerView returns the current spinner frame for external use.
func (m Model) SpinnerView() string {
	return m.spinner.View()
}

func (m Model) headerView() string {
	if m.dir != nil {
		return m.dirHeaderView()
	}

	if m.file == nil || len(m.file.files) != 1 {
		return ""
	}
	name := m.file.path
	base := lipgloss.NewStyle()

	fileIcon := icons.GetIcon(name, false)
	prefix := base.Render(fileIcon) + base.Render(" ")
	name = utils.TruncateString(name, m.Width-lipgloss.Width(prefix))
	top := prefix + base.Bold(true).Render(name)

	bottom := filenode.ViewFileDiffStats(m.file.files[0], base)
	return base.
		Width(m.Width).
		Height(dirHeaderHeight - 1).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(lipgloss.Color("8")).
		Render(lipgloss.JoinVertical(lipgloss.Left, top, bottom))
}

func (m Model) dirHeaderView() string {
	base := lipgloss.NewStyle().Foreground(lipgloss.Blue)
	prefix := base.Render(" ")
	name := utils.TruncateString(m.dir.path, m.Width-lipgloss.Width(prefix))

	top := prefix + base.Bold(true).Render(name)
	bottom := filenode.ViewDiffStats(m.dir.additions, m.dir.deletions, base)
	return base.
		Width(m.Width).
		Height(dirHeaderHeight - 1).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(lipgloss.Color("8")).
		Render(lipgloss.JoinVertical(lipgloss.Left, top, bottom))
}

func (m Model) SetFilePatch(file *gitdiff.File) (Model, tea.Cmd) {
	m.dir = nil

	fname := filenode.GetFileName(file)
	key := cacheKey(fname, m.display)
	if cached, ok := m.cache[key]; ok && cached.diff != "" {
		m.file = cached
		m.streaming = false
		m.vp.SetContent(cached.diff)
		return m, nil
	}
	// Already in-flight — just point current view at it and wait
	if m.inflight[key] {
		m.loadingKey = key
		m.loading = true
		m.streaming = false
		return m, m.spinner.Tick
	}

	files := make([]*gitdiff.File, 1)
	files[0] = file
	additions, deletions := filenode.DiffStats(file)
	m.file = &cachedNode{
		path:      fname,
		files:     files,
		additions: additions,
		deletions: deletions,
	}
	m.cache[key] = m.file

	if m.useDifft {
		rootKey := cacheKey("/", m.display)
		// Check per-file cache from root difft output
		if section, ok := m.difftFileCache[fname]; ok {
			diff := truncateLines(section, m.vp.Width())
			m.file.diff = diff
			m.streaming = false
			m.vp.SetContent(diff)
			log.Debug("SetFilePatch from cache", "key", key)
			return m, nil
		}
		// Root completed but file not in cache — difft produced no output for it
		if !m.inflight[rootKey] && len(m.difftFileCache) > 0 {
			m.file.diff = ""
			m.streaming = false
			m.vp.SetContent("No structural changes")
			log.Debug("SetFilePatch no difft output", "key", key)
			return m, nil
		}
		// Root is still streaming — wait for it instead of spawning a redundant process
		if m.inflight[rootKey] {
			m.loading = true
			m.loadingKey = key
			log.Debug("SetFilePatch waiting for root", "key", key)
			return m, m.spinner.Tick
		}
		log.Debug("SetFilePatch cache miss", "key", key, "cache_size", len(m.difftFileCache))
	}

	m.loading = true
	m.loadingKey = key
	m.inflight[key] = true
	log.Debug("SetFilePatch spawning", "key", key)
	if m.useDifft {
		return m, tea.Batch(diffFileDifft(m.file, m.contentWidth(), m.display, m.gitCmd), m.spinner.Tick)
	}
	return m, tea.Batch(diffFile(m.file, m.contentWidth(), m.display), m.spinner.Tick)
}

func (m Model) SetDirPatch(dirPath string, files []*gitdiff.File) (Model, tea.Cmd) {
	m.file = nil

	key := cacheKey(dirPath, m.display)
	if cached, ok := m.cache[key]; ok && cached.diff != "" {
		m.dir = cached
		m.streaming = false
		m.vp.SetContent(cached.diff)
		return m, nil
	}
	// Already in-flight — just point current view at it and wait
	if m.inflight[key] {
		m.loadingKey = key
		m.loading = true
		m.streaming = false
		return m, m.spinner.Tick
	}

	var added, deleted int64
	for _, file := range files {
		na, nd := filenode.DiffStats(file)
		added += na
		deleted += nd
	}
	m.dir = &cachedNode{
		path:      dirPath,
		files:     files,
		additions: added,
		deletions: deleted,
	}
	m.cache[key] = m.dir

	// Check per-file cache from root difft output (non-root dirs only)
	if m.useDifft && dirPath != "/" {
		rootKey := cacheKey("/", m.display)
		rootDone := !m.inflight[rootKey] && len(m.difftFileCache) > 0
		var buf strings.Builder
		allCached := true
		for _, file := range files {
			fname := filenode.GetFileName(file)
			if section, ok := m.difftFileCache[fname]; ok {
				buf.WriteString(section)
			} else if rootDone {
				// Root completed but file not in cache — difft produced no output for it
				continue
			} else {
				allCached = false
				break
			}
		}
		if allCached {
			diff := truncateLines(buf.String(), m.vp.Width())
			m.dir.diff = diff
			m.streaming = false
			m.vp.SetContent(diff)
			log.Debug("SetDirPatch from cache", "key", key)
			return m, nil
		}
		// Root is still streaming — wait for it instead of spawning a redundant process
		if m.inflight[rootKey] {
			m.loading = true
			m.loadingKey = key
			return m, m.spinner.Tick
		}
	}

	preamble := ""
	if dirPath == "/" {
		preamble = m.preamble
	}
	m.loading = true
	m.loadingKey = key
	m.inflight[key] = true
	log.Debug("SetDirPatch spawning", "key", key)
	if m.useDifft {
		return m, tea.Batch(diffDirDifft(m.dir, m.contentWidth(), m.display, m.gitCmd, preamble), m.spinner.Tick)
	}
	return m, tea.Batch(diffDir(m.dir, m.contentWidth(), m.display, preamble), m.spinner.Tick)
}

func (m *Model) GoToTop() {
	m.vp.GotoTop()
}

// SetDisplayMode updates the diff view mode and re-renders.
func (m *Model) SetDisplayMode(display DisplayMode) tea.Cmd {
	m.display = display
	return m.diff()
}

// renderFromFileCache tries to render the current waiting view from difftFileCache.
// Called when root stream completes and a non-root view is loading.
func (m *Model) renderFromFileCache() {
	if m.file != nil {
		if section, ok := m.difftFileCache[m.file.path]; ok {
			diff := truncateLines(section, m.vp.Width())
			m.file.diff = diff
			key := cacheKey(m.file.path, m.display)
			if node, ok := m.cache[key]; ok {
				node.diff = diff
			}
			m.loading = false
			m.streaming = false
			m.vp.SetContent(diff)
		}
	} else if m.dir != nil && m.dir.path != "/" {
		var buf strings.Builder
		allCached := true
		for _, f := range m.dir.files {
			fname := filenode.GetFileName(f)
			if section, ok := m.difftFileCache[fname]; ok {
				buf.WriteString(section)
			} else {
				allCached = false
				break
			}
		}
		if allCached {
			diff := truncateLines(buf.String(), m.vp.Width())
			m.dir.diff = diff
			key := cacheKey(m.dir.path, m.display)
			if node, ok := m.cache[key]; ok {
				node.diff = diff
			}
			m.loading = false
			m.streaming = false
			m.vp.SetContent(diff)
		}
	}
}

// parseRootChunk incrementally scans the root stream buffer for new file headers.
// Each time a new header is found, the previous section is complete and cached.
// This is O(chunk_size) per call, not O(total_buffer).
func (m *Model) parseRootChunk(fullText string) {
	text := fullText[m.rootParsed:]
	offset := m.rootParsed

	for len(text) > 0 {
		nlIdx := strings.IndexByte(text, '\n')
		if nlIdx < 0 {
			break // incomplete line — wait for next chunk
		}
		line := text[:nlIdx]
		text = text[nlIdx+1:]

		stripped := ansi.Strip(line)
		if match := difftHeaderRe.FindStringSubmatch(stripped); match != nil {
			path := strings.TrimSpace(match[1])
			if idx := strings.Index(path, " → "); idx >= 0 {
				path = path[idx+len(" → "):]
			}
			// Multi-hunk files (--- 1/2, --- 2/2) share the same path.
			// Only flush when we see a DIFFERENT file.
			if path != m.rootLastPath {
				if m.rootLastPath != "" {
					m.difftFileCache[m.rootLastPath] = fullText[m.rootLastOffset:offset]
					log.Debug("root cache file", "path", m.rootLastPath, "total_cached", len(m.difftFileCache))
				}
				m.rootLastPath = path
				m.rootLastOffset = offset
			}
		}
		offset += nlIdx + 1
	}
	m.rootParsed = offset
}

// flushRootLastSection caches the final file section from a completed root stream.
func (m *Model) flushRootLastSection(fullText string) {
	if m.rootLastPath != "" && m.rootLastOffset < len(fullText) {
		m.difftFileCache[m.rootLastPath] = fullText[m.rootLastOffset:]
		log.Debug("root cache flush last", "path", m.rootLastPath, "total_cached", len(m.difftFileCache))
	} else {
		log.Debug("root cache flush EMPTY", "lastPath", m.rootLastPath, "lastOffset", m.rootLastOffset, "textLen", len(fullText), "total_cached", len(m.difftFileCache))
	}
	// Reset incremental state
	m.rootParsed = 0
	m.rootLastPath = ""
	m.rootLastOffset = 0
}

// CycleDisplay cycles through available display modes and re-renders.
// In difft mode: inline → side-by-side → side-by-side-show-both → inline.
// In delta mode: inline → side-by-side → inline.
func (m *Model) CycleDisplay() tea.Cmd {
	if m.useDifft {
		m.display = (m.display + 1) % 3
	} else {
		m.display = (m.display + 1) % 2
	}
	return m.diff()
}

// DisplayMode returns the current display mode.
func (m *Model) DisplayMode() DisplayMode {
	return m.display
}

// ScrollUp scrolls the viewport up by the given number of lines.
func (m *Model) ScrollUp(lines int) {
	m.vp.ScrollUp(lines)
}

// ScrollDown scrolls the viewport down by the given number of lines.
func (m *Model) ScrollDown(lines int) {
	m.vp.ScrollDown(lines)
}

func diffFile(node *cachedNode, width int, display DisplayMode) tea.Cmd {
	if width == 0 || node == nil || len(node.files) != 1 {
		return nil
	}

	file := node.files[0]
	key := cacheKey(node.path, display)
	return func() tea.Msg {
		// Only use side-by-side if preference is set AND file is not new/deleted
		useSideBySide := display.IsSideBySide() && !file.IsNew && !file.IsDelete
		args := []string{
			"--paging=never",
			fmt.Sprintf("-w=%d", width),
			fmt.Sprintf("--max-line-length=%d", width),
		}
		if useSideBySide {
			args = append(args, "--side-by-side")
		}
		deltac := exec.Command("delta", args...)
		deltac.Env = os.Environ()
		deltac.Stdin = strings.NewReader(file.String() + "\n")
		out, err := deltac.Output()
		if err != nil {
			return common.ErrMsg{Err: err}
		}
		return diffContentMsg{cacheKey: key, text: string(out)}
	}
}

func diffDir(dir *cachedNode, width int, display DisplayMode, preamble string) tea.Cmd {
	if width == 0 || dir == nil {
		return nil
	}
	key := cacheKey(dir.path, display)
	return func() tea.Msg {
		s := common.BgStyles[common.Selected]
		c := common.LipglossColorToHex(common.Colors[common.Selected])
		useSideBySide := display.IsSideBySide()
		args := []string{
			"--paging=never",
			fmt.Sprintf("--file-modified-label=%s",
				utils.RemoveReset(s.Foreground(lipgloss.Yellow).Render(" "))),
			fmt.Sprintf("--file-removed-label=%s",
				utils.RemoveReset(s.Foreground(lipgloss.Red).Render(" "))),
			fmt.Sprintf("--file-added-label=%s",
				utils.RemoveReset(s.Foreground(lipgloss.Green).Render(" "))),
			fmt.Sprintf("--file-style='%s bold %s'", c, c),
			fmt.Sprintf("--file-decoration-style='%s box %s'", c, c),
			fmt.Sprintf("-w=%d", width),
			fmt.Sprintf("--max-line-length=%d", width),
		}
		if useSideBySide {
			args = append(args, "--side-by-side")
		}
		deltac := exec.Command("delta", args...)
		deltac.Env = os.Environ()
		strs := strings.Builder{}
		for _, file := range dir.files {
			strs.WriteString(file.String())
		}
		deltac.Stdin = strings.NewReader(strs.String() + "\n")
		out, err := deltac.Output()
		if err != nil {
			return common.ErrMsg{Err: err}
		}

		text := string(out)
		if preamble != "" {
			text = renderPreamble(preamble) + "\n" + text
		}
		return diffContentMsg{cacheKey: key, text: text}
	}
}

func filePathArgs(file *gitdiff.File) []string {
	if file.OldName != "" && file.NewName != "" && file.OldName != file.NewName {
		return []string{file.OldName, file.NewName}
	}
	name := file.NewName
	if name == "" {
		name = file.OldName
	}
	return []string{name}
}

func buildDifftCmd(gitCmd string, width int, dftDisplay string, pathArgs []string) *exec.Cmd {
	args := strings.Fields(gitCmd)

	// Insert -c diff.external=difft right after "git" (before subcommand args).
	// This is faster than using GIT_EXTERNAL_DIFF env var.
	if len(args) > 0 && args[0] == "git" {
		rest := make([]string, len(args)-1)
		copy(rest, args[1:])
		args = append([]string{"git", "-c", "diff.external=difft"}, rest...)
	}

	if len(pathArgs) > 0 {
		args = append(args, "--")
		args = append(args, pathArgs...)
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("DFT_WIDTH=%d", width),
		fmt.Sprintf("DFT_DISPLAY=%s", dftDisplay),
		"DFT_COLOR=always",
	)
	return cmd
}

func diffFileDifft(node *cachedNode, width int, display DisplayMode, gitCmd string) tea.Cmd {
	if width == 0 || node == nil || len(node.files) != 1 {
		return nil
	}

	file := node.files[0]
	key := cacheKey(node.path, display)
	dftDisplay := display.DifftDisplayString()
	if file.IsNew || file.IsDelete {
		dftDisplay = "inline"
	}
	return streamDifftCmd(gitCmd, width, dftDisplay, filePathArgs(file), key, "")
}

func diffDirDifft(dir *cachedNode, width int, display DisplayMode, gitCmd string, preamble string) tea.Cmd {
	if width == 0 || dir == nil {
		return nil
	}
	key := cacheKey(dir.path, display)
	dftDisplay := display.DifftDisplayString()

	var paths []string
	if dir.path != "/" {
		paths = []string{dir.path + "/"}
	}

	return streamDifftCmd(gitCmd, width, dftDisplay, paths, key, preamble)
}

// streamDifftCmd runs a difft command and streams its output in chunks.
func streamDifftCmd(gitCmd string, width int, dftDisplay string, pathArgs []string, cacheKey string, preamble string) tea.Cmd {
	return func() tea.Msg {
		cmd := buildDifftCmd(gitCmd, width, dftDisplay, pathArgs)
		log.Debug("difft stream cmd", "args", cmd.Args)
		start := time.Now()

		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return common.ErrMsg{Err: err}
		}
		if err := cmd.Start(); err != nil {
			return common.ErrMsg{Err: err}
		}

		ch := make(chan difftChunk, 10)
		go func() {
			defer close(ch)
			scanner := bufio.NewScanner(pipe)
			scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
			var batch strings.Builder
			lines := 0
			for scanner.Scan() {
				batch.WriteString(scanner.Text())
				batch.WriteByte('\n')
				lines++
				if lines >= 50 {
					ch <- difftChunk{text: batch.String()}
					batch.Reset()
					lines = 0
				}
			}
			if batch.Len() > 0 {
				ch <- difftChunk{text: batch.String()}
			}
			if err := cmd.Wait(); err != nil {
				log.Debug("difft stream exit error (may be normal)", "err", err)
			}
			log.Debug("difft stream done", "elapsed", time.Since(start))
		}()

		buf := &strings.Builder{}

		// Prepend preamble to first chunk if present
		prefix := ""
		if preamble != "" {
			prefix = renderPreamble(preamble) + "\n"
		}

		// Read first chunk
		chunk, ok := <-ch
		if !ok {
			return difftStreamMsg{cacheKey: cacheKey, done: true, buf: buf}
		}
		if chunk.err != nil {
			return common.ErrMsg{Err: chunk.err}
		}
		return difftStreamMsg{ch: ch, cacheKey: cacheKey, chunk: prefix + chunk.text, buf: buf}
	}
}

// readNextChunk returns a tea.Cmd that reads the next chunk from a difft stream channel.
func readNextChunk(ch <-chan difftChunk, key string, buf *strings.Builder) tea.Cmd {
	return func() tea.Msg {
		chunk, ok := <-ch
		if !ok {
			return difftStreamMsg{cacheKey: key, done: true, buf: buf}
		}
		if chunk.err != nil {
			return common.ErrMsg{Err: chunk.err}
		}
		return difftStreamMsg{ch: ch, cacheKey: key, chunk: chunk.text, buf: buf}
	}
}

// truncateLines truncates each line to the given width to prevent ANSI escape overflow.
func truncateLines(text string, width int) string {
	if width <= 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if lipgloss.Width(line) > width {
			lines[i] = ansi.Truncate(line, width, "")
		}
	}
	return strings.Join(lines, "\n")
}

// difftHeaderRe matches difftastic file headers like "path/to/file.ext --- Language"
// or multi-hunk headers like "path/to/file.ext --- 1/2 --- Language".
// The header may contain ANSI color codes, so we strip them before extracting the path.
var difftHeaderRe = regexp.MustCompile(`(?m)^(.+?)\s+---\s+(?:\d+/\d+\s+---\s+)?\S+\s*$`)

func renderPreamble(preamble string) string {
	preamble = strings.TrimSpace(preamble)
	if preamble == "" {
		return ""
	}

	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	yellow := lipgloss.NewStyle().Foreground(lipgloss.Yellow)

	var out []string
	for _, line := range strings.Split(preamble, "\n") {
		switch {
		case strings.HasPrefix(line, "commit "):
			out = append(
				out,
				dim.Render("commit ")+yellow.Render(strings.TrimPrefix(line, "commit ")),
			)
		case strings.HasPrefix(line, "Author:"),
			strings.HasPrefix(line, "AuthorDate:"),
			strings.HasPrefix(line, "Date:"),
			strings.HasPrefix(line, "Commit:"),
			strings.HasPrefix(line, "CommitDate:"),
			strings.HasPrefix(line, "Merge:"):
			out = append(out, dim.Render(line))
		default:
			out = append(out, line)
		}
	}

	return strings.Join(out, "\n")
}

type diffContentMsg struct {
	cacheKey string
	text     string
}

func (m *Model) ClearCache() {
	m.cache = make(nodeCache)
	m.inflight = make(map[string]bool)
	m.difftFileCache = make(map[string]string)
	m.rootParsed = 0
	m.rootLastPath = ""
	m.rootLastOffset = 0
}

func (m *Model) RootDiffStats() (int64, int64) {
	if item, ok := m.cache[cacheKey("/", m.display)]; ok {
		return item.additions, item.deletions
	}

	return 0, 0
}
