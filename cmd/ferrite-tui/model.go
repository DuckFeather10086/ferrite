package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// pollInterval refreshes adapter occupancy and recording state. The daemon
// answers /api/status from memory, so this is cheap.
const pollInterval = 2 * time.Second

// scheduleWindow is how much guide data to pull for the selected channel.
const scheduleWindow = 6 * time.Hour

type view int

const (
	viewChannels view = iota
	viewRecordings
)

// Model is the whole remote. It owns no TV state of its own — everything
// authoritative lives in the daemon and arrives via polling — which is what
// lets a second remote (phone, web UI) stay consistent with this one.
type Model struct {
	client *Client
	player *Player

	channels   []Channel
	cursor     int
	status     Status
	schedules  map[uint16][]Event
	recordings []Recording
	recCursor  int

	view view
	// pendingDelete is the recording awaiting a y/n confirmation. Deleting
	// unlinks the file, which is not undoable, so it never happens on a
	// single keystroke. 0 means nothing is pending.
	pendingDelete int64
	busy          string // in-flight long operation, shown in the status bar
	frame         int    // animation frame for the busy indicator
	message       string
	failure       string

	width, height int
	ready         bool
}

func NewModel(c *Client, p *Player) Model {
	return Model{client: c, player: p, schedules: map[uint16][]Event{}}
}

// ── messages ───────────────────────────────────────────────────────

type channelsMsg struct {
	channels []Channel
	err      error
}
type statusMsg struct {
	status Status
	err    error
}
type scheduleMsg struct {
	serviceID uint16
	events    []Event
	err       error
}
type switchMsg struct {
	result SwitchResult
	err    error
}
type recordMsg struct {
	result RecordResult
	err    error
}
type stopRecMsg struct {
	id  int64
	err error
}
type recordingsMsg struct {
	recordings []Recording
	err        error
}
type deleteRecMsg struct {
	id          int64
	fileDeleted bool
	err         error
}
type tickMsg time.Time

// ── commands ───────────────────────────────────────────────────────

func (m Model) fetchChannels() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ch, err := m.client.Channels(ctx)
		return channelsMsg{channels: ch, err: err}
	}
}

func (m Model) fetchStatus() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st, err := m.client.Status(ctx)
		return statusMsg{status: st, err: err}
	}
}

func (m Model) fetchSchedule(serviceID uint16) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		now := time.Now()
		ev, err := m.client.Schedule(ctx, serviceID, now.Add(-time.Hour), now.Add(scheduleWindow))
		return scheduleMsg{serviceID: serviceID, events: ev, err: err}
	}
}

func (m Model) fetchRecordings() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rec, err := m.client.Recordings(ctx)
		return recordingsMsg{recordings: rec, err: err}
	}
}

func (m Model) switchTo(channel string) tea.Cmd {
	return func() tea.Msg {
		// No timeout beyond the client's: a cold tune is legitimately slow.
		res, err := m.client.Switch(context.Background(), channel)
		return switchMsg{result: res, err: err}
	}
}

func (m Model) recordNow(channel string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, err := m.client.Record(ctx, channel, "", 0)
		return recordMsg{result: res, err: err}
	}
}

func (m Model) stopRecording(id int64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return stopRecMsg{id: id, err: m.client.StopRecording(ctx, id)}
	}
}

func (m Model) deleteRecording(id int64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		fileDeleted, err := m.client.DeleteRecording(ctx, id)
		return deleteRecMsg{id: id, fileDeleted: fileDeleted, err: err}
	}
}

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.fetchChannels(), m.fetchStatus(), tick())
}

