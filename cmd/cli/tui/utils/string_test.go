package utils

import (
	"strings"
	"testing"
)

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"Hello", 5},
		{"你好", 4},
		{"Hello你好", 9},
		{"a\x1b[31mb\x1b[0m", 2}, // ANSI stripped
		{"✓✗○●▶", 8},             // ✓✗ wide 1, ○●▶ wide 2
	}
	for _, c := range cases {
		if got := DisplayWidth(c.in); got != c.want {
			t.Errorf("DisplayWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestTruncateByWidth(t *testing.T) {
	cases := []struct {
		in       string
		maxWidth int
		want     string
	}{
		{"short", 10, "short"},
		// NB: the ellipsis "…" is 2 columns wide, so maxWidth 5 leaves 3.
		{"你好世界", 5, "你…"},                // 2 + 2(ellipsis) = 4 ≤ 5
		{"abcdefgh", 5, "abc…"},          // 3 + 2(ellipsis) = 5
		{"\x1b[31mabc\x1b[0m", 5, "abc"}, // styling stripped, no ellipsis needed
	}
	for _, c := range cases {
		if got := TruncateByWidth(c.in, c.maxWidth); got != c.want {
			t.Errorf("TruncateByWidth(%q, %d) = %q, want %q", c.in, c.maxWidth, got, c.want)
		}
	}
}

func TestTruncateStyled(t *testing.T) {
	styled := "\x1b[38;2;0;122;255;48;2;45;45;48m┃\x1b[48;2;45;45;48m\x1b[48;2;45;45;48m 内容\x1b[m"
	for _, mw := range []int{1, 3, 7} {
		out := TruncateStyled(styled, mw)
		if dw := DisplayWidth(out); dw > mw {
			t.Errorf("TruncateStyled(…, %d) width = %d, want ≤ %d", mw, dw, mw)
		}
		// A reset must close the row so no style leaks onto the next line.
		if !strings.HasSuffix(out, "\x1b[m") {
			t.Errorf("TruncateStyled(…, %d) must end in a reset, got %q", mw, out)
		}
	}
	// The rail color of a surviving cell must be preserved, not stripped.
	if out := TruncateStyled(styled, 7); !strings.Contains(out, "\x1b[38;2;0;122;255") {
		t.Errorf("TruncateStyled dropped the rail color: %q", out)
	}
	// A row that already fits comes back untouched.
	if got := TruncateStyled(styled, 99); got != styled {
		t.Errorf("TruncateStyled should pass through fitting rows")
	}
}

func TestStripANSI(t *testing.T) {
	in := "\x1b[38;2;255;255;255m█\x1b[0m"
	if got := StripANSI(in); got != "█" {
		t.Errorf("StripANSI(%q) = %q, want %q", in, got, "█")
	}
}

func TestUnifiedEndOfLine(t *testing.T) {
	in := "line1\r\nline2\rline3"
	if got := UnifiedEndOfLine(in); got != "line1\nline2\nline3" {
		t.Errorf("UnifiedEndOfLine(%q) = %q", in, got)
	}
}

func TestOverlayBackground(t *testing.T) {
	bg := "48;2;38;70;109"
	// Plain ASCII: exactly the range gets the background, the rest untouched.
	got := OverlayBackground("hello world", 6, 11, bg)
	if want := "hello \x1b[0m\x1b[" + bg + "mworld\x1b[0m"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Zero-width range returns the input unchanged.
	if got := OverlayBackground("abc", 2, 2, bg); got != "abc" {
		t.Fatalf("empty range changed the input: %q", got)
	}
	// Existing foreground styling survives inside the range and is restored
	// after it. The input's own reset between spans sits inside the range,
	// so it comes back with the selection background re-applied (the exit
	// then restores the plain style at "rest").
	styled := "a\x1b[31mred\x1b[0mrest"
	got = OverlayBackground(styled, 1, 4, bg)
	want := "a\x1b[31m\x1b[0m\x1b[31;" + bg + "mred\x1b[0m\x1b[" + bg + "m\x1b[0mrest"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Transcript-shaped line: a styled span, a mid-line reset, more text —
	// the selection background must survive the reset instead of letting
	// the first span boundary wipe the highlight for the rest of the line.
	row := "\x1b[38;2;100;98;98;48;2;0;0;0m> pptx_read\x1b[m\x1b[38;2;100;98;98;48;2;0;0;0m [README.md]\x1b[m"
	got = OverlayBackground(row, 0, 40, bg)
	if !strings.Contains(got, "48;2;38;70;109m [README.md]") {
		t.Fatalf("text after a mid-line reset must render selected: %q", got)
	}
	if strings.Count(got, bg) < 3 {
		t.Fatalf("selection background lost across the line: %q", got)
	}
	// A combined fg+bg sequence (lipgloss emits one SGR with both) keeps its
	// foreground whole when the background is swapped: the "48;2;R;G;B"
	// tail must go as a unit, leaving no orphaned "2;R;G;B" subparams.
	combined := "\x1b[38;2;253;252;252;48;2;0;0;0mtext"
	got = OverlayBackground(combined, 0, 4, bg)
	want = "\x1b[38;2;253;252;252;48;2;0;0;0m\x1b[0m\x1b[38;2;253;252;252;" + bg + "mtext\x1b[0m"
	if got != want {
		t.Fatalf("combined fg+bg: got %q, want %q", got, want)
	}
	// The style must survive re-emission intact after the range ends too.
	got = OverlayBackground(combined+"\x1b[0mpad", 1, 2, bg)
	if strings.Contains(got, "2;0;0;0;") {
		t.Fatalf("orphaned truecolor subparams leaked into %q", got)
	}
	// A wide (CJK) rune joins the selection when its first cell is inside.
	got = OverlayBackground("中文", 0, 1, bg) // 中 occupies cells 0-1
	if !strings.Contains(got, bg) {
		t.Fatalf("wide rune not painted when the range covers its first cell: %q", got)
	}
	got = OverlayBackground("中文", 1, 2, bg) // boundary inside 中
	if strings.Contains(got, bg) {
		t.Fatalf("wide rune painted on a mid-rune boundary: %q", got)
	}
}

func TestPlainCells(t *testing.T) {
	// ANSI styling is stripped; the cell range applies to visible columns.
	got := PlainCells("\x1b[31mabc\x1b[0mdef", 2, 5)
	if got != "cde" {
		t.Fatalf("got %q, want %q", got, "cde")
	}
	// Wide runes come out whole.
	if got := PlainCells("中文", 0, 1); got != "中" {
		t.Fatalf("got %q, want 中", got)
	}
}

func TestSanitizeControl(t *testing.T) {
	// ESC (and therefore every ANSI/OSC sequence), BEL, NUL and DEL are
	// dropped; \n and \t survive and \r\n is normalized.
	in := "ok\x1b]52;c;Zm9v\x07\x1b[31mred\x1b[0m\r\nline2\tend\x00\x7f"
	want := "ok]52;c;Zm9v[31mred[0m\nline2\tend"
	if got := SanitizeControl(in); got != want {
		t.Fatalf("SanitizeControl = %q, want %q", got, want)
	}
	// Clean input is returned unchanged.
	if got := SanitizeControl("plain text 你好"); got != "plain text 你好" {
		t.Fatalf("SanitizeControl changed clean input: %q", got)
	}
	// C1 controls (U+0080–U+009F) are dropped too.
	if got := SanitizeControl("a\u009bb"); got != "ab" {
		t.Fatalf("C1 control survived: %q", got)
	}
}
