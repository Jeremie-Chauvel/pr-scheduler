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
//   - Enter: schedule auto-merge for selected PR (opens time picker)
//   - m: toggle "only my PRs"
//   - r: refresh PR list
//   - q: quit (will warn if there are active scheduled merges)
//
// Time picker:
//   - Navigate with Up/Down or j/k
//   - Select from presets: Now, 5min, 15min, 30min, 1h, 2h, 4h, 8h, 12h, 24h
//   - Choose "Custom time..." for manual entry (YYYY-MM-DD HH:MM format)
//   - Esc: cancel/go back
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

// Time preset for the time picker
type timePreset struct {
	Label       string
	Description string
	IsCustom    bool
	Duration    time.Duration // Used if not custom
}

type timePresetItem struct {
	preset timePreset
}

func (i timePresetItem) Title() string       { return i.preset.Label }
func (i timePresetItem) Description() string { return i.preset.Description }
func (i timePresetItem) FilterValue() string { return i.preset.Label }

// For scheduling auto-merge of a PR.
type scheduledMerge struct {
	PR                    pr
	When                  time.Time
	PreMergeCommentPosted bool
	MergeTriggered        bool
	CheckScheduled        bool
	CheckAt               time.Time
	FailureHandled        bool
	Done                  bool
	LastMessage           string
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
	commentResultMsg struct {
		prNumber    int
		isPreMerge  bool
		err         error
	}
	disableAutoMergeResultMsg struct {
		prNumber int
		err      error
	}
	commitSHAMsg struct {
		prNumber int
		sha      string
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

func commentPRCmd(prNumber int, body string, isPreMerge bool) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("gh", "pr", "comment", strconv.Itoa(prNumber), "--body", body)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return commentResultMsg{prNumber: prNumber, isPreMerge: isPreMerge, err: fmt.Errorf("gh pr comment failed: %w (%s)", err, string(out))}
		}
		return commentResultMsg{prNumber: prNumber, isPreMerge: isPreMerge, err: nil}
	}
}

func disableAutoMergeCmd(prNumber int) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("gh", "pr", "merge", "--disable-auto", strconv.Itoa(prNumber))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return disableAutoMergeResultMsg{prNumber: prNumber, err: fmt.Errorf("gh pr merge --disable-auto failed: %w (%s)", err, string(out))}
		}
		return disableAutoMergeResultMsg{prNumber: prNumber, err: nil}
	}
}

func getCommitSHACmd(prNumber int) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("gh", "pr", "view", strconv.Itoa(prNumber), "--json", "headRefOid", "--jq", ".headRefOid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return commitSHAMsg{prNumber: prNumber, sha: "", err: fmt.Errorf("gh pr view failed: %w (%s)", err, string(out))}
		}
		return commitSHAMsg{prNumber: prNumber, sha: strings.TrimSpace(string(out)), err: nil}
	}
}