// ── update ─────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height, m.ready = msg.Width, msg.Height, true
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		m.frame++
		cmds := []tea.Cmd{tick(), m.fetchStatus()}
		// Keep the guide for the selected channel warm.
		if ch, ok := m.selected(); ok {
			if _, cached := m.schedules[ch.ServiceID]; !cached {
				cmds = append(cmds, m.fetchSchedule(ch.ServiceID))
			}
		}
		if m.view == viewRecordings {
			cmds = append(cmds, m.fetchRecordings())
		}
		return m, tea.Batch(cmds...)

	case channelsMsg:
		if msg.err != nil {
			m.failure = "channels: " + msg.err.Error()
			return m, nil
		}
		m.channels, m.failure = msg.channels, ""
		if ch, ok := m.selected(); ok {
			return m, m.fetchSchedule(ch.ServiceID)
		}
		return m, nil

	case statusMsg:
		if msg.err != nil {
			m.failure = "status: " + msg.err.Error()
			return m, nil
		}
		m.status, m.failure = msg.status, ""
		return m, nil

	case scheduleMsg:
		if msg.err == nil {
			m.schedules[msg.serviceID] = msg.events
		}
		return m, nil

	case recordingsMsg:
		if msg.err != nil {
			m.failure = "recordings: " + msg.err.Error()
			return m, nil
		}
		m.recordings = msg.recordings
		if m.recCursor >= len(m.recordings) {
			m.recCursor = max(0, len(m.recordings)-1)
		}
		return m, nil

	case switchMsg:
		m.busy = ""
		if msg.err != nil {
			m.failure = switchFailure(msg.err)
			return m, nil
		}
		m.failure = ""
		// One URL for live TV, whatever is tuned — see Client.StreamURL.
		url := m.client.StreamURL(m.status.Stream)
		shown := m.displayFor(msg.result.Channel)
		if err := m.player.Play(msg.result.Channel, url); err != nil {
			// Not a failure of the TV: the stream is up, we just aren't the
			// ones showing it. Hand over the URL so it can be opened
			// wherever the user actually is — and the watch block below
			// lists the same playlist on every address of the daemon.
			switch {
			case errors.Is(err, ErrNoDisplay):
				m.message = "now on " + shown + " — no display here, open: " + url
			case errors.Is(err, ErrPlaybackDisabled):
				m.message = "now on " + shown + " — " + url
			default:
				m.failure = "player: " + err.Error()
			}
			return m, m.fetchStatus()
		}
		m.message = "watching " + shown
		return m, m.fetchStatus()

	case recordMsg:
		m.busy = ""
		if msg.err != nil {
			m.failure = recordFailure(msg.err)
			return m, nil
		}
		m.failure = ""
		title := msg.result.Title
		if title == "" {
			title = "untitled"
		}
		m.message = fmt.Sprintf("recording %s (%s) — id %d, R to stop",
			m.displayFor(msg.result.Channel), title, msg.result.ID)
		return m, tea.Batch(m.fetchStatus(), m.fetchRecordings())

	case stopRecMsg:
		m.busy = ""
		if msg.err != nil {
			m.failure = "stop: " + msg.err.Error()
			return m, nil
		}
		m.failure = ""
		m.message = fmt.Sprintf("stopped recording %d", msg.id)
		return m, tea.Batch(m.fetchStatus(), m.fetchRecordings())

	case deleteRecMsg:
		m.busy = ""
		if msg.err != nil {
			m.failure = deleteFailure(msg.err)
			return m, m.fetchRecordings()
		}
		m.failure = ""
		if msg.fileDeleted {
			m.message = fmt.Sprintf("deleted recording %d and its file", msg.id)
		} else {
			// The row is gone either way; say which, so "where did my file
			// go" has an answer.
			m.message = fmt.Sprintf("deleted recording %d (no file was on disk)", msg.id)
		}
		return m, tea.Batch(m.fetchStatus(), m.fetchRecordings())
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A pending delete swallows the next keystroke: only y confirms, and
	// anything else cancels rather than falling through to a command the
	// user thought they were answering a question with.
	if m.pendingDelete != 0 {
		id := m.pendingDelete
		m.pendingDelete = 0
		if msg.String() == "y" {
			m.busy = fmt.Sprintf("deleting recording %d", id)
			m.message, m.failure = "", ""
			return m, m.deleteRecording(id)
		}
		m.message = "delete canceled"
		return m, nil
	}

	switch msg.String() {
	case "q", "esc", "ctrl+c":
		m.player.Stop()
		return m, tea.Quit

	case "j", "down":
		m.moveCursor(1)
		return m, m.scheduleForSelectionIfNeeded()

	case "k", "up":
		m.moveCursor(-1)
		return m, m.scheduleForSelectionIfNeeded()

	case "enter":
		if m.busy != "" {
			return m, nil
		}
		// In the recordings view the cursor is on a recording, not a
		// channel — enter plays that file instead of tuning whatever the
		// channel list happened to be left on.
		if m.view == viewRecordings {
			return m.playSelectedRecording()
		}
		ch, ok := m.selected()
		if !ok {
			return m, nil
		}
		m.busy = "tuning " + ch.Display()
		m.message, m.failure = "", ""
		return m, m.switchTo(ch.Name)

	case "d":
		rec, ok := m.selectedRecording()
		if !ok || m.busy != "" {
			return m, nil
		}
		m.pendingDelete = rec.ID
		m.message, m.failure = "", ""
		return m, nil

	case "r":
		ch, ok := m.selected()
		if !ok || m.busy != "" {
			return m, nil
		}
		m.busy = "starting recording on " + ch.Display()
		m.message, m.failure = "", ""
		return m, m.recordNow(ch.Name)

	case "R":
		id, ok := m.stopTarget()
		if !ok {
			m.failure = "nothing is recording"
			return m, nil
		}
		m.busy = "stopping recording"
		return m, m.stopRecording(id)

	case "s":
		m.player.Stop()
		m.message = "playback stopped (the daemon keeps the tune until it idles out)"
		return m, nil

	case "l":
		if m.view == viewRecordings {
			m.view = viewChannels
			return m, nil
		}
		m.view = viewRecordings
		return m, m.fetchRecordings()

	case "g":
		cmds := []tea.Cmd{m.fetchChannels(), m.fetchStatus()}
		if ch, ok := m.selected(); ok {
			delete(m.schedules, ch.ServiceID)
			cmds = append(cmds, m.fetchSchedule(ch.ServiceID))
		}
		m.message = "refreshing"
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m *Model) moveCursor(delta int) {
	if m.view == viewRecordings {
		m.recCursor = clamp(m.recCursor+delta, 0, len(m.recordings)-1)
		return
	}
	m.cursor = clamp(m.cursor+delta, 0, len(m.channels)-1)
}

