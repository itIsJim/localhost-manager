// tui.go — `localhost-manager tui`: full-screen interactive terminal UI.
//
// Built on Bubble Tea + Lipgloss. Mirrors the web UI: live port table with
// status badges, filter pills, fuzzy text filter, and a confirm-prompt kill
// flow (SIGTERM first, SIGKILL offered if the process survives).

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Palette mirrors public/index.html, adaptive to terminal light/dark themes.
var (
	cAccent = lipgloss.AdaptiveColor{Light: "#6D28D9", Dark: "#A78BFA"}
	cBorder = lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#3A404A"}
	cMuted  = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9AA1AB"}
	cGreen  = lipgloss.AdaptiveColor{Light: "#166534", Dark: "#6EE7A0"}
	cYellow = lipgloss.AdaptiveColor{Light: "#92400E", Dark: "#FBBF24"}
	cRed    = lipgloss.AdaptiveColor{Light: "#991B1B", Dark: "#F87171"}

	sTitle  = lipgloss.NewStyle().Bold(true)
	sLogo   = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sMuted  = lipgloss.NewStyle().Foreground(cMuted)
	sSel    = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sPanel  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cBorder).Padding(0, 1)
	sPrompt = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cRed).Padding(0, 1)
	sSearch = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cAccent).Padding(0, 1)
	sPillOn = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sGreen  = lipgloss.NewStyle().Foreground(cGreen)
	sYellow = lipgloss.NewStyle().Foreground(cYellow)
	sRed    = lipgloss.NewStyle().Foreground(cRed)
)

func statusStyle(status string) lipgloss.Style {
	switch status {
	case "active":
		return sGreen
	case "pending":
		return sYellow
	}
	return sRed
}

var filters = []string{"all", "active", "pending", "stale"}

type (
	scanMsg []PortInfo
	tickMsg time.Time
	killMsg struct {
		res   *killResult
		err   error
		force bool
	}
)

func scanCmd() tea.Cmd {
	return func() tea.Msg { return scanMsg(scanPorts()) }
}

func killCmd(pid int, force bool) tea.Cmd {
	return func() tea.Msg {
		res, err := killPID(pid, force)
		return killMsg{res, err, force}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

const (
	modeNormal = iota
	modeConfirm
	modeConfirmForce
)

type model struct {
	rows          []PortInfo
	filterIdx     int
	search        textinput.Model
	searching     bool
	sel           int
	selKey        string
	offset        int
	scanning      bool
	lastScan      time.Time
	width, height int
	mode          int
	target        PortInfo
	msg           string
	spin          spinner.Model
}

func newModel() model {
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot),
		spinner.WithStyle(lipgloss.NewStyle().Foreground(cAccent)))
	ti := textinput.New()
	ti.Prompt = "/ "
	ti.Placeholder = "filter by port, process, PID…"
	ti.PromptStyle = sPillOn
	return model{search: ti, scanning: true, spin: sp, width: 100, height: 30}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(scanCmd(), tickCmd(), m.spin.Tick)
}

func (m model) visible() []PortInfo {
	f := filters[m.filterIdx]
	q := strings.ToLower(strings.TrimSpace(m.search.Value()))
	var v []PortInfo
	for _, r := range m.rows {
		if f != "all" && r.Status != f {
			continue
		}
		if q != "" {
			hay := strings.ToLower(strings.Join([]string{
				strconv.Itoa(r.Port), strconv.Itoa(r.PID), r.Command, r.Args, r.User, r.HTTPInfo,
			}, " "))
			if !strings.Contains(hay, q) {
				continue
			}
		}
		v = append(v, r)
	}
	return v
}

func rowKey(r PortInfo) string { return strconv.Itoa(r.PID) + ":" + strconv.Itoa(r.Port) }

