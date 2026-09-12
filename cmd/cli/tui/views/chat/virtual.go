package chat

import (
	"fmt"
	"strings"
	"time"

	"github.com/yusheng-g/openagent-go/cmd/cli/tui/layout"
	"github.com/yusheng-g/openagent-go/cmd/cli/tui/theme"
	"github.com/yusheng-g/openagent-go/cmd/cli/tui/utils"
)

// ── line-level virtual scrolling (17.2) ──
//
// The viewport is fed a *windowed* document: messages whose row range
// intersects the visible window are styled (through the per-message render
// cache) and their rows are emitted verbatim; every other row is a cheap
// placeholder. Heights come from the render cache when the block was
// already styled (exact, free); cache misses estimate without styling —
// measuring every message up front is what made long-session loads slow.
// The settle pass in renderVirtualDocAt styles the window band and
// re-locates the window on exact heights before rows are cut, so drawn
// rows and window math never drift within what is visible; off-window
// estimates only shift the placeholder bulk and self-correct on reveal.

// placeholderRow is the muted gutter row shown for off-window lines. The
// padding is measured with the same width function fitRow uses, so the row
// is exactly vpW wide whatever go-runewidth decides for the rail glyph
// (box-drawing width varies with the EastAsianWidth flag some dependencies
// toggle globally) — uniform rows keep the soft-wrap mapping 1:1 with the
// virtual window.
func placeholderRow(vpW int) string {
	rail := "┆"
	return theme.BaseStyle().Background(theme.BgSurface).
		Foreground(theme.TextMute).Render(rail + strings.Repeat(" ", max(0, vpW-utils.DisplayWidth(rail))))
}

// padRow is a blank full-width page-background row used for the transcript's
// top padding (layout.TranscriptTopPad) — breathing room above the first
// message block.
func padRow(vpW int) string {
	return theme.BaseStyle().Render(strings.Repeat(" ", max(0, vpW)))
}

// fitRow normalizes an (ANSI-styled) transcript row to exactly vpW columns.
// bubbles viewport soft-wraps any row wider than its content width, which
// would shift its line↔row mapping away from our virtual window offsets;
// uniform rows keep that mapping 1:1. Long rows are cut to width with their
// styling preserved (a card's box-drawing rail measures double width under
// runewidth, so style-stripping here would drain every bordered row of its
// colors); short rows are padded with trailing spaces.
func fitRow(row string, vpW int) string {
	c := utils.DisplayWidth(row)
	switch {
	case c == vpW:
		return row
	case c > vpW:
		return utils.TruncateStyled(row, vpW)
	default:
		return row + strings.Repeat(" ", vpW-c)
	}
}

// virtualLineHeights returns the rendered height of every message, in
// viewport rows. Heights come from the per-message render cache when the
// block was already styled (exact, free); cache misses estimate without
// styling — styling every block up front is what made long-session loads
// slow, and off-window rows are placeholders anyway. The real render
// replaces the estimate as the window reveals the block (the settle pass
// in renderVirtualDocAt re-locates the window on exact heights first, so
// a visible block is never clipped by its own estimate). Hidden
// (visibility-gated) messages occupy no rows.
func (m *Model) virtualLineHeights(vpW int) []int {
	h := make([]int, len(m.messages))
	for i := range m.messages {
		if hh, ok := m.cachedMessageHeight(i, m.messages[i], vpW); ok {
			h[i] = hh
			continue
		}
		h[i] = m.estimateMessageHeight(i, m.messages[i], vpW)
	}
	return h
}

// cachedMessageHeight returns the height of message i from an existing
// render-cache entry (free). ok=false on miss or when the entry is stale
// for the current width/state. A gated-out message reports (0, true) — it
// occupies no rows.
func (m *Model) cachedMessageHeight(i int, msg ChatMessage, vpW int) (int, bool) {
	if msg.Seq == 0 {
		return 0, false
	}
	e, ok := m.renderCache[msg.Seq]
	if !ok {
		return 0, false
	}
	turnEnd := m.isTurnEndAt(i, msg)
	if !renderCacheHits(e, msg, vpW, m.loading, m.replaying, turnEnd, m.permissionReq != nil, m.visibleConfig, m.expandOverrideAt(msg.Seq)) {
		return 0, false
	}
	if e.skip || e.block == "" {
		return 0, true
	}
	return strings.Count(e.block, "\n") + 1, true
}

