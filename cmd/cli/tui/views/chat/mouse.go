package chat

import (
	tea "charm.land/bubbletea/v2"

	"github.com/yusheng-g/openagent-go/cmd/cli/tui/layout"
)

// ── mouse interaction ──
//
// CellMotion tracking (see createView) delivers clicks, wheel, drags and
// motion to the app. This file owns the routing: clicks reach the scrollbar
// drag and the transcript selection; motion extends whichever drag is
// active; release ends it.

// scrollbarX returns the terminal column of the scrollbar track. The left
// area paints with 1-column page padding, so inside it the viewport spans
// columns 1..leftW-4, one gap column follows, and the bar sits at leftW-2
// (renderLeft joins viewport + gap + scrollbar in that order).
func (m *Model) scrollbarX() int {
	return layout.GetLeftWidth(m.width) - 2
}

// scrollbarMetrics mirrors renderScrollbar's thumb math so hit-testing and
// drag mapping stay consistent with what is drawn. ok=false when the
// document fits the viewport (no thumb, nothing to drag).
func (m *Model) scrollbarMetrics() (thumbH, maxOffset int, ok bool) {
	total := m.chatViewport.TotalLineCount()
	h := m.chatViewport.Height()
	if h <= 0 || total <= h {
		return 0, 0, false
	}
	return max(1, h*h/total), total - h, true
}

// handleMouseClick routes a left press. The scrollbar claim comes first:
// the bar column sits right beside the transcript, so without the check a
// press there would fall through to selection. Next a press on a thought/
// tool block's header row flips that block's expansion and is consumed —
// header clicks never anchor a selection. Every other press starts the
// box selection as before.
func (m *Model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if msg.Button == tea.MouseLeft && m.inChat && !m.panelOpen && m.permissionReq == nil {
		if m.startScrollbarDrag(msg.X, msg.Y) {
			return m, nil
		}
		if msg.Y < m.chatViewport.Height() {
			if m.toggleAtClick(msg.X, msg.Y) {
				return m, nil
			}
			m.startSelection(msg.X, msg.Y)
		}
	}
	return m, nil
}

// toggleAtClick expands or collapses the thought/tool block whose header
// row the press landed on. The hit resolves through the same mapping the
// virtual window renders with (selCellAt row → messageAtLine), so it is
// exact for anything on screen — the settle pass guarantees visible rows
// carry exact heights. A block's header is its row 0: block chrome (turn
// markers) trails the block and never precedes it, and gated-out blocks
// occupy no rows, so a click can only land on a drawn header. Returns
// false when the press misses one, leaving the click to the selection.
func (m *Model) toggleAtClick(x, y int) bool {
	if y < 0 || y >= m.chatViewport.Height() {
		return false
	}
	row := m.selCellAt(x, y).row - layout.TranscriptTopPad
	heights := m.virtualLineHeights(layout.GetTranscriptWidth(m.width))
	idx, within := messageAtLine(heights, row)
	if idx < 0 || within != 0 {
		return false
	}
	msg := m.messages[idx]
	if msg.Seq == 0 {
		// The streaming message has no identity yet (its header is not
		// clickable, and a Seq-0 key would collide across messages).
		return false
	}
	var expanded bool
	switch msg.Role {
	case "thought":
		expanded = m.effectiveThoughtExpanded(msg, msg.ThoughtEnd.IsZero() && m.loading)
	case "tool":
		expanded = m.effectiveToolDetail(msg)
	default:
		return false
	}
	if m.expandOverride == nil {
		m.expandOverride = make(map[int64]int8)
	}
	if expanded {
		m.expandOverride[msg.Seq] = -1
	} else {
		m.expandOverride[msg.Seq] = 1
	}
	m.viewportDirty = true
	return true
}

// startScrollbarDrag begins a scrollbar drag from a left press at the bar
// column. A press on the track first jumps the thumb so it centers under
// the cursor (standard scrollbar behavior), then drags from there; a press
// on the thumb grabs it in place. Returns false when the press misses the
// bar or there is nothing to scroll.
func (m *Model) startScrollbarDrag(x, y int) bool {
	if x != m.scrollbarX() || y < 0 || y >= m.chatViewport.Height() {
		return false
	}
	thumbH, maxOffset, ok := m.scrollbarMetrics()
	if !ok {
		return false
	}
	span := m.chatViewport.Height() - thumbH
	thumbY := m.chatViewport.YOffset() * span / maxOffset
	if y >= thumbY && y < thumbY+thumbH {
		m.sbarGrab = y - thumbY
	} else {
		// Track press: jump so the cursor sits mid-thumb, then drag.
		want := min(max(y-thumbH/2, 0), span)
		m.chatViewport.SetYOffset(want * maxOffset / span)
		m.sbarGrab = y - want
		m.needAutoScroll = m.isNearBottom()
	}
	m.sbarDrag = true
	return true
}

// handleMouseMotion extends the active drag. CellMotion reports motion only
// while a button is held, and the held button arrives in the message, so a
// left motion outside the bar column still scrolls — scrollbars keep
// dragging when the cursor strays sideways.
func (m *Model) handleMouseMotion(msg tea.MouseMotionMsg) (tea.Model, tea.Cmd) {
	if m.sbarDrag && msg.Button == tea.MouseLeft {
		m.dragScrollbarTo(msg.Y)
		return m, nil
	}
	if m.selection.active && msg.Button == tea.MouseLeft {
		m.extendSelection(msg.X, msg.Y)
	}
	return m, nil
}

// dragScrollbarTo maps the cursor row back to a scroll offset — the exact
// inverse of the renderScrollbar thumb mapping — and applies it via
// SetYOffset; feedViewport rebuilds the styled window around the new offset
// at the end of the Update. Dragging is user scroll intent: auto-scroll
// stays off unless the drag parks the window back at the bottom (same rule
// as wheel-down).
func (m *Model) dragScrollbarTo(y int) {
	thumbH, maxOffset, ok := m.scrollbarMetrics()
	if !ok {
		return
	}
	span := m.chatViewport.Height() - thumbH
	thumbY := min(max(y-m.sbarGrab, 0), span)
	off := thumbY * maxOffset / span
	if off != m.chatViewport.YOffset() {
		m.chatViewport.SetYOffset(off)
		m.needAutoScroll = m.isNearBottom()
	}
}

// handleMouseRelease ends the active drag. A released box selection copies
// its text via OSC 52.
func (m *Model) handleMouseRelease(msg tea.MouseReleaseMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseLeft {
		return m, nil
	}
	if m.sbarDrag {
		m.sbarDrag = false
	}
	if m.selection.active {
		return m.finishSelection()
	}
	return m, nil
}