func (m Model) scheduleForSelectionIfNeeded() tea.Cmd {
	ch, ok := m.selected()
	if !ok {
		return nil
	}
	if _, cached := m.schedules[ch.ServiceID]; cached {
		return nil
	}
	return m.fetchSchedule(ch.ServiceID)
}

// playSelectedRecording sends the highlighted recording to the local
// player. The daemon serves the file over HTTP (Range included), so
// nothing is copied here first — and no tuner is involved, so watching a
// recording never interrupts live TV or another recording.
func (m Model) playSelectedRecording() (tea.Model, tea.Cmd) {
	rec, ok := m.selectedRecording()
	if !ok {
		return m, nil
	}
	if rec.SizeBytes != nil && *rec.SizeBytes == 0 {
		m.failure = fmt.Sprintf("recording %d is empty", rec.ID)
		if rec.Error != "" {
			m.failure += ": " + rec.Error
		}
		return m, nil
	}

	url := m.client.RecordingFileURL(rec.ID)
	label := fmt.Sprintf("rec %d %s", rec.ID, firstLine(rec.Title))
	if err := m.player.PlayFile(label, url); err != nil {
		switch {
		case errors.Is(err, ErrNoDisplay):
			m.message = "no display here — open: " + url
		case errors.Is(err, ErrPlaybackDisabled):
			m.message = url
		default:
			m.failure = "player: " + err.Error()
		}
		return m, nil
	}
	m.failure = ""
	m.message = "playing " + label
	if rec.State == "recording" {
		// The endpoint serves the bytes written so far, so playback ends at
		// whatever the file was when mpv opened it.
		m.message += " (still recording — plays up to the current end)"
	}
	return m, nil
}