// estimateMessageHeight approximates a block's row count without styling,
// mirroring styleMessageBlock's role branches and visibility gates
// coarsely — ± a couple of rows is fine: off-window rows are placeholders,
// and the settle pass swaps in exact heights before rows are cut.
func (m *Model) estimateMessageHeight(i int, msg ChatMessage, vpW int) int {
	w := max(20, vpW-transcriptIndent-2) // inner text width, coarse
	rows := 2                            // card chrome: padding + separator rows
	switch msg.Role {
	case "thought":
		expanded := m.effectiveThoughtExpanded(msg, msg.ThoughtEnd.IsZero() && m.loading)
		if expanded {
			rows += 1 + wrappedRows(msg.Content, w)
		} else {
			rows++
		}
	case "compact":
		rows += wrappedRows(msg.Content, w)
	case "tool":
		if isSkillTool(msg.ToolName) && !m.visibleConfig.ShowToolSkill {
			return 0
		}
		if isShellTool(msg.ToolName) && !m.visibleConfig.ShowToolShell {
			return 0
		}
		if msg.ToolStatus == toolPending && m.permissionReq != nil {
			return 0
		}
		rows++ // status line
		if m.effectiveToolDetail(msg) && msg.ToolOutput != "" {
			rows += min(strings.Count(msg.ToolOutput, "\n")+1, defaultToolOutputLines) + 1
		}
	default:
		rows += wrappedRows(msg.Content, w)
	}
	if m.isTurnEndAt(i, msg) {
		rows += 3 // marker row with a blank row on each side
	}
	return rows
}

// wrappedRows counts display rows for text at width w: hard lines plus
// wrapped overflow, display-width aware (CJK counts double).
func wrappedRows(text string, w int) int {
	if text == "" {
		return 0
	}
	n := 0
	for _, ln := range strings.Split(text, "\n") {
		n += max(1, (utils.DisplayWidth(ln)+w-1)/w)
	}
	return n
}

// virtualPrefixLines returns the document row at which message idx starts:
// the transcript's top pad plus the sum of rendered rows before it. The
// viewport's YOffset lives in these document coordinates, so scroll-to-
// message jumps can use the value directly.
func (m *Model) virtualPrefixLines(idx int) int {
	vpW := layout.GetTranscriptWidth(m.width)
	n := layout.TranscriptTopPad
	for i, h := range m.virtualLineHeights(vpW) {
		if i >= idx {
			break
		}
		n += h
	}
	return n
}

// messageAtLine returns the message index whose estimated row range
// contains the given content line, plus the row offset within it. Lines
// past the end clamp into the last message.
func messageAtLine(heights []int, line int) (idx, within int) {
	if len(heights) == 0 {
		return -1, 0
	}
	line = max(0, line)
	for i, h := range heights {
		if line < h {
			return i, line
		}
		line -= h
	}
	last := len(heights) - 1
	return last, min(line, max(0, heights[last]-1))
}

// feedViewport refreshes the viewport content with the windowed document.
// It refeeds when the scroll offset or viewport size moved the window, so
// scrolling supplements the newly revealed messages (styling them on
// demand) while stable ones reuse the render cache. Called by syncViewport
// from Update, never by the render pass.
func (m *Model) feedViewport(h int) {
	m.chatViewport.SetHeight(h)
	offset := m.chatViewport.YOffset()
	if m.viewportDirty || offset != m.fedOffset || h != m.fedHeight {
		m.chatViewport.SetContent(m.renderVirtualDocAt(h, offset))
		if m.needAutoScroll {
			m.chatViewport.GotoBottom()
		}
		m.fedOffset = m.chatViewport.YOffset()
		m.fedHeight = h
		// Auto-scroll moved the offset: rebase the styled window to it.
		if m.fedOffset != offset {
			m.chatViewport.SetContent(m.renderVirtualDocAt(h, m.fedOffset))
		}
		m.viewportDirty = false
	}
}

// renderVirtualDoc builds the viewport document for the current scroll
// position: real styled rows inside the visible window [offset,
// offset+height), placeholder rows elsewhere. The row count always equals
// the estimated total, so bubbles' scroll clamping and the scrollbar stay
// consistent.
func (m *Model) renderVirtualDoc(height int) string {
	return m.renderVirtualDocAt(height, m.chatViewport.YOffset())
}

