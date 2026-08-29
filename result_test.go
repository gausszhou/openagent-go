package openagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeWindowModel is a Model with a fixed, small context window so the
// result policy threshold (cw * artifactFraction / 100) can be made
// trivially small in tests without writing megabytes of "result".
type fakeWindowModel struct{ cw int }

func (m *fakeWindowModel) ChatCompletion(context.Context, ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return nil, nil
}
func (m *fakeWindowModel) ChatCompletionStream(context.Context, ChatCompletionRequest) (StreamReader, error) {
	return nil, nil
}
func (m *fakeWindowModel) ContextWindow() int { return m.cw }

// TestDefaultResultPolicy_UsesSessionModelContextWindow asserts the built-in
// result policy reads the context window from session.Model, truncating
// oversized tool output by saving it to disk and replacing Content with a
// pointer.
func TestDefaultResultPolicy_UsesSessionModelContextWindow(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	root := ArtifactRoot()
	if !strings.HasPrefix(root, scratch) {
		t.Fatalf("ArtifactRoot=%q not under TMPDIR scratch %q", root, scratch)
	}

	// Window = 10_000 tokens → threshold = 10_000 * 5 / 100 = 500 tokens.
	// 5000 ASCII bytes ≈ 625 tokens > 500 → saved; 1000 bytes ≈ 250
	// tokens < 500 → untouched.
	const big = 5000
	const small = 1000

	sess := Session{
		ID:    "s-test",
		Model: &fakeWindowModel{cw: 10_000},
	}
	ctx := context.Background()

	// Big result → truncated, saved to disk, FileRef set.
	bigRes := strings.Repeat("x", big)
	policy := &DefaultResultPolicy{}
	res := policy.Apply(ctx, sess, &ToolResult{Content: bigRes})
	if !res.Truncated {
		t.Fatal("big result not flagged Truncated")
	}
	if res.FileRef == "" {
		t.Fatal("big result missing FileRef")
	}
	if strings.Contains(res.Content, strings.Repeat("x", 100)) {
		t.Fatal("Content still carries the raw big output")
	}
	// Layout: <ArtifactRoot()>/sess-<sessionID>/artifact-<8hex>.txt
	sessDir := filepath.Join(root, "sess-"+sess.ID)
	matches, err := filepath.Glob(filepath.Join(sessDir, "artifact-*.txt"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 saved artifact, got %d (%v)", len(matches), matches)
	}
	if name := filepath.Base(matches[0]); len(name) <= len("artifact-.txt") {
		t.Fatalf("artifact filename %q has no random suffix", name)
	}
	got, _ := os.ReadFile(matches[0])
	if string(got) != bigRes {
		t.Fatalf("artifact content mismatch: got len=%d want len=%d", len(got), len(bigRes))
	}

	// Small result → NOT truncated, unchanged.
	smallRes := strings.Repeat("y", small)
	res2 := policy.Apply(ctx, sess, &ToolResult{Content: smallRes})
	if res2.Truncated || res2.FileRef != "" || res2.Content != smallRes {
		t.Fatalf("small result was truncated: %+v", res2)
	}
	matches2, _ := filepath.Glob(filepath.Join(sessDir, "*.txt"))
	if len(matches2) != 1 {
		t.Fatalf("small result should not have been saved; file count = %d", len(matches2))
	}
}

// TestDefaultResultPolicy_NilAndErrorResultsPassthrough asserts error results
// and nil results are never truncated.
func TestDefaultResultPolicy_NilAndErrorResultsPassthrough(t *testing.T) {
	policy := &DefaultResultPolicy{}
	sess := Session{ID: "s-nil", Model: &fakeWindowModel{cw: 100}}

	if got := policy.Apply(context.Background(), sess, nil); got != nil {
		t.Fatalf("nil result became non-nil: %+v", got)
	}

	errRes := &ToolResult{Error: &ToolError{Message: "boom"}}
	got := policy.Apply(context.Background(), sess, errRes)
	if got.Truncated || got.FileRef != "" {
		t.Fatalf("error result was truncated: %+v", got)
	}
}

// TestDefaultResultPolicy_WrapsOverlongSingleLine: a huge single-line
// result (minified JSON / base64 / newline-less logs) must be written to
// disk with line breaks every maxArtifactLine runes — read/grep cap a
// single line at 1MB, so an unwrapped megabyte line would make the
// artifact unreadable ("bufio.Scanner: token too long").
//
// maxArtifactLine is shrunk for the test: tokenizer.Count is the slow
// part of Apply (linear, ~0.4ms/byte), so the input stays small while
// still exceeding both the (shrunk) wrap threshold and the token
// threshold.
func TestDefaultResultPolicy_WrapsOverlongSingleLine(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	_ = ArtifactRoot()

	old := maxArtifactLine
	maxArtifactLine = 1024
	t.Cleanup(func() { maxArtifactLine = old })

	// ~6KB single line, no newlines: past the 500-token threshold
	// (window 10k × 5%) and past the shrunk wrap threshold.
	big := strings.Repeat("z", maxArtifactLine*6)
	sess := Session{ID: "s-wrap", Model: &fakeWindowModel{cw: 10_000}}
	policy := &DefaultResultPolicy{}
	res := policy.Apply(context.Background(), sess, &ToolResult{Content: big})
	if !res.Truncated || res.FileRef == "" {
		t.Fatalf("expected truncation: %+v", res)
	}

	got, err := os.ReadFile(res.FileRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < len(big) {
		t.Fatalf("wrapped artifact smaller than original: %d < %d", len(got), len(big))
	}
	// Every line must stay within the wrap cap (the production cap of
	// 32K runes stays inside the read/grep 1MB single-line limit).
	// Wrapped lines carry the wrapMarker suffix — the CONTENT part stays
	// within the cap.
	breaks := 0
	for _, ln := range strings.Split(string(got), "\n") {
		content := strings.TrimSuffix(ln, wrapMarker)
		if n := len([]rune(content)); n > maxArtifactLine {
			t.Fatalf("line content of %d runes exceeds maxArtifactLine %d", n, maxArtifactLine)
		}
		if strings.HasSuffix(ln, wrapMarker) {
			breaks++
		}
	}
	if breaks == 0 {
		t.Fatalf("no wrap markers found in artifact (want %d breaks)", len(big)/maxArtifactLine)
	}
	if res.Metadata["artifact_bytes"] != len(big) {
		t.Fatalf("artifact_bytes = %v, want original size %d", res.Metadata["artifact_bytes"], len(big))
	}

	// Short single lines (no newlines) pass through unwrapped.
	small := strings.Repeat("q", 1000)
	res2 := policy.Apply(context.Background(), sess, &ToolResult{Content: small})
	if res2.Truncated {
		t.Fatalf("small result truncated: %+v", res2)
	}
}

// wrapLongLines must treat '\r' as a line terminator too: '\r'-only
// endings and Windows '\r\n' must never be counted as content (which
// would falsely wrap short "\r"-separated lines and mislead the model
// with a continuation marker).
func TestWrapLongLinesTreatsCRAsLineEnding(t *testing.T) {
	// CR-only separated content: each "line" is 1 rune — far below the
	// (shrunk) cap — so no artificial breaks may appear.
	s := strings.Repeat("a\r", 3000)
	got := wrapLongLines(s, 1024)
	if strings.Contains(got, wrapMarker) {
		t.Fatalf("CR-separated content was falsely wrapped: %d markers", strings.Count(got, wrapMarker))
	}

	// Windows CRLF: same, no breaks.
	got = wrapLongLines(strings.Repeat("b\r\n", 3000), 1024)
	if strings.Contains(got, wrapMarker) {
		t.Fatalf("CRLF content was falsely wrapped: %d markers", strings.Count(got, wrapMarker))
	}

	// A genuine long line WITH trailing CRLF still wraps at the cap.
	long := strings.Repeat("c", 4000) + "\r\n"
	got = wrapLongLines(long, 1024)
	if !strings.Contains(got, wrapMarker) {
		t.Fatal("long CRLF-terminated line was not wrapped")
	}
}

// TestDefaultResultPolicy_ArtifactReadInPlaceTruncated verifies the guard
// that prevents the artifact-of-artifact cascade: when "read" targets a
// path under ArtifactRoot() and the result exceeds the threshold, Apply
// truncates IN PLACE (Content → bounded preview, FileRef → the SAME
// artifact file) instead of spilling to a new artifact file. "grep" is
// not guarded — it goes through normal spill truncation.
func TestDefaultResultPolicy_ArtifactReadInPlaceTruncated(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	root := ArtifactRoot()

	// Window = 10_000 → threshold = 500 tokens. 5000 bytes ≈ 625 tokens
	// > 500 → would be truncated without the guard.
	const big = 5000
	bigRes := strings.Repeat("x\n", big/2) // newlines so truncatePreview can snap to line boundary
	sess := Session{ID: "s-guard", Model: &fakeWindowModel{cw: 10_000}}
	policy := &DefaultResultPolicy{}
	ctx := context.Background()

	// First Apply: no tool metadata → truncates and creates artifact A
	// (normal spill path).
	res1 := policy.Apply(ctx, sess, &ToolResult{Content: bigRes})
	if !res1.Truncated || res1.FileRef == "" {
		t.Fatalf("baseline: big result should truncate, got %+v", res1)
	}
	artifactPath := res1.FileRef
	if !strings.HasPrefix(artifactPath, root) {
		t.Fatalf("artifact path %q not under ArtifactRoot %q", artifactPath, root)
	}
	artifactsBefore := countArtifacts(t, root)

	// Helper: build metadata as the runtime call site would.
	toolMeta := func(tool, path string) map[string]any {
		args, _ := json.Marshal(map[string]string{"path": path})
		return map[string]any{
			"tool_name": tool,
			"tool_args": json.RawMessage(args),
		}
	}

	// read on the artifact path → in-place truncation: Truncated=true,
	// FileRef points at the SAME artifact file, Content is a prefix, and
	// NO new artifact file is created.
	res2 := policy.Apply(ctx, sess, &ToolResult{Content: bigRes, Metadata: toolMeta("read", artifactPath)})
	if !res2.Truncated {
		t.Fatal("read of artifact should be Truncated (in-place)")
	}
	if res2.FileRef != artifactPath {
		t.Fatalf("FileRef should point at the same artifact file %q, got %q", artifactPath, res2.FileRef)
	}
	if res2.Content == bigRes {
		t.Fatal("Content should be a truncated preview, not the full content")
	}
	// Content is preview + continuation hint; the preview portion (before
	// the "\n..." hint) must be a prefix of the original at a line boundary.
	hintIdx := strings.Index(res2.Content, "\n... [")
	if hintIdx < 0 {
		t.Fatalf("Content missing continuation hint: %q", res2.Content)
	}
	preview := res2.Content[:hintIdx]
	if !strings.HasPrefix(bigRes, preview) {
		t.Fatal("Content preview should be a prefix of the original content")
	}
	if !strings.Contains(res2.Content, "continue") {
		t.Fatal("Content should contain a continuation hint")
	}
	// No new artifact file created.
	if n := countArtifacts(t, root); n != artifactsBefore {
		t.Fatalf("in-place truncation created a new artifact file: before=%d after=%d", artifactsBefore, n)
	}

	// grep on the artifact path → NOT guarded → normal spill truncation
	// (creates a new artifact file). grep is excluded from the guard
	// because its FileRef would point at the grepped file, not the match
	// list, and grep overflow is rare/self-terminating.
	res3 := policy.Apply(ctx, sess, &ToolResult{Content: bigRes, Metadata: toolMeta("grep", artifactPath)})
	if !res3.Truncated {
		t.Fatal("grep of artifact should be truncated (normal spill, not guarded)")
	}
	if res3.FileRef == artifactPath {
		t.Fatal("grep spill should create a NEW artifact file, not reuse the source")
	}

	// grep with empty path (defaults to workspace) → truncates normally.
	emptyArgs, _ := json.Marshal(map[string]string{})
	res4 := policy.Apply(ctx, sess, &ToolResult{Content: bigRes, Metadata: map[string]any{
		"tool_name": "grep",
		"tool_args": json.RawMessage(emptyArgs),
	}})
	if !res4.Truncated {
		t.Fatal("grep with empty path should still truncate")
	}

	// read of a non-artifact path → guard false → normal spill truncation.
	res5 := policy.Apply(ctx, sess, &ToolResult{Content: bigRes, Metadata: toolMeta("read", "/workspace/main.go")})
	if !res5.Truncated {
		t.Fatal("read of non-artifact path should still truncate")
	}
	if res5.FileRef == "/workspace/main.go" {
		t.Fatal("non-artifact read should spill to a new file, not set FileRef to the read path")
	}

	// read with a relative path that resolves under ArtifactRoot → guard
	// catches it via filepath.Abs and truncates in place.
	cwd, _ := os.Getwd()
	relPath, err := filepath.Rel(cwd, artifactPath)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	artifactsBeforeRel := countArtifacts(t, root)
	res6 := policy.Apply(ctx, sess, &ToolResult{Content: bigRes, Metadata: toolMeta("read", relPath)})
	if !res6.Truncated || res6.FileRef != artifactPath {
		t.Fatalf("read via relative artifact path %q: Truncated=%v FileRef=%q, want in-place truncation with FileRef=%q",
			relPath, res6.Truncated, res6.FileRef, artifactPath)
	}
	if n := countArtifacts(t, root); n != artifactsBeforeRel {
		t.Fatalf("relative-path in-place truncation created a new artifact file: before=%d after=%d", artifactsBeforeRel, n)
	}

	// No metadata at all → guard false → normal spill truncation.
	res7 := policy.Apply(ctx, sess, &ToolResult{Content: bigRes})
	if !res7.Truncated {
		t.Fatal("result with no metadata should still truncate")
	}
}

// countArtifacts counts artifact-*.txt files under root (the test's
// TMPDIR-scoped ArtifactRoot). Used to assert that in-place truncation
// does not create a new artifact file.
func countArtifacts(t *testing.T, root string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, "*", "artifact-*.txt"))
	if err != nil {
		t.Fatalf("glob artifacts: %v", err)
	}
	return len(matches)
}

