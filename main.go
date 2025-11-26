// gh-pr-scheduler: schedule auto-merge for GitHub PRs using gh CLI + Bubble Tea.
//
// Requirements:
//   - gh (GitHub CLI) installed and authenticated
//   - notify-send available (Linux desktop notifications)
//   - Go modules enabled
//
// Usage:
//
//	go run main.go
//
// Keys:
//   - Up/Down or j/k: move selection
//   - Enter: schedule auto-merge for selected PR
//   - m: toggle "only my PRs"
//   - r: refresh PR list
//   - q: quit
//
// When scheduling, enter time as:
//   - "now" (or leave empty) => immediate
//   - "YYYY-MM-DD HH:MM" (24h, local time)
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---------- Data types ----------

type pr struct {
	Number     int
	Title      string
	Author     string
	State      string
	MergeState string
	URL        string
}

type prItem struct {
	p pr
}

func (i prItem) Title() string { return fmt.Sprintf("#%d %s", i.p.Number, i.p.Title) }
func (i prItem) Description() string {
	return fmt.Sprintf("%s | %s | @%s", i.p.State, i.p.MergeState, i.p.Author)
}
func (i prItem) FilterValue() string { return i.p.Title }

// For scheduling auto-merge of a PR.
type scheduledMerge struct {
	PR             pr
	When           time.Time
	MergeTriggered bool
	CheckScheduled bool
	CheckAt        time.Time
	Done           bool
	LastMessage    string
}

// ---------- Messages ----------

type (
	prListMsg      []pr
	meMsg          string
	errMsg         struct{ err error }
	tickMsg        time.Time
	mergeResultMsg struct {
		prNumber int
		err      error
	}
	checkMergedMsg struct {
		prNumber int
		merged   bool
		err      error
	}
)

// ---------- Commands (side effects) ----------

func fetchMeCmd() tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("gh", "api", "user", "--jq", ".login")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return errMsg{fmt.Errorf("failed to get current GitHub user: %w (%s)", err, string(out))}
		}
		login := strings.TrimSpace(string(out))
		return meMsg(login)
	}
}

func fetchPRsCmd() tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("gh", "pr", "list",
			"--state", "open",
			"--json", "number,title,author,state,mergeStateStatus,url",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return errMsg{fmt.Errorf("gh pr list failed: %w (%s)", err, string(out))}
		}

		var raw []struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			State  string `json:"state"`
			URL    string `json:"url"`
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			MergeStateStatus string `json:"mergeStateStatus"`
		}

		if err := json.Unmarshal(out, &raw); err != nil {
			return errMsg{fmt.Errorf("failed to parse gh pr list output: %w", err)}
		}

		prs := make([]pr, 0, len(raw))
		for _, r := range raw {
			ms := r.MergeStateStatus
			if ms == "" {
				ms = "unknown"
			}
			prs = append(prs, pr{
				Number:     r.Number,
				Title:      r.Title,
				Author:     r.Author.Login,
				State:      r.State,
				MergeState: ms,
				URL:        r.URL,
			})
		}

		return prListMsg(prs)
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func mergePRCmd(prNumber int) tea.Cmd {
	return func() tea.Msg {
		// Auto-merge using regular merge; change to --squash/--rebase if you prefer.
		cmd := exec.Command("gh", "pr", "merge", "--auto", "--merge", strconv.Itoa(prNumber))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return mergeResultMsg{prNumber: prNumber, err: fmt.Errorf("gh pr merge failed: %w (%s)", err, string(out))}
		}
		return mergeResultMsg{prNumber: prNumber, err: nil}
	}
}

func checkMergedCmd(prNumber int, title string) tea.Cmd {
	return func() tea.Msg {
		// Ask gh if the PR is merged.
		cmd := exec.Command("gh", "pr", "view", strconv.Itoa(prNumber),
			"--json", "merged",
			"--jq", ".merged",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return checkMergedMsg{
				prNumber: prNumber,
				merged:   false,
				err:      fmt.Errorf("gh pr view failed: %w (%s)", err, string(out)),
			}
		}

		mergedStr := strings.TrimSpace(string(out))
		merged := mergedStr == "true"

		if !merged {
			// Send desktop notification if the PR is not merged yet.
			// This assumes Linux with notify-send available.
			notifyTitle := "PR not merged"
			notifyBody := fmt.Sprintf("PR #%d (%s) is still not merged after auto-merge.", prNumber, title)
			_ = exec.Command("notify-send", notifyTitle, notifyBody, "-u", "critical").Run()
		}

		return checkMergedMsg{
			prNumber: prNumber,
			merged:   merged,
			err:      nil,
		}
	}
}