func (m Model) selected() (Channel, bool) {
	if m.cursor < 0 || m.cursor >= len(m.channels) {
		return Channel{}, false
	}
	return m.channels[m.cursor], true
}

// displayFor maps a canonical channel name — what the daemon reports in
// adapter status and recording rows, and what every request takes — to the
// label a person should read. Unknown names pass through: a recording can
// name a channel that has since left channels.json.
func (m Model) displayFor(name string) string {
	for _, ch := range m.channels {
		if ch.Name == name {
			return ch.Display()
		}
	}
	return name
}

// selectedRecording is the highlighted row, and only in the recordings
// view — the channel list has its own cursor, and acting on a recording
// the user cannot see is how you delete the wrong one.
func (m Model) selectedRecording() (Recording, bool) {
	if m.view != viewRecordings {
		return Recording{}, false
	}
	if m.recCursor < 0 || m.recCursor >= len(m.recordings) {
		return Recording{}, false
	}
	return m.recordings[m.recCursor], true
}

// stopTarget picks which recording "R" ends: the selected row in the
// recordings view, otherwise the most recently started active one.
func (m Model) stopTarget() (int64, bool) {
	if m.view == viewRecordings {
		if rec, ok := m.selectedRecording(); ok && rec.State == "recording" {
			return rec.ID, true
		}
		return 0, false
	}
	if len(m.status.Recording) == 0 {
		return 0, false
	}
	ids := append([]int64{}, m.status.Recording...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	return ids[0], true
}

// nowNext returns the airing and following events for a channel.
func (m Model) nowNext(serviceID uint16) (*Event, []Event) {
	events := m.schedules[serviceID]
	now := time.Now()
	var current *Event
	var upcoming []Event
	for i := range events {
		switch {
		case events[i].Airing(now):
			current = &events[i]
		case events[i].Start.After(now):
			upcoming = append(upcoming, events[i])
		}
	}
	return current, upcoming
}

func switchFailure(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Busy() {
		return "tuner busy: " + apiErr.Message
	}
	return "switch: " + err.Error()
}

func recordFailure(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Busy() {
		return "cannot record: " + apiErr.Message
	}
	return "record: " + err.Error()
}

// deleteFailure surfaces the daemon's own text for the one refusal that is
// actionable: a recording still in progress has to be stopped first.
func deleteFailure(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		return "cannot delete: " + apiErr.Message
	}
	return "delete: " + err.Error()
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ── view ───────────────────────────────────────────────────────────

var (
	styleTitle    = lipgloss.NewStyle().Bold(true)
	styleCursor   = lipgloss.NewStyle().Bold(true).Reverse(true)
	styleDim      = lipgloss.NewStyle().Faint(true)
	styleLive     = lipgloss.NewStyle().Bold(true)
	styleRec      = lipgloss.NewStyle().Bold(true)
	styleFailure  = lipgloss.NewStyle().Bold(true)
	styleStatusBr = lipgloss.NewStyle().Faint(true)
)

const busyFrames = `|/-\`

func (m Model) View() string {
	if !m.ready {
		return "connecting to " + m.client.BaseURL + "…\n"
	}

	header := m.renderHeader()
	footerParts := []string{m.renderStatusBar(), m.renderKeys()}
	if m.view == viewChannels {
		// The watch URLs belong to the live view; the recordings list would
		// rather have the rows.
		footerParts = append([]string{m.renderWatchURLs()}, footerParts...)
	}
	footer := strings.Join(footerParts, "\n")

	// Everything the frame draws is counted here — header, the blank line
	// under it, footer — so adding a line to any of them cannot silently
	// push the cursor, or the key hints, off the bottom of the screen.
	rows := m.height - m.headerHeight() - 1 - countLines(footer)
	if rows < 3 {
		rows = 3
	}

	var body string
	if m.view == viewRecordings {
		body = m.renderRecordings(rows)
	} else {
		listWidth := m.width / 3
		if listWidth < 24 {
			listWidth = 24
		}
		guideWidth := m.width - listWidth - 3
		if guideWidth < 20 {
			guideWidth = 20
		}
		left := lipgloss.NewStyle().Width(listWidth).Render(m.renderChannels(rows))
		right := lipgloss.NewStyle().Width(guideWidth).Render(m.renderGuide(rows))
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right)
	}

	// Height pins the footer to the bottom of the terminal instead of
	// letting it float under a short list; MaxHeight is the backstop that
	// keeps a long guide from pushing it off.
	body = lipgloss.NewStyle().Height(rows).MaxHeight(rows).
		Render(strings.TrimRight(body, "\n"))

	return strings.Join([]string{header, "", body, footer}, "\n")
}

// countLines is how many terminal rows s occupies. An empty string is one
// row (a blank line), which is what the status bar renders when it has
// nothing to say.
func countLines(s string) int {
	return strings.Count(s, "\n") + 1
}

// renderChannels draws the channel list in rows terminal rows, title
// included.
func (m Model) renderChannels(rows int) string {
	live := m.status.LiveChannel()
	playing := m.player.Playing()

	var b strings.Builder
	b.WriteString(styleTitle.Render("Channels") + "\n")
	if len(m.channels) == 0 {
		b.WriteString(styleDim.Render("  (none)") + "\n")
		return b.String()
	}

	// Keep the cursor on screen for long lists. Two rows of the budget are
	// already spent: the title above, and the "… N more" line below.
	visible := rows - 2
	if visible < 3 {
		visible = 3
	}
	start := 0
	if m.cursor >= visible {
		start = m.cursor - visible + 1
	}
	end := min(len(m.channels), start+visible)

	for i := start; i < end; i++ {
		ch := m.channels[i]
		marker := " "
		switch {
		case ch.Name == playing:
			marker = styleLive.Render("▶")
		case ch.Name == live:
			marker = styleDim.Render("·")
		}
		line := fmt.Sprintf("%s %s", marker, ch.Display())
		if i == m.cursor {
			line = styleCursor.Render(line)
		}
		b.WriteString(line + "\n")
	}
	if end < len(m.channels) {
		b.WriteString(styleDim.Render(fmt.Sprintf("  … %d more", len(m.channels)-end)) + "\n")
	}
	return b.String()
}

// renderGuide draws now-and-next for the selected channel, bounded to rows
// so a channel with a dense schedule can't shove the footer off screen.
func (m Model) renderGuide(rows int) string {
	ch, ok := m.selected()
	if !ok {
		return ""
	}
	var b strings.Builder
	// The canonical name comes along when it differs from the label: it is
	// what /api/... and `dvb-rs tune` take, so it is worth being able to
	// read it off the screen.
	ident := fmt.Sprintf("  (sid %d)", ch.ServiceID)
	if ch.Display() != ch.Name {
		ident = fmt.Sprintf("  (%s · sid %d)", ch.Name, ch.ServiceID)
	}
	b.WriteString(styleTitle.Render(ch.Display()) + styleDim.Render(ident) + "\n")

	current, upcoming := m.nowNext(ch.ServiceID)
	if current == nil && len(upcoming) == 0 {
		b.WriteString(styleDim.Render("no guide data — EPG may not have scanned this channel yet") + "\n")
		return b.String()
	}
	if current != nil {
		b.WriteString(styleLive.Render("NOW  ") + timeRange(*current) + "  " + current.Title + "\n")
		if current.Synopsis != "" {
			b.WriteString(styleDim.Render("     "+truncate(current.Synopsis, 200)) + "\n")
		}
	}
	// The title line plus NOW and its synopsis are already spent.
	next := min(rows-3, 8)
	for i, ev := range upcoming {
		if i >= next {
			break
		}
		b.WriteString(styleDim.Render("next ") + timeRange(ev) + "  " + ev.Title + "\n")
	}
	return b.String()
}

// renderRecordings draws the recordings list in rows terminal rows.
func (m Model) renderRecordings(rows int) string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Recordings") + "\n")
	if len(m.recordings) == 0 {
		b.WriteString(styleDim.Render("  (none)") + "\n")
	}
	for i, rec := range m.recordings {
		if i >= rows-2 {
			b.WriteString(styleDim.Render(fmt.Sprintf("  … %d more", len(m.recordings)-i)) + "\n")
			break
		}
		state := rec.State
		if state == "recording" {
			state = styleRec.Render("● REC")
		}
		// Columns are laid out by display width, not %-Ns: a channel name
		// in kana is 3 bytes and 2 cells per rune, and the state cell
		// carries ANSI. fmt would count both and shear the table.
		line := cell(state, 7) + cell(m.displayFor(rec.Channel), 16) +
			cell(rec.Start.Local().Format("01-02 15:04"), 14) +
			cellRight(humanSize(rec.SizeBytes), 8) + "  " + firstLine(rec.Title)
		if i == m.recCursor {
			line = styleCursor.Render(line)
		}
		b.WriteString(line + "\n")
		if rec.Error != "" {
			b.WriteString(styleDim.Render("       "+rec.Error) + "\n")
		}
	}
	return b.String()
}

func (m Model) renderStatusBar() string {
	parts := []string{}

	if playing := m.player.Playing(); playing != "" {
		parts = append(parts, styleLive.Render("▶ "+m.displayFor(playing)))
	} else if live := m.status.LiveChannel(); live != "" {
		parts = append(parts, "tuned "+m.displayFor(live))
	} else {
		parts = append(parts, styleDim.Render("off"))
	}

	for _, a := range m.status.Adapters {
		switch {
		case a.Reserved:
			parts = append(parts, fmt.Sprintf("a%d EPG", a.Adapter))
		case a.Channel != "":
			parts = append(parts, fmt.Sprintf("a%d %s×%d/%s",
				a.Adapter, m.displayFor(a.Channel), a.Refs, a.Prio))
		default:
			parts = append(parts, fmt.Sprintf("a%d idle", a.Adapter))
		}
	}
	if n := len(m.status.Recording); n > 0 {
		parts = append(parts, styleRec.Render(fmt.Sprintf("● REC ×%d", n)))
	}

	bar := styleStatusBr.Render(strings.Join(parts, " │ "))

	switch {
	case m.pendingDelete != 0:
		return bar + "\n" + styleFailure.Render(
			fmt.Sprintf("delete recording %d and its file?  y / any other key",
				m.pendingDelete))
	case m.busy != "":
		frame := string(busyFrames[m.frame%len(busyFrames)])
		return bar + "\n" + frame + " " + m.busy + "…"
	case m.failure != "":
		return bar + "\n" + styleFailure.Render("✗ "+m.failure)
	case m.message != "":
		return bar + "\n" + m.message
	}
	return bar + "\n"
}

func (m Model) renderKeys() string {
	if m.view == viewRecordings {
		return styleDim.Render("j/k select  ⏎ play  d delete  R stop selected  l back  g refresh  q quit")
	}
	return styleDim.Render("j/k select  ⏎ watch  r rec  R stop rec  s stop player  l recordings  g refresh  q quit")
}

// cell renders s padded (or truncated) to w terminal cells. Unlike
// fmt's %-Ns this counts what the terminal draws: wide CJK runes as two
// columns and ANSI escapes as none.
func cell(s string, w int) string {
	return lipgloss.NewStyle().Width(w).MaxWidth(w).Render(s)
}

func cellRight(s string, w int) string {
	return lipgloss.NewStyle().Width(w).MaxWidth(w).Align(lipgloss.Right).Render(s)
}

func timeRange(e Event) string {
	return e.Start.Local().Format("15:04") + "-" + e.End().Local().Format("15:04")
}

func humanSize(n *int64) string {
	if n == nil {
		return "-"
	}
	v := float64(*n)
	for _, unit := range []string{"B", "K", "M", "G"} {
		if v < 1024 {
			return fmt.Sprintf("%.0f%s", v, unit)
		}
		v /= 1024
	}
	return fmt.Sprintf("%.1fT", v)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