// TestTruncatePreview_SingleHugeLine verifies the byte-level fallback: a
// single line with no newlines that exceeds the token budget must still
// produce a non-empty preview (lines >= 1), never ("", 0). Returning empty
// would give the model "0 lines shown" and force a re-read of the same
// line → infinite loop.
func TestTruncatePreview_SingleHugeLine(t *testing.T) {
	// A single line with no '\n', large enough to exceed a small budget.
	big := strings.Repeat("x", 10000)
	preview, lines := truncatePreview("gpt-4", big, 100)
	if preview == "" {
		t.Fatal("single huge line: preview must not be empty (would cause infinite loop)")
	}
	if lines < 1 {
		t.Fatalf("single huge line: lines = %d, want >= 1", lines)
	}
	if CountTokens("gpt-4", preview) > 100 {
		t.Fatalf("preview exceeds token budget: %d > 100", CountTokens("gpt-4", preview))
	}
}

// TestTruncatePreview_NormalMultiLine verifies the line-boundary path: for
// multi-line content with newlines, the preview is a prefix cut at a line
// boundary and lines counts the newlines in the preview.
func TestTruncatePreview_NormalMultiLine(t *testing.T) {
	// 100 lines, each ~100 bytes → ~10000 bytes total. Budget 500 tokens
	// (~2000 bytes) → should keep a prefix at a line boundary.
	var b strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&b, "line %d %s\n", i, strings.Repeat("x", 90))
	}
	s := b.String()
	preview, lines := truncatePreview("gpt-4", s, 500)
	if preview == "" {
		t.Fatal("multi-line preview empty")
	}
	if !strings.HasPrefix(s, preview) {
		t.Fatal("preview must be a prefix of the input")
	}
	// Preview ends at a line boundary (no partial line).
	if len(preview) > 0 && preview[len(preview)-1] != '\n' {
		t.Fatalf("preview not cut at line boundary: ends %q", preview[len(preview)-10:])
	}
	wantLines := strings.Count(preview, "\n")
	if lines != wantLines {
		t.Fatalf("lines = %d, want %d (newline count in preview)", lines, wantLines)
	}
}