// ---------- Model ----------

type mode int

const (
	modeListing mode = iota
	modeScheduling
)

type model struct {
	width, height int

	list      list.Model
	prs       []pr
	onlyMine  bool
	me        string
	status    string
	lastErr   error
	mode      mode
	input     textinput.Model
	schedFor  *pr
	scheduled []scheduledMerge
	now       time.Time
}

// ---------- Init ----------

func initialModel() model {
	ti := textinput.New()
	ti.Placeholder = "YYYY-MM-DD HH:MM or 'now'"
	ti.CharLimit = 32
	ti.Prompt = "Schedule at> "

	delegate := list.NewDefaultDelegate()
	l := list.New([]list.Item{}, delegate, 0, 0)
	l.Title = "Open pull requests"
	l.SetShowHelp(true)

	return model{
		list:      l,
		status:    "Loading...",
		mode:      modeListing,
		input:     ti,
		scheduled: []scheduledMerge{},
		now:       time.Now(),
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		fetchMeCmd(),
		fetchPRsCmd(),
		tickCmd(),
	)
}

// ---------- Helpers ----------

func (m *model) applyFilter() {
	var filtered []pr
	if m.onlyMine && m.me != "" {
		for _, p := range m.prs {
			if p.Author == m.me {
				filtered = append(filtered, p)
			}
		}
	} else {
		filtered = m.prs
	}

	items := make([]list.Item, 0, len(filtered))
	for _, p := range filtered {
		items = append(items, prItem{p: p})
	}
	m.list.SetItems(items)
	m.status = fmt.Sprintf("%d PRs (onlyMine=%v)", len(filtered), m.onlyMine)
}

func (m *model) findScheduledIndex(prNumber int) int {
	for i, s := range m.scheduled {
		if s.PR.Number == prNumber && !s.Done {
			return i
		}
	}
	return -1
}

// ---------- Update ----------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.list.SetSize(m.width, m.height-5)
		return m, nil

	case meMsg:
		m.me = string(msg)
		m.status = "Loaded GitHub user: " + m.me
		m.applyFilter()
		return m, nil

	case prListMsg:
		m.prs = msg
		m.applyFilter()
		return m, nil

	case errMsg:
		m.lastErr = msg.err
		m.status = "Error: " + msg.err.Error()
		return m, nil

	case tickMsg:
		m.now = time.Time(msg)
		// For each scheduled merge, decide whether to trigger actions.
		var cmds []tea.Cmd
		for i := range m.scheduled {
			s := &m.scheduled[i]
			if s.Done {
				continue
			}
			// Trigger auto-merge once the scheduled time has passed.
			if !s.MergeTriggered && !s.When.IsZero() && m.now.After(s.When) {
				s.MergeTriggered = true
				s.LastMessage = fmt.Sprintf("Triggering auto-merge for PR #%d", s.PR.Number)
				cmds = append(cmds, mergePRCmd(s.PR.Number))
			}
			// After we have a CheckAt time and it's passed, schedule a check.
			if s.MergeTriggered && !s.CheckScheduled && !s.CheckAt.IsZero() && m.now.After(s.CheckAt) {
				s.CheckScheduled = true
				s.LastMessage = fmt.Sprintf("Checking merge status for PR #%d", s.PR.Number)
				cmds = append(cmds, checkMergedCmd(s.PR.Number, s.PR.Title))
			}
		}
		// Keep ticking.
		cmds = append(cmds, tickCmd())
		return m, tea.Batch(cmds...)

	case mergeResultMsg:
		idx := m.findScheduledIndex(msg.prNumber)
		if idx >= 0 {
			if msg.err != nil {
				m.scheduled[idx].Done = true
				m.scheduled[idx].LastMessage = "Auto-merge failed: " + msg.err.Error()
				m.status = m.scheduled[idx].LastMessage
			} else {
				// Auto-merge set; schedule the check 1 minute later.
				m.scheduled[idx].CheckAt = m.now.Add(1 * time.Minute)
				m.scheduled[idx].LastMessage = "Auto-merge set, will check in 1 minute"
				m.status = fmt.Sprintf("PR #%d: auto-merge set; check at %s", msg.prNumber, m.scheduled[idx].CheckAt.Format(time.RFC3339))
			}
		}
		return m, nil

	case checkMergedMsg:
		idx := m.findScheduledIndex(msg.prNumber)
		if idx >= 0 {
			m.scheduled[idx].Done = true
			if msg.err != nil {
				m.scheduled[idx].LastMessage = "Check failed: " + msg.err.Error()
			} else if msg.merged {
				m.scheduled[idx].LastMessage = "PR is merged"
			} else {
				m.scheduled[idx].LastMessage = "PR is still not merged (notification sent)"
			}
			m.status = fmt.Sprintf("PR #%d: %s", msg.prNumber, m.scheduled[idx].LastMessage)
		}
		return m, nil

	case tea.KeyMsg:
		if m.mode == modeScheduling {
			return m.updateSchedulingKey(msg)
		}
		return m.updateListingKey(msg)

	default:
		// Let list/textinput handle other messages when appropriate.
		if m.mode == modeScheduling {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}
}