func checkMergedCmd(prNumber int) tea.Cmd {
	return func() tea.Msg {
		// Ask gh if the PR is merged.
		cmd := exec.Command("gh", "pr", "view", strconv.Itoa(prNumber),
			"--json", "state",
			"--jq", ".state",
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
		merged := mergedStr == "MERGED"

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
	modeTimePicker
	modeScheduling
)

type model struct {
	width, height int

	list         list.Model
	timePicker   list.Model
	prs          []pr
	onlyMine     bool
	me           string
	status       string
	lastErr      error
	mode         mode
	input        textinput.Model
	schedFor     *pr
	scheduled    []scheduledMerge
	now          time.Time
	quitWarned   bool
}

// ---------- Init ----------

func getTimePresets() []list.Item {
	presets := []timePreset{
		{Label: "Now", Description: "Merge immediately", IsCustom: false, Duration: 0},
		{Label: "In 1 minute", Description: "Merge in 1 minute", IsCustom: false, Duration: 1 * time.Minute},
		{Label: "In 2 minutes", Description: "Merge in 2 minutes", IsCustom: false, Duration: 2 * time.Minute},
		{Label: "In 5 minutes", Description: "Merge in 5 minutes", IsCustom: false, Duration: 5 * time.Minute},
		{Label: "In 15 minutes", Description: "Merge in 15 minutes", IsCustom: false, Duration: 15 * time.Minute},
		{Label: "In 30 minutes", Description: "Merge in 30 minutes", IsCustom: false, Duration: 30 * time.Minute},
		{Label: "In 1 hour", Description: "Merge in 1 hour", IsCustom: false, Duration: 1 * time.Hour},
		{Label: "In 2 hours", Description: "Merge in 2 hours", IsCustom: false, Duration: 2 * time.Hour},
		{Label: "In 4 hours", Description: "Merge in 4 hours", IsCustom: false, Duration: 4 * time.Hour},
		{Label: "In 8 hours", Description: "Merge in 8 hours", IsCustom: false, Duration: 8 * time.Hour},
		{Label: "In 12 hours", Description: "Merge in 12 hours", IsCustom: false, Duration: 12 * time.Hour},
		{Label: "In 24 hours", Description: "Merge tomorrow at this time", IsCustom: false, Duration: 24 * time.Hour},
		{Label: "Custom time...", Description: "Enter a custom date/time", IsCustom: true, Duration: 0},
	}

	items := make([]list.Item, len(presets))
	for i, p := range presets {
		items[i] = timePresetItem{preset: p}
	}
	return items
}

func initialModel() model {
	ti := textinput.New()
	ti.Placeholder = "YYYY-MM-DD HH:MM or 'now'"
	ti.CharLimit = 32
	ti.Prompt = "Schedule at> "

	delegate := list.NewDefaultDelegate()
	l := list.New([]list.Item{}, delegate, 0, 0)
	l.Title = "Open pull requests"
	l.SetShowHelp(true)

	// Time picker list
	timeDelegate := list.NewDefaultDelegate()
	tp := list.New(getTimePresets(), timeDelegate, 0, 0)
	tp.Title = "When to merge?"
	tp.SetShowHelp(false)
	tp.SetFilteringEnabled(false)

	return model{
		list:       l,
		timePicker: tp,
		status:     "Loading...",
		mode:       modeListing,
		input:      ti,
		scheduled:  []scheduledMerge{},
		now:        time.Now(),
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

func (m *model) hasActiveSchedules() bool {
	for _, s := range m.scheduled {
		if !s.Done {
			return true
		}
	}
	return false
}

// ---------- Update ----------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.list.SetSize(m.width, m.height-5)
		m.timePicker.SetSize(m.width, m.height-5)
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
			// First, post a comment before triggering auto-merge.
			if !s.PreMergeCommentPosted && !s.When.IsZero() && m.now.After(s.When) {
				s.PreMergeCommentPosted = true
				s.LastMessage = fmt.Sprintf("Posting pre-merge comment for PR #%d", s.PR.Number)
				comment := fmt.Sprintf("Setting PR to auto-merge. Scheduled merge time: %s", s.When.Format("2006-01-02 15:04"))
				cmds = append(cmds, commentPRCmd(s.PR.Number, comment, true))
			}
			// After we have a CheckAt time and it's passed, schedule a check.
			if s.MergeTriggered && !s.CheckScheduled && !s.CheckAt.IsZero() && m.now.After(s.CheckAt) {
				s.CheckScheduled = true
				s.LastMessage = fmt.Sprintf("Checking merge status for PR #%d", s.PR.Number)
				cmds = append(cmds, checkMergedCmd(s.PR.Number))
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
			if msg.err != nil {
				m.scheduled[idx].Done = true
				m.scheduled[idx].LastMessage = "Check failed: " + msg.err.Error()
				m.status = fmt.Sprintf("PR #%d: %s", msg.prNumber, m.scheduled[idx].LastMessage)
			} else if msg.merged {
				m.scheduled[idx].Done = true
				m.scheduled[idx].LastMessage = "PR is merged"
				m.status = fmt.Sprintf("PR #%d: %s", msg.prNumber, m.scheduled[idx].LastMessage)
			} else {
				// PR is not merged - get commit SHA to include in failure comment
				m.scheduled[idx].LastMessage = "PR not merged, fetching commit SHA..."
				m.status = fmt.Sprintf("PR #%d: %s", msg.prNumber, m.scheduled[idx].LastMessage)
				return m, getCommitSHACmd(msg.prNumber)
			}
		}
		return m, nil

	case commitSHAMsg:
		idx := m.findScheduledIndex(msg.prNumber)
		if idx >= 0 {
			s := &m.scheduled[idx]
			sha := msg.sha
			if msg.err != nil {
				sha = "unknown"
			}
			// Disable auto-merge and post failure comment
			s.FailureHandled = true
			s.LastMessage = "Disabling auto-merge and posting failure comment..."
			m.status = fmt.Sprintf("PR #%d: %s", msg.prNumber, s.LastMessage)

			failureComment := fmt.Sprintf("Auto-merge did not complete successfully. Disabling auto-merge.\n\nCurrent commit: %s", sha)

			// Send desktop notification
			notifyTitle := "PR not merged"
			notifyBody := fmt.Sprintf("PR #%d (%s) is still not merged after auto-merge.", msg.prNumber, s.PR.Title)
			_ = exec.Command("notify-send", notifyTitle, notifyBody, "-u", "critical").Run()

			return m, tea.Batch(
				disableAutoMergeCmd(msg.prNumber),
				commentPRCmd(msg.prNumber, failureComment, false),
			)
		}
		return m, nil

	case commentResultMsg:
		idx := m.findScheduledIndex(msg.prNumber)
		if idx >= 0 {
			if msg.isPreMerge {
				// Pre-merge comment posted, now trigger auto-merge
				if msg.err != nil {
					m.scheduled[idx].LastMessage = "Comment failed: " + msg.err.Error() + " (continuing with merge)"
				} else {
					m.scheduled[idx].LastMessage = "Pre-merge comment posted, triggering auto-merge..."
				}
				m.scheduled[idx].MergeTriggered = true
				m.status = fmt.Sprintf("PR #%d: %s", msg.prNumber, m.scheduled[idx].LastMessage)
				return m, mergePRCmd(msg.prNumber)
			} else {
				// Failure comment posted - mark as done
				m.scheduled[idx].Done = true
				if msg.err != nil {
					m.scheduled[idx].LastMessage = "PR not merged (failure comment failed: " + msg.err.Error() + ")"
				} else {
					m.scheduled[idx].LastMessage = "PR not merged (auto-merge disabled, notification sent)"
				}
				m.status = fmt.Sprintf("PR #%d: %s", msg.prNumber, m.scheduled[idx].LastMessage)
			}
		}
		return m, nil

	case disableAutoMergeResultMsg:
		idx := m.findScheduledIndex(msg.prNumber)
		if idx >= 0 {
			if msg.err != nil {
				// Log the error but don't fail - the comment is more important
				m.scheduled[idx].LastMessage = "Failed to disable auto-merge: " + msg.err.Error()
			}
			// Don't mark as done here - wait for the comment result
		}
		return m, nil

	case tea.KeyMsg:
		if m.mode == modeScheduling {
			return m.updateSchedulingKey(msg)
		} else if m.mode == modeTimePicker {
			return m.updateTimePickerKey(msg)
		}
		return m.updateListingKey(msg)

	default:
		// Let list/textinput handle other messages when appropriate.
		if m.mode == modeScheduling {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		} else if m.mode == modeTimePicker {
			var cmd tea.Cmd
			m.timePicker, cmd = m.timePicker.Update(msg)
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
		// Check if there are active scheduled merges
		if m.hasActiveSchedules() && !m.quitWarned {
			m.quitWarned = true
			m.status = "Warning: Active scheduled merges! Press 'q' again to force quit."
			return m, nil
		}
		return m, tea.Quit

	case "m":
		m.quitWarned = false // Reset quit warning
		m.onlyMine = !m.onlyMine
		m.applyFilter()
		return m, nil

	case "r":
		m.quitWarned = false // Reset quit warning
		m.status = "Refreshing PR list..."
		return m, fetchPRsCmd()

	case "enter":
		m.quitWarned = false // Reset quit warning
		// Start time picker for selected PR.
		if item, ok := m.list.SelectedItem().(prItem); ok {
			p := item.p
			// Avoid scheduling duplicates for same PR if one is already active.
			if m.findScheduledIndex(p.Number) >= 0 {
				m.status = fmt.Sprintf("PR #%d already has a scheduled merge", p.Number)
				return m, nil
			}
			m.schedFor = &p
			m.mode = modeTimePicker
			m.status = fmt.Sprintf("Select merge time for PR #%d", p.Number)
		}
		return m, nil
	}

	// Reset quit warning on any other key
	m.quitWarned = false
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) updateTimePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		// User selected a time preset
		if item, ok := m.timePicker.SelectedItem().(timePresetItem); ok {
			preset := item.preset

			if preset.IsCustom {
				// Switch to custom text input mode
				m.input.SetValue("")
				m.input.CursorEnd()
				m.input.Focus()
				m.mode = modeScheduling
				m.status = "Enter custom time for PR #" + strconv.Itoa(m.schedFor.Number)
				return m, nil
			}

			// Use the preset duration
			when := m.now.Add(preset.Duration)

			if m.schedFor == nil {
				m.status = "No PR selected to schedule."
				m.mode = modeListing
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
			m.schedFor = nil
			return m, nil
		}
		return m, nil

	case "esc", "q":
		m.mode = modeListing
		m.status = "Scheduling cancelled"
		m.schedFor = nil
		return m, nil
	}

	var cmd tea.Cmd
	m.timePicker, cmd = m.timePicker.Update(msg)
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
		m.mode = modeTimePicker
		m.input.Blur()
		m.status = "Back to time selection"
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
	} else if m.mode == modeTimePicker {
		b.WriteString(m.timePicker.View())
		b.WriteString("\n")
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
