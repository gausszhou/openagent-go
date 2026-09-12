// Package utils holds string helpers shared by the TUI views and
// components. All width-based operations use display width (CJK runes
// count as 2) via go-runewidth so that Chinese text never breaks the
// layout.
package utils

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// ansiRe matches CSI escape sequences (colors, cursor moves, etc.).
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// StripANSI removes ANSI escape sequences from s. Used before measuring
// or truncating text that contains styled tool output.
func StripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// DisplayWidth returns the visible width of s: ANSI sequences are
// stripped first, then CJK/wide runes count as 2 columns.
func DisplayWidth(s string) int {
	return runewidth.StringWidth(StripANSI(s))
}

// TruncateByWidth cuts s to at most maxWidth display columns, appending
// the ellipsis "…". Layout-affecting ANSI styling is stripped so the
// truncated string stays well-formed.
func TruncateByWidth(s string, maxWidth int) string {
	return runewidth.Truncate(StripANSI(s), maxWidth, "…")
}

// TruncateStyled cuts s to at most maxWidth display columns while keeping
// the ANSI styling of the surviving cells intact. Unlike TruncateByWidth,
// this is used on rows that are only slightly over-width (e.g. a
// box-drawing glyph measured as double width by runewidth), where throwing
// away the row's colors would visibly degrade the transcript. The result
// always ends in a reset so no open style leaks onto the following line.
func TruncateStyled(s string, maxWidth int) string {
	if DisplayWidth(s) <= maxWidth {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	width := 0
	rest := s
	for rest != "" {
		loc := ansiRe.FindStringIndex(rest)
		plain := rest
		if loc != nil {
			plain = rest[:loc[0]]
		}
		if plain != "" {
			space := maxWidth - width
			if space <= 0 {
				b.WriteString("\x1b[m")
				return b.String()
			}
			keep := runewidth.Truncate(plain, space, "")
			b.WriteString(keep)
			width += runewidth.StringWidth(keep)
			if runewidth.StringWidth(plain) > space {
				b.WriteString("\x1b[m")
				return b.String()
			}
		}
		if loc == nil {
			return b.String()
		}
		b.WriteString(rest[loc[0]:loc[1]])
		rest = rest[loc[1]:]
	}
	return b.String()
}

// UnifiedEndOfLine normalizes \r\n and legacy \r line endings to \n.
// Markdown rendering and tool output both assume Unix line endings.
func UnifiedEndOfLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

// isControl reports whether r is a C0 control (except the layout whitespace
// the UI keeps), DEL, or a C1 control. ESC (\x1b) is a C0 control, so the
// whole ANSI/OSC escape family is covered.
func isControl(r rune) bool {
	switch r {
	case '\n', '\t':
		return false
	}
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// SanitizeControl removes terminal control characters from untrusted text
// (agent replies, tool output, session metadata) before it is rendered.
// Without this, a malicious tool result or fetched page can smuggle ANSI/OSC
// sequences into the terminal — e.g. OSC 52 clipboard writes, OSC 8 link
// spoofing, title changes or cursor moves. Line endings are normalized first,
// then every control rune except \n and \t is dropped. The UI's own styling
// is added after this point, so no legitimate SGR is lost.
func SanitizeControl(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	s = UnifiedEndOfLine(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sgrStateRe matches SGR sequences (the "m" flavor of CSI): plain resets
// and parameterized styles.
var sgrStateRe = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// updateSGRState folds an SGR sequence into the accumulated parameter
// string active since the last reset. A reset clears it; any other style
// appends its parameters (the transcript's lipgloss output never emits
// partial cancels like "39", so concatenation mirrors what the terminal
// shows closely enough for cell repainting).
func updateSGRState(active, seq string) string {
	m := sgrStateRe.FindStringSubmatch(seq)
	if m == nil {
		return active
	}
	if m[1] == "" || m[1] == "0" {
		return ""
	}
	if active == "" {
		return m[1]
	}
	return active + ";" + m[1]
}

// sgrUnits splits an SGR parameter list into atomic units: single-parameter
// attributes plus the multi-parameter color forms consumed whole —
// "38;2;R;G;B" / "48;2;R;G;B" (truecolor) and "38;5;N" / "48;5;N" (256).
// Filtering must happen per unit: dropping a bare "48" from
// "38;2;…;48;2;R;G;B" would orphan the "2;R;G;B" tail and corrupt the
// re-emitted sequence (the terminal then prints the stray terminator).
func sgrUnits(params string) []string {
	parts := strings.Split(params, ";")
	var units []string
	for i := 0; i < len(parts); i++ {
		if parts[i] == "38" || parts[i] == "48" {
			extra := 0
			if i+1 < len(parts) && parts[i+1] == "2" {
				extra = 4
			} else if i+1 < len(parts) && parts[i+1] == "5" {
				extra = 2
			}
			if extra > 0 {
				end := min(i+extra, len(parts)-1)
				units = append(units, strings.Join(parts[i:end+1], ";"))
				i = end
				continue
			}
		}
		units = append(units, parts[i])
	}
	return units
}

// overlaySGR re-emits the accumulated style without any background unit
// (48…/49), optionally replacing it with overlayCode.
func overlaySGR(active string, withBg bool, overlayCode string) string {
	var keep []string
	if active != "" {
		for _, u := range sgrUnits(active) {
			if u == "49" || u == "48" || strings.HasPrefix(u, "48;") {
				continue
			}
			keep = append(keep, u)
		}
	}
	if withBg {
		keep = append(keep, overlayCode)
	}
	if len(keep) == 0 {
		return "\x1b[0m"
	}
	return "\x1b[0m\x1b[" + strings.Join(keep, ";") + "m"
}

// OverlayBackground repaints the display cells [from, to) of s with the
// given SGR background code (e.g. "48;2;38;70;109"), keeping every cell's
// existing foreground. ANSI sequences carry no cells; a wide (CJK) rune is
// part of the range when its first cell is. Cells past the end of the
// visible text are not conjured up — callers pass already padded rows.
// Used by the transcript's box-selection highlight.
func OverlayBackground(s string, from, to int, bgCode string) string {
	if from >= to {
		return s
	}
	// Accept both the bare parameter list ("48;2;38;70;109") and a full
	// sequence ("\x1b[48;2;38;70;109m"): callers pass theme.ColorBgCode's
	// output, and a full sequence appended to a rebuilt SGR would abort it
	// mid-list (the wrapper's final "m" would print as a literal glyph).
	bgCode = strings.TrimSuffix(strings.TrimPrefix(bgCode, "\x1b["), "m")
	var b strings.Builder
	b.Grow(len(s) + 32)
	cell := 0
	inSel := false
	active := ""
	rest := s
	for rest != "" {
		if rest[0] == '\x1b' {
			loc := ansiRe.FindStringIndex(rest)
			if loc == nil || loc[0] != 0 {
				// Unknown escape byte (OSC, incomplete CSI): pass it
				// through and keep walking — the next bytes re-enter the
				// normal rune/sequence handling.
				b.WriteByte(rest[0])
				rest = rest[1:]
				continue
			}
			seq := rest[:loc[1]]
			rest = rest[loc[1]:]
			if !sgrStateRe.MatchString(seq) {
				// Non-style CSI: no cell styling, pass through.
				b.WriteString(seq)
				continue
			}
			active = updateSGRState(active, seq)
			if inSel {
				// Transcript rows carry their own resets between styled
				// spans; inside the selection each of those resets must
				// come back WITH the selection background, or the first
				// span boundary wipes the highlight for the rest of the
				// line. Re-emit the folded style instead of the raw
				// sequence.
				b.WriteString(overlaySGR(active, true, bgCode))
			} else {
				b.WriteString(seq)
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(rest)
		w := runewidth.RuneWidth(r)
		want := cell >= from && cell < to
		if want != inSel {
			b.WriteString(overlaySGR(active, want, bgCode))
			inSel = want
		}
		b.WriteRune(r)
		cell += w
		rest = rest[size:]
	}
	if inSel {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// PlainCells returns the plain (ANSI-stripped) text of display cells
// [from, to) of s, collecting whole runes (a wide rune joins the output
// when its first cell is in range). Used to copy a box-selected span.
func PlainCells(s string, from, to int) string {
	var b strings.Builder
	cell := 0
	rest := s
	for rest != "" {
		if rest[0] == '\x1b' {
			loc := ansiRe.FindStringIndex(rest)
			if loc == nil || loc[0] != 0 {
				rest = rest[1:]
				continue
			}
			rest = rest[loc[1]:]
			continue
		}
		r, size := utf8.DecodeRuneInString(rest)
		w := runewidth.RuneWidth(r)
		if cell >= from && cell < to {
			b.WriteRune(r)
		}
		cell += w
		rest = rest[size:]
		if cell >= to {
			break
		}
	}
	return b.String()
}