// TestDefaultResultPolicy_ArtifactReadPaginationNoOffByOne simulates the
// model paging through a large artifact file by repeated Apply calls, each
// time passing the continuation line from the previous hint as the read
// start line. It asserts that:
//   - every source line is covered (no skips),
//   - no line is duplicated,
//   - the continuation line advances strictly past the data shown.
//
// This is the regression test for the off-by-one where read's "[lines ...]:"
// prefix was counted as file data, making each page skip one real line.
func TestDefaultResultPolicy_ArtifactReadPaginationNoOffByOne(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	root := ArtifactRoot()

	// Build a large multi-line content that exceeds threshold when read
	// in full. Window 10_000 → threshold 500 tokens. ~5000 bytes of
	// numbered lines → ~625 tokens > 500.
	const numLines = 300
	var b strings.Builder
	for i := 1; i <= numLines; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	bigRes := b.String()

	sess := Session{ID: "s-paginate", Model: &fakeWindowModel{cw: 10_000}}
	policy := &DefaultResultPolicy{}
	ctx := context.Background()

	// Spill to create the artifact file A.
	res1 := policy.Apply(ctx, sess, &ToolResult{Content: bigRes})
	if !res1.Truncated || res1.FileRef == "" {
		t.Fatalf("baseline spill failed: %+v", res1)
	}
	artifactPath := res1.FileRef
	if !strings.HasPrefix(artifactPath, root) {
		t.Fatalf("artifact not under ArtifactRoot: %q", artifactPath)
	}

	// Read the artifact file from disk (what the read tool would return).
	artifactContent, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the model paging: start at line 1, each Apply produces a
	// preview + continuation hint; extract the next line from the hint
	// and repeat until the hint says no more content.
	seen := map[string]bool{}
	startLine := 1
	iterations := 0
	const maxIterations = 100 // 300 lines / ~10-20 per page → well under 100

	for iterations < maxIterations {
		iterations++

		// Simulate what the read tool returns for this page: a "[lines ...]:"
		// prefix (read inserts it when line>1 or limit>0) + the file content
		// from startLine onward.
		fileLines := strings.Split(string(artifactContent), "\n")
		if startLine > len(fileLines) {
			break // past end
		}
		// read's prefix line (tool/file.go:134-137).
		var pageContent string
		if startLine > 1 {
			pageContent = fmt.Sprintf("[lines %d-%d, %d total, %d bytes]:\n",
				startLine, len(fileLines), len(fileLines), len(artifactContent))
		}
		pageContent += strings.Join(fileLines[startLine-1:], "\n")

		args, _ := json.Marshal(map[string]any{"path": artifactPath, "line": startLine})
		res := policy.Apply(ctx, sess, &ToolResult{
			Content: pageContent,
			Metadata: map[string]any{
				"tool_name": "read",
				"tool_args": json.RawMessage(args),
			},
		})

		if !res.Truncated {
			// Content fit in budget — all remaining lines shown.
			for _, ln := range fileLines[startLine-1:] {
				if ln == "" {
					continue
				}
				if seen[ln] {
					t.Fatalf("duplicate line %q at startLine=%d", ln, startLine)
				}
				seen[ln] = true
			}
			break
		}

		// Extract the continuation line from the hint.
		hintIdx := strings.Index(res.Content, "\n... [")
		if hintIdx < 0 {
			t.Fatalf("missing continuation hint at startLine=%d: %q", startLine, res.Content)
		}
		preview := res.Content[:hintIdx]
		// Parse "line=N" from the hint.
		hint := res.Content[hintIdx:]
		lineIdx := strings.Index(hint, "line=")
		if lineIdx < 0 {
			t.Fatalf("hint missing line= at startLine=%d: %q", startLine, hint)
		}
		numStr := hint[lineIdx+len("line="):]
		end := strings.IndexAny(numStr, " \n]")
		if end < 0 {
			end = len(numStr)
		}
		nextLine, err := strconv.Atoi(numStr[:end])
		if err != nil {
			t.Fatalf("parse continuation line at startLine=%d: %v", startLine, err)
		}

		// Verify the preview lines are seen exactly once.
		for _, ln := range strings.Split(preview, "\n") {
			// Skip the read prefix line and empty lines.
			if ln == "" || strings.HasPrefix(ln, "[lines ") {
				continue
			}
			if seen[ln] {
				t.Fatalf("duplicate line %q at startLine=%d", ln, startLine)
			}
			seen[ln] = true
		}

		if nextLine <= startLine {
			t.Fatalf("continuation line %d did not advance past startLine %d (infinite loop)", nextLine, startLine)
		}
		startLine = nextLine
	}

	if iterations >= maxIterations {
		t.Fatalf("pagination did not terminate in %d iterations (infinite loop)", maxIterations)
	}

	// Assert every non-empty source line was seen (no skips).
	sourceLines := strings.Split(string(artifactContent), "\n")
	for _, ln := range sourceLines {
		if ln == "" {
			continue
		}
		if !seen[ln] {
			t.Fatalf("line %q was never shown — pagination skipped it", ln)
		}
	}
}