func (m *model) clampSel(v []PortInfo) {
	for i, r := range v {
		if rowKey(r) == m.selKey {
			m.sel = i
			return
		}
	}
	if m.sel >= len(v) {
		m.sel = len(v) - 1
	}
	if m.sel < 0 {
		m.sel = 0
	}
	if m.sel < len(v) {
		m.selKey = rowKey(v[m.sel])
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case scanMsg:
		m.rows, m.scanning, m.lastScan = msg, false, time.Now()
		m.clampSel(m.visible())
		return m, nil

	case tickMsg:
		cmds := []tea.Cmd{tickCmd()}
		if !m.scanning && m.mode == modeNormal {
			m.scanning = true
			cmds = append(cmds, scanCmd())
		}
		return m, tea.Batch(cmds...)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case killMsg:
		switch {
		case msg.err != nil:
			m.msg = "kill failed: " + msg.err.Error()
		case msg.res.Alive && !msg.force:
			m.mode = modeConfirmForce
		case msg.res.Alive:
			m.msg = fmt.Sprintf("%s (PID %d) is still alive after SIGKILL", m.target.Command, m.target.PID)
		default:
			m.msg = fmt.Sprintf("%s (PID %d) exited after %s", m.target.Command, m.target.PID, msg.res.Signal)
		}
		m.scanning = true
		return m, scanCmd()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Modal confirm prompt swallows everything.
	if m.mode == modeConfirm || m.mode == modeConfirmForce {
		switch msg.String() {
		case "y", "Y":
			force := m.mode == modeConfirmForce
			m.mode, m.msg = modeNormal, "sending signal…"
			return m, killCmd(m.target.PID, force)
		case "ctrl+c":
			return m, tea.Quit
		default:
			m.mode, m.msg = modeNormal, ""
			return m, nil
		}
	}

	// Search input captures typing until enter/esc.
	if m.searching {
		switch msg.String() {
		case "enter", "esc":
			m.searching = false
			if msg.String() == "esc" {
				m.search.SetValue("")
			}
			m.search.Blur()
			return m, nil
		case "ctrl+c":
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		m.sel, m.selKey = 0, ""
		return m, cmd
	}

	v := m.visible()
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "down", "j":
		if m.sel < len(v)-1 {
			m.sel++
		}
	case "up", "k":
		if m.sel > 0 {
			m.sel--
		}
	case "g", "home":
		m.sel = 0
	case "G", "end":
		m.sel = len(v) - 1
	case "tab", "f":
		m.filterIdx = (m.filterIdx + 1) % len(filters)
		m.sel, m.selKey, m.msg = 0, "", ""
	case "1", "2", "3", "4":
		m.filterIdx = int(msg.String()[0] - '1')
		m.sel, m.selKey, m.msg = 0, "", ""
	case "/":
		m.searching, m.msg = true, ""
		m.search.Focus()
		return m, textinput.Blink
	case "r":
		if !m.scanning {
			m.scanning = true
			return m, scanCmd()
		}
	case "x", "K", "enter":
		if m.sel >= len(v) {
			break
		}
		t := v[m.sel]
		switch {
		case t.Self:
			m.msg = "that's this app — press q to quit instead"
		case !t.Killable:
			m.msg = "not killable: owned by " + t.User
		default:
			m.target, m.mode, m.msg = t, modeConfirm, ""
		}
	}
	if m.sel >= 0 && m.sel < len(v) {
		m.selKey = rowKey(v[m.sel])
	}
	return m, nil
}

func (m model) View() string {
	if m.width < 40 {
		return "terminal too narrow"
	}
	v := m.visible()
	var sections []string

	// Title bar: logo + counts left, scan state right.
	counts := map[string]int{}
	for _, r := range m.rows {
		counts[r.Status]++
	}
	left := sLogo.Render("◍ localhost") + sTitle.Render(" manager") + "  " +
		sMuted.Render(fmt.Sprintf("%d ports · ", len(m.rows))) +
		sGreen.Render(fmt.Sprintf("%d active", counts["active"])) + sMuted.Render(" · ") +
		sYellow.Render(fmt.Sprintf("%d pending", counts["pending"])) + sMuted.Render(" · ") +
		sRed.Render(fmt.Sprintf("%d stale", counts["stale"]))
	right := sMuted.Render("refreshed " + m.lastScan.Format("15:04:05"))
	if m.scanning {
		right = m.spin.View() + sMuted.Render(" scanning")
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	sections = append(sections, left+strings.Repeat(" ", gap)+right)

	// Bordered search input, shown while typing or when a query is set.
	if m.searching || m.search.Value() != "" {
		sections = append(sections, sSearch.Width(m.width-2).Render(m.search.View()))
	}

	// Port table panel.
	innerW := m.width - 4 // panel border + padding
	chrome := 7           // title, panel borders+header, details, footer
	if m.searching || m.search.Value() != "" {
		chrome += 3
	}
	if m.mode != modeNormal {
		chrome += 3
	}
	bodyH := m.height - chrome
	if bodyH < 3 {
		bodyH = 3
	}
	if m.sel < m.offset {
		m.offset = m.sel
	}
	if m.sel >= m.offset+bodyH {
		m.offset = m.sel - bodyH + 1
	}
	var body []string
	body = append(body, sMuted.Render("  "+headerCells(innerW-2)))
	for i := m.offset; i < len(v) && i-m.offset < bodyH; i++ {
		r := v[i]
		cells := rowCells(r, innerW-2)
		status := r.Status
		if r.Self {
			status += " (this app)"
		}
		line := "  " + cells + statusStyle(r.Status).Render(status)
		if i == m.sel {
			line = sSel.Render("▸ ") + sTitle.Render(cells) + statusStyle(r.Status).Render(status)
		}
		body = append(body, line)
	}
	if len(v) == 0 {
		body = append(body, sMuted.Render("  no ports match"))
	}
	sections = append(sections, sPanel.Width(m.width-2).Render(strings.Join(body, "\n")))

	// Selected-row detail line.
	detail := ""
	if m.sel < len(v) {
		r := v[m.sel]
		detail = fmt.Sprintf(" %s · bound to %s · %s",
			strings.Join(r.Reasons, "; "), strings.Join(r.Addrs, ", "), r.Args)
	}
	sections = append(sections, sMuted.Render(truncateWidth(detail, m.width)))

	// Bordered confirm prompt, or the plain message line.
	switch m.mode {
	case modeConfirm:
		q := fmt.Sprintf("Kill %s (PID %d) on port %d? Sends SIGTERM.  ",
			m.target.Command, m.target.PID, m.target.Port)
		sections = append(sections, sPrompt.Width(m.width-2).Render(
			sTitle.Render(q)+sGreen.Render("[y]es")+"  "+sMuted.Render("[n]o")))
	case modeConfirmForce:
		q := fmt.Sprintf("%s (PID %d) survived SIGTERM — send SIGKILL?  ",
			m.target.Command, m.target.PID)
		sections = append(sections, sPrompt.Width(m.width-2).Render(
			sTitle.Render(q)+sRed.Render("[y]es")+"  "+sMuted.Render("[n]o")))
	default:
		if m.msg != "" {
			sections = append(sections, " "+m.msg)
		}
	}

	// Footer: filter pills + key hints.
	var pills []string
	for i, f := range filters {
		if i == m.filterIdx {
			pills = append(pills, sPillOn.Render("["+f+"]"))
		} else {
			pills = append(pills, sMuted.Render(" "+f+" "))
		}
	}
	sections = append(sections,
		" "+strings.Join(pills, " ")+"   "+
			sMuted.Render("↑/↓ move · x kill · / search · tab filter · r refresh · q quit"))

	return strings.Join(sections, "\n")
}

func truncateWidth(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r)) > w-1 {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func runTUI() {
	if !isTTY(os.Stdin) || !isTTY(os.Stdout) {
		fmt.Fprintln(os.Stderr, "tui needs a terminal (use `list` for plain output)")
		os.Exit(1)
	}
	p := tea.NewProgram(newModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tui error:", err)
		os.Exit(1)
	}
}