func (m model) updateListingKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "m":
		m.onlyMine = !m.onlyMine
		m.applyFilter()
		return m, nil

	case "r":
		m.status = "Refreshing PR list..."
		return m, fetchPRsCmd()

	case "enter":
		// Start scheduling for selected PR.
		if item, ok := m.list.SelectedItem().(prItem); ok {
			p := item.p
			// Avoid scheduling duplicates for same PR if one is already active.
			if m.findScheduledIndex(p.Number) >= 0 {
				m.status = fmt.Sprintf("PR #%d already has a scheduled merge", p.Number)
				return m, nil
			}
			m.schedFor = &p
			m.input.SetValue("")
			m.input.CursorEnd()
			m.input.Focus()
			m.mode = modeScheduling
			m.status = fmt.Sprintf("Scheduling auto-merge for PR #%d", p.Number)
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) updateSchedulingKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		// Parse date/time and create schedule.
		raw := strings.TrimSpace(m.input.Value())
		var when time.Time
		var err error

		if raw == "" || strings.ToLower(raw) == "now" {
			when = m.now
		} else {
			layout := "2006-01-02 15:04"
			when, err = time.ParseInLocation(layout, raw, time.Local)
		}

		if err != nil {
			m.status = "Invalid time format. Use YYYY-MM-DD HH:MM or 'now'."
			return m, nil
		}
		if m.schedFor == nil {
			m.status = "No PR selected to schedule."
			m.mode = modeListing
			m.input.Blur()
			return m, nil
		}

		s := scheduledMerge{
			PR:      *m.schedFor,
			When:    when,
			CheckAt: time.Time{}, // set after auto-merge triggers
		}
		m.scheduled = append(m.scheduled, s)
		m.status = fmt.Sprintf("Scheduled auto-merge for PR #%d at %s", s.PR.Number, when.Format("2006-01-02 15:04"))
		m.mode = modeListing
		m.input.Blur()
		m.schedFor = nil
		return m, nil

	case tea.KeyEsc:
		m.mode = modeListing
		m.input.Blur()
		m.status = "Scheduling cancelled"
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// ---------- View ----------

func (m model) View() string {
	headerStyle := lipgloss.NewStyle().Bold(true)
	statusStyle := lipgloss.NewStyle().Faint(true)
	errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("9"))

	var b strings.Builder

	// Header
	b.WriteString(headerStyle.Render("GitHub PR Auto-Merge Scheduler"))
	b.WriteString("\n")

	// Current filter/user
	filterInfo := fmt.Sprintf("onlyMine=%v", m.onlyMine)
	if m.me != "" {
		filterInfo += " | me=@" + m.me
	}
	b.WriteString(filterInfo)
	b.WriteString("\n\n")

	// Main content
	if m.mode == modeScheduling {
		b.WriteString("Enter schedule time (local):\n")
		b.WriteString(m.input.View())
		b.WriteString("\n\n")
	} else {
		b.WriteString(m.list.View())
		b.WriteString("\n")
	}

	// Scheduled jobs summary (short)
	if len(m.scheduled) > 0 {
		b.WriteString("Scheduled merges:\n")
		for _, s := range m.scheduled {
			state := "pending"
			if s.Done {
				state = "done"
			} else if s.MergeTriggered && !s.CheckScheduled {
				state = "auto-merge set, waiting to check"
			} else if s.CheckScheduled && !s.Done {
				state = "checking..."
			}
			b.WriteString(fmt.Sprintf("  #%d at %s [%s]",
				s.PR.Number,
				s.When.Format("2006-01-02 15:04"),
				state,
			))
			if s.LastMessage != "" {
				b.WriteString(" - " + s.LastMessage)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Status line + error if any
	if m.lastErr != nil {
		b.WriteString(errStyle.Render("Error: " + m.lastErr.Error()))
		b.WriteString("\n")
	}
	b.WriteString(statusStyle.Render(m.status))
	b.WriteString("\n")

	return b.String()
}

// ---------- main ----------

func main() {
	p := tea.NewProgram(initialModel())
	if err := p.Start(); err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}
}