// TestDefaultResultPolicy_ArtifactReadBytesSemantics asserts the in-place
// truncation path records artifact_bytes as the ORIGINAL content size (pre-
// truncation), matching the spill path semantics.
func TestDefaultResultPolicy_ArtifactReadBytesSemantics(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)

	const big = 5000
	bigRes := strings.Repeat("x\n", big/2)
	sess := Session{ID: "s-bytes", Model: &fakeWindowModel{cw: 10_000}}
	policy := &DefaultResultPolicy{}
	ctx := context.Background()

	// Spill to get an artifact path.
	res1 := policy.Apply(ctx, sess, &ToolResult{Content: bigRes})
	artifactPath := res1.FileRef

	args, _ := json.Marshal(map[string]string{"path": artifactPath})
	res2 := policy.Apply(ctx, sess, &ToolResult{
		Content: bigRes,
		Metadata: map[string]any{
			"tool_name": "read",
			"tool_args": json.RawMessage(args),
		},
	})
	if !res2.Truncated {
		t.Fatal("expected in-place truncation")
	}
	got, ok := res2.Metadata["artifact_bytes"].(int)
	if !ok {
		t.Fatalf("artifact_bytes not int: %T %v", res2.Metadata["artifact_bytes"], res2.Metadata["artifact_bytes"])
	}
	if got != len(bigRes) {
		t.Fatalf("artifact_bytes = %d, want original size %d (spill-path semantics)", got, len(bigRes))
	}
}
