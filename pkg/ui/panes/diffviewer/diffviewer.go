package diffviewer

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

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

type Model struct {
	common.Common
	vp       viewport.Model
	file     *cachedNode
	dir      *cachedNode
	cache    nodeCache
	display  DisplayMode
	preamble string
	useDifft bool
	gitCmd   string
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

func New(sideBySide bool) Model {
	display := DisplayInline
	if sideBySide {
		display = DisplaySideBySide
	}
	return Model{
		vp:      viewport.Model{},
		display: display,
		cache:   map[string]*cachedNode{},
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

	case diffContentMsg:
		// Truncate lines to viewport width to prevent ANSI escape overflow.
		lines := strings.Split(msg.text, "\n")
		for i, line := range lines {
			if lipgloss.Width(line) > m.vp.Width() && m.vp.Width() > 0 {
				lines[i] = ansi.Truncate(line, m.vp.Width(), "")
			}
		}
		diff := strings.Join(lines, "\n")
		if _, ok := m.cache[msg.cacheKey]; ok {
			m.cache[msg.cacheKey].diff = diff
		}
		m.vp.SetContent(diff)
	}

	return m, tea.Batch(cmds...)
}

const scrollbarWidth = 3 // 1 space + 1 scrollbar character + 1 padding

func (m Model) View() string {
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
	return m.diff()
}

func (m Model) contentWidth() int {
	return m.Width - scrollbarWidth
}

func (m *Model) diff() tea.Cmd {
	if m.file != nil {
		key := cacheKey(m.file.path, m.display)
		if cached, ok := m.cache[key]; ok && cached.diff != "" {
			m.file = cached
			m.vp.SetContent(cached.diff)
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
			return diffFileDifft(node, m.contentWidth(), m.display, m.gitCmd)
		}
		return diffFile(node, m.contentWidth(), m.display)
	} else if m.dir != nil {
		key := cacheKey(m.dir.path, m.display)
		if cached, ok := m.cache[key]; ok && cached.diff != "" {
			m.dir = cached
			m.vp.SetContent(cached.diff)
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
			return diffDirDifft(node, m.contentWidth(), m.display, m.gitCmd, preamble)
		}
		return diffDir(node, m.contentWidth(), m.display, preamble)
	}

	return nil
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
	if cached, ok := m.cache[key]; ok {
		m.file = cached
		m.vp.SetContent(cached.diff)
		return m, nil
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
		return m, diffFileDifft(m.file, m.contentWidth(), m.display, m.gitCmd)
	}
	return m, diffFile(m.file, m.contentWidth(), m.display)
}

func (m Model) SetDirPatch(dirPath string, files []*gitdiff.File) (Model, tea.Cmd) {
	m.file = nil

	key := cacheKey(dirPath, m.display)
	if cached, ok := m.cache[key]; ok {
		m.dir = cached
		m.vp.SetContent(cached.diff)
		return m, nil
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
	preamble := ""
	if dirPath == "/" {
		preamble = m.preamble
	}
	if m.useDifft {
		return m, diffDirDifft(m.dir, m.contentWidth(), m.display, m.gitCmd, preamble)
	}
	return m, diffDir(m.dir, m.contentWidth(), m.display, preamble)
}

func (m *Model) GoToTop() {
	m.vp.GotoTop()
}

// SetDisplayMode updates the diff view mode and re-renders.
func (m *Model) SetDisplayMode(display DisplayMode) tea.Cmd {
	m.display = display
	return m.diff()
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

func diffFileDifft(node *cachedNode, width int, display DisplayMode, gitCmd string) tea.Cmd {
	if width == 0 || node == nil || len(node.files) != 1 {
		return nil
	}

	file := node.files[0]
	key := cacheKey(node.path, display)
	return func() tea.Msg {
		dftDisplay := display.DifftDisplayString()
		if file.IsNew || file.IsDelete {
			dftDisplay = "inline"
		}

		pathArgs := ""
		if file.OldName != "" && file.NewName != "" && file.OldName != file.NewName {
			pathArgs = fmt.Sprintf(" -- %q %q", file.OldName, file.NewName)
		} else {
			pathArgs = fmt.Sprintf(" -- %q", node.path)
		}

		cmd := exec.Command("sh", "-c", gitCmd+pathArgs)
		cmd.Env = append(os.Environ(),
			"GIT_EXTERNAL_DIFF=difft",
			fmt.Sprintf("DFT_WIDTH=%d", width),
			fmt.Sprintf("DFT_DISPLAY=%s", dftDisplay),
			"DFT_COLOR=always",
		)
		out, err := cmd.Output()
		if err != nil {
			return common.ErrMsg{Err: err}
		}
		return diffContentMsg{cacheKey: key, text: string(out)}
	}
}

func diffDirDifft(dir *cachedNode, width int, display DisplayMode, gitCmd string, preamble string) tea.Cmd {
	if width == 0 || dir == nil {
		return nil
	}
	key := cacheKey(dir.path, display)
	return func() tea.Msg {
		dftDisplay := display.DifftDisplayString()

		env := append(os.Environ(),
			"GIT_EXTERNAL_DIFF=difft",
			fmt.Sprintf("DFT_WIDTH=%d", width),
			fmt.Sprintf("DFT_DISPLAY=%s", dftDisplay),
			"DFT_COLOR=always",
		)

		var text string
		if dir.path == "/" {
			// Root: run the full git command
			cmd := exec.Command("sh", "-c", gitCmd)
			cmd.Env = env
			out, err := cmd.Output()
			if err != nil {
				return common.ErrMsg{Err: err}
			}
			text = string(out)
		} else {
			// Subdirectory: run per-file and concatenate
			var sb strings.Builder
			for _, file := range dir.files {
				filePath := file.NewName
				if filePath == "" {
					filePath = file.OldName
				}
				pathArgs := ""
				if file.OldName != "" && file.NewName != "" && file.OldName != file.NewName {
					pathArgs = fmt.Sprintf(" -- %q %q", file.OldName, file.NewName)
				} else {
					pathArgs = fmt.Sprintf(" -- %q", filePath)
				}
				cmd := exec.Command("sh", "-c", gitCmd+pathArgs)
				cmd.Env = env
				out, err := cmd.Output()
				if err != nil {
					continue
				}
				sb.Write(out)
			}
			text = sb.String()
		}

		if preamble != "" {
			text = renderPreamble(preamble) + "\n" + text
		}
		return diffContentMsg{cacheKey: key, text: text}
	}
}

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
}

func (m *Model) RootDiffStats() (int64, int64) {
	if item, ok := m.cache[cacheKey("/", m.display)]; ok {
		return item.additions, item.deletions
	}

	return 0, 0
}