// renderVirtualDocAt is renderVirtualDoc for an explicit window offset —
// search jumps must feed a document whose styled window is the destination
// before SetYOffset clamps there.
//
// The document is [pad rows][message rows]; the viewport window lives in
// document coordinates, so the message windowing runs shifted by the pad:
// message row k is visible iff pad+k ∈ [offset, offset+height).
func (m *Model) renderVirtualDocAt(height, offset int) string {
	vpW := layout.GetTranscriptWidth(m.width)
	if len(m.messages) == 0 {
		return ""
	}
	heights := m.virtualLineHeights(vpW)
	offset = max(0, offset)
	windowH := max(1, height)
	pad := layout.TranscriptTopPad
	msgWinStart := offset - pad // message-coordinate window start (may be negative)
	msgWinEnd := msgWinStart + windowH
	first, startWithin := messageAtLine(heights, max(0, msgWinStart))
	last, _ := messageAtLine(heights, max(0, msgWinEnd-1))

	// Settle pass: cache misses stand in as estimates, so style the window
	// band (± one screen of context) now — the exact heights replace the
	// estimates and the window re-locates once before any row is cut, so a
	// visible block is never clipped by its own estimate.
	const band = 1 // in screens
	lo := first
	for rows, i := 0, first; i >= 0 && rows < band*windowH; i-- {
		rows += heights[i]
		lo = i
	}
	hi := last
	for rows, i := 0, last; i < len(heights) && rows < band*windowH; i++ {
		rows += heights[i]
		hi = i
	}
	for i := lo; i <= hi; i++ {
		m.renderMessageBlock(i, m.messages[i], vpW)
	}
	heights = m.virtualLineHeights(vpW)
	first, startWithin = messageAtLine(heights, max(0, msgWinStart))
	last, _ = messageAtLine(heights, max(0, msgWinEnd-1))

	var b strings.Builder
	// Off-window bulk is the dominant part of the document at large offsets
	// (every row outside the styled window). Those rows are never drawn —
	// the window rows are real by construction — so the bulk is emitted as
	// bare empty lines: the row count (and with it bubbles' scroll clamping
	// and the scrollbar) stays exact while SetContent avoids handing the
	// renderer tens of thousands of styled rows to width-measure per feed.
	// Only the visible slots keep the real gutter row.
	emitPad := func(rows int) {
		for r := 0; r < rows; r++ {
			b.WriteString(padRow(vpW))
			b.WriteByte('\n')
		}
	}
	emitPlaceholder := func(rows int) {
		if rows > 0 {
			b.WriteString(strings.Repeat("\n", rows))
		}
	}

	// Top pad rows — always emitted so the doc's row count and the scroll
	// mapping include them; visible only when the window reaches row 0.
	emitPad(pad)

	// Message rows. `cursor` is the message-coordinate row of message
	// `first`'s row 0; negative when the window starts inside the pad.
	cursor := msgWinStart - startWithin
	// Rows of messages before `first` (all above the window): placeholders.
	above := 0
	for i := 0; i < first; i++ {
		above += heights[i]
	}
	emitPlaceholder(above)

	// Window messages: real block rows where visible, placeholders where
	// the window clips the message's estimated range or styling is gated.
	for i := first; i <= last; i++ {
		block, skip := m.renderMessageBlock(i, m.messages[i], vpW)
		var lines []string
		if !skip && block != "" {
			lines = strings.Split(block, "\n")
		}
		for r := 0; r < heights[i]; r++ {
			abs := cursor + r
			if abs < msgWinStart || abs >= msgWinEnd || skip || r >= len(lines) {
				b.WriteString(placeholderRow(vpW))
			} else {
				b.WriteString(fitRow(lines[r], vpW))
			}
			b.WriteByte('\n')
		}
		cursor += heights[i]
	}

	// Rows below the window.
	below := 0
	for i := last + 1; i < len(heights); i++ {
		below += heights[i]
	}
	emitPlaceholder(below)

	// Transient retry divider: turn-scoped operational state from the
	// model_retrying session update — appended after the last message row,
	// never part of the message store, gone once the model produces again.
	if m.retry != nil {
		b.WriteString("\n")
		b.WriteString(fitRow(m.retryRow(vpW), vpW))
	}

	// TrimSuffix, not TrimRight: the below-window bulk is bare newlines and
	// must survive as empty lines (TrimRight would collapse the row count).
	return strings.TrimSuffix(b.String(), "\n")
}

// retryRow renders the transient backoff divider shown while the kernel
// waits between model attempts: attempt progress, the provider error, and
// a next-attempt countdown (fed once a second by the spinner tick). The
// same centered-rule language as the compaction divider, in warning color.
func (m *Model) retryRow(vpW int) string {
	r := m.retry
	remaining := r.delay - time.Since(r.startedAt)
	if remaining < 0 {
		remaining = 0
	}
	label := fmt.Sprintf("Retrying %d/%d", r.attempt, r.max)
	if r.err != "" {
		err := utils.TruncateByWidth(r.err, 48)
		label += " · " + err
	}
	label += fmt.Sprintf(" · next in %ds", int(remaining.Seconds())+1)
	return centerRule(label, vpW, theme.Warning)
}
