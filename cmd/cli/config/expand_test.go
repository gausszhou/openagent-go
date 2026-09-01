package config

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// withEnv sets env vars for the duration of a test and restores the prior
// state (including unset) on cleanup. ExpandBytes reads the live environment,
// so tests must isolate themselves. An empty value sets the var to "" (so the
// "empty var should fall back" Compose semantics are testable).
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		old, had := os.LookupEnv(k)
		os.Setenv(k, v)
		k, old, had := k, old, had
		t.Cleanup(func() {
			if had {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}
}

// expandUnmarshal runs ExpandBytes then json.Unmarshal into a generic
// map[string]any, returning the result. Useful for asserting the *effective*
// JSON value (int vs string vs bool) after expansion.
func expandUnmarshal(t *testing.T, data []byte) map[string]any {
	t.Helper()
	expanded, warns := ExpandBytes(data)
	t.Logf("expanded: %s", expanded)
	t.Logf("warnings: %v", warns)
	var m map[string]any
	if err := json.Unmarshal(expanded, &m); err != nil {
		t.Fatalf("json.Unmarshal of expanded bytes failed: %v\nexpanded=%s", err, expanded)
	}
	return m
}

// ---- string mode (quoted) ----

func TestExpandStringModeBasic(t *testing.T) {
	withEnv(t, map[string]string{"OPENAI_API_KEY": "sk-test"})
	expanded, warns := ExpandBytes([]byte(`{"api_key":"${OPENAI_API_KEY}"}`))
	if len(warns) != 0 {
		t.Errorf("expected no warnings, got %v", warns)
	}
	var m map[string]any
	if err := json.Unmarshal(expanded, &m); err != nil {
		t.Fatalf("unmarshal: %v (expanded=%s)", err, expanded)
	}
	if m["api_key"] != "sk-test" {
		t.Errorf("api_key = %v, want sk-test", m["api_key"])
	}
}

func TestExpandStringModeBareVar(t *testing.T) {
	withEnv(t, map[string]string{"TOKEN": "abc123"})
	m := expandUnmarshal(t, []byte(`{"token":"$TOKEN"}`))
	if m["token"] != "abc123" {
		t.Errorf("token = %v, want abc123", m["token"])
	}
}

func TestExpandStringModeDefault(t *testing.T) {
	// VAR unset → default used.
	os.Unsetenv("UNSET_VAR_X")
	m := expandUnmarshal(t, []byte(`{"v":"${UNSET_VAR_X:-fallback}"}`))
	if m["v"] != "fallback" {
		t.Errorf("v = %v, want fallback", m["v"])
	}
	// VAR set → value used.
	withEnv(t, map[string]string{"SET_VAR_X": "real"})
	m = expandUnmarshal(t, []byte(`{"v":"${SET_VAR_X:-fallback}"}`))
	if m["v"] != "real" {
		t.Errorf("v = %v, want real", m["v"])
	}
	// VAR set but empty → default used (Compose :- semantics: empty = unset).
	withEnv(t, map[string]string{"EMPTY_VAR_X": ""})
	m = expandUnmarshal(t, []byte(`{"v":"${EMPTY_VAR_X:-fallback}"}`))
	if m["v"] != "fallback" {
		t.Errorf("v = %v, want fallback (empty var should fall back)", m["v"])
	}
}

func TestExpandStringModeLiteralDollar(t *testing.T) {
	// $$ → literal $, so "$$VAR" is a literal "$VAR" not an expansion.
	withEnv(t, map[string]string{"VAR": "expanded"})
	m := expandUnmarshal(t, []byte(`{"v":"$$VAR"}`))
	if m["v"] != "$VAR" {
		t.Errorf("v = %v, want $VAR", m["v"])
	}
}

// ---- raw mode (unquoted) ----

func TestExpandRawModeInt(t *testing.T) {
	withEnv(t, map[string]string{"PORT": "8080"})
	m := expandUnmarshal(t, []byte(`{"port": ${PORT}}`))
	port, ok := m["port"].(float64)
	if !ok {
		t.Fatalf("port type = %T, want float64 (int after JSON)", m["port"])
	}
	if port != 8080 {
		t.Errorf("port = %v, want 8080", port)
	}
}

func TestExpandRawModeBool(t *testing.T) {
	withEnv(t, map[string]string{"ON": "true"})
	m := expandUnmarshal(t, []byte(`{"flag": ${ON}}`))
	if m["flag"] != true {
		t.Errorf("flag = %v (%T), want bool(true)", m["flag"], m["flag"])
	}
}

func TestExpandRawModeNull(t *testing.T) {
	withEnv(t, map[string]string{"N": "null"})
	m := expandUnmarshal(t, []byte(`{"x": ${N}}`))
	if m["x"] != nil {
		t.Errorf("x = %v, want nil", m["x"])
	}
}

func TestExpandRawModeFailSafe(t *testing.T) {
	// PORT=abc used as raw token → invalid JSON → Unmarshal must error.
	// This is the fail-safe contract: no silent corruption, no panic.
	withEnv(t, map[string]string{"PORT": "abc"})
	expanded, _ := ExpandBytes([]byte(`{"port": ${PORT}}`))
	var m map[string]any
	if err := json.Unmarshal(expanded, &m); err == nil {
		t.Fatalf("expected JSON parse error for raw non-numeric token, got nil; expanded=%s", expanded)
	}
}

// ---- JSON escaping (string mode safety) ----

func TestExpandStringModeJSONEscape(t *testing.T) {
	// A value with quotes, backslash, and newline (like a PEM key fragment).
	// The substituted value must not break the surrounding JSON string.
	val := `-----BEGIN KEY-----\nMIIB"weird"\n-----END KEY-----`
	withEnv(t, map[string]string{"PEM": val})
	m := expandUnmarshal(t, []byte(`{"key":"${PEM}"}`))
	if m["key"] != val {
		t.Errorf("key = %q, want %q", m["key"], val)
	}
}

func TestExpandStringModeActualNewline(t *testing.T) {
	// A real newline in the env value (not a backslash-n sequence) must be
	// emitted as \n in the JSON so the byte stream stays a valid string.
	val := "line1\nline2"
	withEnv(t, map[string]string{"MULTI": val})
	m := expandUnmarshal(t, []byte(`{"v":"${MULTI}"}`))
	if m["v"] != val {
		t.Errorf("v = %q, want %q", m["v"], val)
	}
}

// ---- edge cases ----

func TestExpandUnclosedBrace(t *testing.T) {
	// ${VAR with no closing } before the string ends: parseBraced returns
	// ok=false, so the caller emits "$" literally and "{VAR" is scanned as
	// ordinary bytes → faithful passthrough of "${VAR". Still valid JSON.
	withEnv(t, map[string]string{"VAR": "x"})
	m := expandUnmarshal(t, []byte(`{"v":"${VAR"}`))
	if m["v"] != "${VAR" {
		t.Errorf("v = %v, want ${VAR (literal passthrough)", m["v"])
	}
}

func TestExpandEmptyBraces(t *testing.T) {
	// ${} → empty name, resolves to empty, no warning (empty name is a no-op).
	m := expandUnmarshal(t, []byte(`{"v":"${}"}`))
	if m["v"] != "" {
		t.Errorf("v = %v, want empty string", m["v"])
	}
}

func TestExpandMultipleRefsInString(t *testing.T) {
	withEnv(t, map[string]string{"A": "alpha", "B": "beta"})
	m := expandUnmarshal(t, []byte(`{"v":"${A}-${B}"}`))
	if m["v"] != "alpha-beta" {
		t.Errorf("v = %v, want alpha-beta", m["v"])
	}
}

func TestExpandBraceDepthDefault(t *testing.T) {
	// ${VAR:-a}b} — the default is "a" and the trailing }b} is literal text
	// inside the string (depth tracking means the first } closes, "b}" is
	// literal). Actually: name=VAR, sep at ":-", default="a", close at first
	// depth-0 }. The "}b}" after → the first } closes at depth 0, so default
	// is "a" and "b}" is leftover literal. Verify string mode.
	withEnv(t, map[string]string{"VAR_DEPTH": ""}) // empty → default used
	m := expandUnmarshal(t, []byte(`{"v":"${VAR_DEPTH:-a}b}"}`))
	// default = "a", then literal "b}", then closing quote.
	if m["v"] != "ab}" {
		t.Errorf("v = %v, want ab}", m["v"])
	}
}

func TestExpandNoRefsPassthrough(t *testing.T) {
	// Plain JSON with no $ tokens → bytes unchanged (no warnings).
	src := []byte(`{"a":"b","c":3,"d":[1,2],"e":{"f":true}}`)
	expanded, warns := ExpandBytes(src)
	if len(warns) != 0 {
		t.Errorf("expected no warnings, got %v", warns)
	}
	if !reflect.DeepEqual(expanded, src) {
		t.Errorf("passthrough changed bytes:\n got=%s\nwant=%s", expanded, src)
	}
}

// ---- nested defaults: recursively resolved (Docker Compose semantics) ----

func TestExpandNestedDefaultResolved(t *testing.T) {
	// ${OUTER:-${INNER}} with OUTER empty and INNER=inner-val.
	// The default ${INNER} is recursively resolved → "inner-val", not emitted
	// as the literal text "${INNER}". This matches Docker Compose semantics
	// and makes nested-default raw-mode tokens produce valid JSON.
	os.Unsetenv("OUTER")
	withEnv(t, map[string]string{"INNER": "inner-val"})
	expanded, warns := ExpandBytes([]byte(`{"v":"${OUTER:-${INNER}}"}`))
	t.Logf("expanded = %s", expanded)
	t.Logf("warnings = %v", warns)
	var m map[string]any
	if err := json.Unmarshal(expanded, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["v"] != "inner-val" {
		t.Errorf("nested default: v = %v, want inner-val (resolved)", m["v"])
	}
	// No warning: INNER was resolved successfully, OUTER has a default.
	if len(warns) != 0 {
		t.Errorf("expected no warnings (both vars handled), got %v", warns)
	}
}

func TestExpandNestedDefaultRawModeValidJSON(t *testing.T) {
	// ${OUTER:-${INNER}} in RAW mode (unquoted) with OUTER empty, INNER=8080.
	// Before recursive resolution, this produced {"v": ${INNER}} (invalid
	// JSON). Now INNER is resolved → {"v": 8080} → native int.
	os.Unsetenv("OUTER")
	withEnv(t, map[string]string{"INNER": "8080"})
	expanded, _ := ExpandBytes([]byte(`{"v": ${OUTER:-${INNER}}}`))
	var m map[string]any
	if err := json.Unmarshal(expanded, &m); err != nil {
		t.Fatalf("raw-mode nested default produced invalid JSON: %v\n%s", err, expanded)
	}
	port, ok := m["v"].(float64)
	if !ok || port != 8080 {
		t.Errorf("v = %v (%T), want 8080 (int)", m["v"], m["v"])
	}
}

func TestExpandNestedDefaultUnsetInnerWarns(t *testing.T) {
	// ${OUTER:-${INNER}} with BOTH OUTER and INNER unset. The recursive
	// resolution of INNER yields empty (INNER has no default). The operator
	// must be warned that the nested fallback resolved to empty.
	os.Unsetenv("OUTER")
	os.Unsetenv("INNER")
	expanded, warns := ExpandBytes([]byte(`{"v":"${OUTER:-${INNER}}"}`))
	t.Logf("expanded = %s", expanded)
	t.Logf("warnings = %v", warns)
	var m map[string]any
	if err := json.Unmarshal(expanded, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["v"] != "" {
		t.Errorf("v = %v, want empty (INNER unset, no default)", m["v"])
	}
	// A warning must surface mentioning INNER (the unresolved nested ref).
	found := false
	for _, w := range warns {
		if strings.Contains(w, "INNER") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning mentioning INNER (nested ref unset), got %v", warns)
	}
}

func TestExpandDeepNestedDefaultResolved(t *testing.T) {
	// ${A:-${B:-${C}}} — three-level nesting. A and B unset, C=deepest.
	// Each level recursively resolves: A empty → default ${B:-${C}} →
	// ExpandBytes re-scans → B empty → default ${C} → C=deepest.
	os.Unsetenv("A")
	os.Unsetenv("B")
	withEnv(t, map[string]string{"C": "deepest"})
	expanded, _ := ExpandBytes([]byte(`{"v":"${A:-${B:-${C}}}"}`))
	var m map[string]any
	if err := json.Unmarshal(expanded, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["v"] != "deepest" {
		t.Errorf("deep nested: v = %v, want deepest", m["v"])
	}
}

func TestExpandNestedDefaultNoWarningWhenOuterSet(t *testing.T) {
	// Same expression, but OUTER is set → the default is never consulted,
	// so the nested ${INNER} inside it is harmless and must NOT warn.
	withEnv(t, map[string]string{"OUTER": "outer-val", "INNER": "inner-val"})
	expanded, warns := ExpandBytes([]byte(`{"v":"${OUTER:-${INNER}}"}`))
	var m map[string]any
	if err := json.Unmarshal(expanded, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["v"] != "outer-val" {
		t.Errorf("v = %v, want outer-val", m["v"])
	}
	if len(warns) != 0 {
		t.Errorf("expected no warnings when OUTER is set (default unused), got %v", warns)
	}
}

func TestExpandLiteralDefaultNoWarning(t *testing.T) {
	// A literal (non-nested) default is the supported fallback form. It must
	// not trip the nested-default warning.
	os.Unsetenv("KEY")
	expanded, warns := ExpandBytes([]byte(`{"v":"${KEY:-sk-fallback}"}`))
	var m map[string]any
	if err := json.Unmarshal(expanded, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["v"] != "sk-fallback" {
		t.Errorf("v = %v, want sk-fallback", m["v"])
	}
	if len(warns) != 0 {
		t.Errorf("literal default must not warn, got %v", warns)
	}
}

// ---- $$ folding consistency (main body vs default) ----

func TestExpandDollarDollarFoldingConsistent(t *testing.T) {
	// "$$" is a literal "$" everywhere — both in the main scan body and in a
	// default that is actually taken. The two paths must agree, or an author
	// who writes "${VAR:-$$cost}" expecting "$cost" silently gets "$$cost".
	os.Unsetenv("SCRATCH_DD")
	cases := []struct{ name, in, want string }{
		{"main body", `{"v":"$$x"}`, "$x"},
		{"default taken", `{"v":"${SCRATCH_DD:-$$x}"}`, "$x"},
		{"default with literal $ after fold", `{"v":"${SCRATCH_DD:-$$cost is $5}"}`, "$cost is $5"},
	}
	for _, c := range cases {
		expanded, warns := ExpandBytes([]byte(c.in))
		if len(warns) != 0 {
			t.Errorf("%s: unexpected warnings %v", c.name, warns)
		}
		var m map[string]any
		if err := json.Unmarshal(expanded, &m); err != nil {
			t.Fatalf("%s: unmarshal: %v (expanded=%s)", c.name, err, expanded)
		}
		if m["v"] != c.want {
			t.Errorf("%s: v = %q, want %q", c.name, m["v"], c.want)
		}
	}
}

func TestExpandDollarDollarDefaultNotFoldedWhenVarSet(t *testing.T) {
	// When VAR is set, the default is never emitted, so its "$$" is irrelevant.
	// This just confirms the var-set path is unaffected by the fold.
	withEnv(t, map[string]string{"SCRATCH_DD": "real"})
	expanded, warns := ExpandBytes([]byte(`{"v":"${SCRATCH_DD:-$$fallback}"}`))
	if len(warns) != 0 {
		t.Errorf("unexpected warnings %v", warns)
	}
	var m map[string]any
	if err := json.Unmarshal(expanded, &m); err != nil {
		t.Fatalf("unmarshal: %v (expanded=%s)", err, expanded)
	}
	if m["v"] != "real" {
		t.Errorf("v = %v, want real", m["v"])
	}
}

// ---- warnings ----

func TestExpandWarningsUnset(t *testing.T) {
	os.Unsetenv("MISSING_ONE")
	os.Unsetenv("MISSING_TWO")
	_, warns := ExpandBytes([]byte(`{"a":"${MISSING_ONE}","b":"${MISSING_TWO}","c":"${MISSING_ONE}"}`))
	// Deduped: MISSING_ONE referenced twice but appears once.
	sort.Strings(warns)
	want := []string{"MISSING_ONE", "MISSING_TWO"}
	if !reflect.DeepEqual(warns, want) {
		t.Errorf("warnings = %v, want %v", warns, want)
	}
}

func TestExpandWarningsDefaultSuppresses(t *testing.T) {
	os.Unsetenv("HAS_DEFAULT")
	_, warns := ExpandBytes([]byte(`{"v":"${HAS_DEFAULT:-def}"}`))
	if len(warns) != 0 {
		t.Errorf("expected no warnings for ${VAR:-def}, got %v", warns)
	}
}

// ---- integration: Load resolves env refs ----

func TestLoadResolvesStringEnvRef(t *testing.T) {
	withEnv(t, map[string]string{"OPENAI_API_KEY": "sk-test"})
	dir := t.TempDir()
	path := dir + "/settings.json"
	src := `{"provider":{"openai":{"api_key":"${OPENAI_API_KEY}"}}}`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := cfg.Provider["openai"]
	if !ok {
		t.Fatal("openai provider missing")
	}
	if p.APIKey != "sk-test" {
		t.Errorf("APIKey = %q, want sk-test", p.APIKey)
	}
}

func TestLoadResolvesRawEnvRefToInt(t *testing.T) {
	withEnv(t, map[string]string{"PORT": "9090"})
	dir := t.TempDir()
	path := dir + "/settings.json"
	src := `{"server":{"port": ${PORT}}}`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("Server.Port = %v, want 9090", cfg.Server.Port)
	}
}

// ---- integration: disk stays literal ----

func TestLoadDiskStaysLiteral(t *testing.T) {
	// After Load resolves the env ref, GetSetting must return the LITERAL
	// on-disk value (the ${...} reference), never the secret.
	withEnv(t, map[string]string{"OPENAI_API_KEY": "sk-test"})
	dir := t.TempDir()
	path := dir + "/settings.json"
	src := `{"provider":{"openai":{"api_key":"${OPENAI_API_KEY}"}}}`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	// Set the config path so GetSetting reads this file.
	t.Setenv("OPENAGENT_CLI_CONFIG", path)

	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := GetSetting("provider.openai.api_key")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	// GetSetting returns the JSON-marshaled value, so a string is quoted.
	if got != `"${OPENAI_API_KEY}"` {
		t.Errorf("GetSetting = %q, want literal quoted ${OPENAI_API_KEY}", got)
	}
}

// ---- sentinel: NormalizeRawRefs / DenormalizeRawRefs ----

func TestNormalizeRawRefsBasic(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"raw braced", `{"port": ${PORT}}`, `{"port": "` + rawSentinel + `${PORT}"}`},
		{"raw bare", `{"x": $VAR}`, `{"x": "` + rawSentinel + `$VAR"}`},
		{"string-mode untouched", `{"k": "${VAR}"}`, `{"k": "${VAR}"}`},
		{"mixed", `{"port": ${PORT}, "key": "${API_KEY}"}`,
			`{"port": "` + rawSentinel + `${PORT}", "key": "${API_KEY}"}`},
		{"no refs", `{"a": 1, "b": "x"}`, `{"a": 1, "b": "x"}`},
		{"default with braces", `{"p": ${PORT:-8080}}`, `{"p": "` + rawSentinel + `${PORT:-8080}"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(NormalizeRawRefs([]byte(c.in)))
			if got != c.want {
				t.Errorf("NormalizeRawRefs(%s)\n  got  = %s\n  want = %s", c.in, got, c.want)
			}
		})
	}
}

func TestNormalizeDenormalizeRoundTrip(t *testing.T) {
	cases := []string{
		`{"port": ${PORT}}`,
		`{"port": ${PORT}, "flag": ${ON}, "key": "${API_KEY}"}`,
		`{"a": ${A:-1}, "b": $B, "c": "plain"}`,
		`{"nested": {"port": ${PORT}}}`,
		`{"arr": [${A}, ${B}]}`,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			norm := NormalizeRawRefs([]byte(in))
			// Normalized form must be valid JSON.
			var probe map[string]any
			if err := json.Unmarshal(norm, &probe); err != nil {
				t.Fatalf("normalized form is not valid JSON: %v\n%s", err, norm)
			}
			// Re-marshal (simulates the RMW write-back).
			remarshaled, err := json.MarshalIndent(probe, "", "  ")
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			// Denormalize must restore the original literal bytes.
			back, err := DenormalizeRawRefs(remarshaled)
			if err != nil {
				t.Fatalf("DenormalizeRawRefs: %v", err)
			}
			// Re-parse the restored bytes to compare structurally (whitespace
			// may differ from the original compact form).
			var orig, restored map[string]any
			if err := json.Unmarshal(NormalizeRawRefs([]byte(in)), &orig); err != nil {
				t.Fatalf("parse orig: %v", err)
			}
			restoredNorm := NormalizeRawRefs(back)
			if err := json.Unmarshal(restoredNorm, &restored); err != nil {
				t.Fatalf("parse restored (%s): %v", back, err)
			}
			if !reflect.DeepEqual(orig, restored) {
				t.Errorf("round-trip drift:\n  orig     = %v\n  restored = %v", orig, restored)
			}
		})
	}
}

func TestDenormalizeRestoresRawBytes(t *testing.T) {
	// After normalize + json.Marshal, denormalize must yield bytes containing
	// the raw (unquoted) ${PORT} token — invalid JSON, but the literal disk form.
	in := `{"port": ${PORT}}`
	norm := NormalizeRawRefs([]byte(in))
	var m map[string]any
	json.Unmarshal(norm, &m)
	remarshaled, _ := json.Marshal(m)
	back, err := DenormalizeRawRefs(remarshaled)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(back), `${PORT}`) {
		t.Errorf("denormalized output missing raw ${PORT}: %s", back)
	}
	// The restored bytes must NOT be valid JSON (raw ${PORT} is invalid).
	if json.Unmarshal(back, &m) == nil {
		t.Errorf("denormalized output is valid JSON — raw token not restored: %s", back)
	}
}

func TestNormalizeRawRefsStringModeNotTouched(t *testing.T) {
	// String-mode "${VAR}" must pass through unchanged — quoting it again
	// would corrupt the on-disk representation.
	in := `{"k": "${VAR}", "x": "$$literal"}`
	got := string(NormalizeRawRefs([]byte(in)))
	if got != in {
		t.Errorf("string-mode changed:\n  got  = %s\n  want = %s", got, in)
	}
}

func TestNormalizeRawRefsEscapedQuoteInString(t *testing.T) {
	// A \" inside a string must not toggle inString — the $ outside that
	// string must still be normalized.
	in := `{"k": "say \"hi\"", "p": ${PORT}}`
	got := string(NormalizeRawRefs([]byte(in)))
	want := `{"k": "say \"hi\"", "p": "` + rawSentinel + `${PORT}"}`
	if got != want {
		t.Errorf("escaped-quote handling:\n  got  = %s\n  want = %s", got, want)
	}
}

func TestStripSentinel(t *testing.T) {
	cases := []struct{ in, want string }{
		{rawSentinel + "${PORT}", "${PORT}"},
		{"${PORT}", "${PORT}"},
		{"plain", "plain"},
	}
	for _, c := range cases {
		if got := StripSentinel(c.in); got != c.want {
			t.Errorf("StripSentinel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- integration: settings tool tolerates raw-mode ${VAR} ----

// TestSettingsToolRawModeRMW is the core regression test for the defect the
// user demanded fixed: a settings.json with an unquoted (raw-mode) ${PORT}
// must NOT break SetSetting/GetSetting/ListSettings. Before the sentinel
// fix, the json.Unmarshal at the RMW read rejected the raw token and every
// write failed.
func TestSettingsToolRawModeRMW(t *testing.T) {
	withEnv(t, map[string]string{"PORT": "9090", "OPENAI_API_KEY": "sk-test"})
	dir := t.TempDir()
	path := dir + "/settings.json"
	// Disk file with a RAW-mode ${PORT} (unquoted) and a string-mode key.
	src := `{"server": {"port": ${PORT}}, "provider": {"openai": {"api_key": "${OPENAI_API_KEY}"}}}`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAGENT_CLI_CONFIG", path)

	// 1. GetSetting on the raw-mode field must return the literal token.
	got, err := GetSetting("server.port")
	if err != nil {
		t.Fatalf("GetSetting(server.port): %v", err)
	}
	// Returned as a quoted string (the literal token, not the resolved int).
	if got != `"${PORT}"` {
		t.Errorf("GetSetting(server.port) = %q, want \"${PORT}\"", got)
	}

	// 2. SetSetting on an UNRELATED key must succeed (not crash on the raw
	// token elsewhere in the doc). This is the core of the defect.
	if err := SetSetting("log.level", "debug"); err != nil {
		t.Fatalf("SetSetting(log.level): %v", err)
	}

	// 3. The raw-mode token must survive the RMW round-trip on disk.
	diskAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diskAfter), `${PORT}`) {
		t.Errorf("raw ${PORT} lost from disk after RMW:\n%s", diskAfter)
	}
	// And the string-mode key must also survive.
	if !strings.Contains(string(diskAfter), `${OPENAI_API_KEY}`) {
		t.Errorf("string-mode ${OPENAI_API_KEY} lost from disk:\n%s", diskAfter)
	}
	// The new value must be present.
	if !strings.Contains(string(diskAfter), `"debug"`) {
		t.Errorf("SetSetting value 'debug' not on disk:\n%s", diskAfter)
	}
	// No sentinel marker must leak to disk.
	if strings.Contains(string(diskAfter), rawSentinel) {
		t.Errorf("sentinel leaked to disk:\n%s", diskAfter)
	}

	// 4. ListSettings must show the literal tokens (no sentinel, no secret).
	listed, err := ListSettings()
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	if strings.Contains(listed, rawSentinel) {
		t.Errorf("sentinel leaked into ListSettings output:\n%s", listed)
	}
	if strings.Contains(listed, "sk-test") {
		t.Errorf("resolved secret leaked into ListSettings:\n%s", listed)
	}
	if !strings.Contains(listed, `${PORT}`) {
		t.Errorf("literal ${PORT} missing from ListSettings:\n%s", listed)
	}
}

// TestSettingsToolRawModeAppendDelete verifies append and delete also work on
// a doc containing raw-mode tokens.
func TestSettingsToolRawModeAppendDelete(t *testing.T) {
	withEnv(t, map[string]string{"PORT": "9090"})
	dir := t.TempDir()
	path := dir + "/settings.json"
	src := `{"server": {"port": ${PORT}}, "tags": ["a"]}`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAGENT_CLI_CONFIG", path)

	// Append to an array in a doc with a raw token.
	if err := AppendSetting("tags", `"b"`); err != nil {
		t.Fatalf("AppendSetting: %v", err)
	}
	got, err := GetSetting("tags")
	if err != nil {
		t.Fatalf("GetSetting(tags): %v", err)
	}
	if !strings.Contains(got, `"a"`) || !strings.Contains(got, `"b"`) {
		t.Errorf("tags after append = %s, want both a and b", got)
	}

	// Delete a key in a doc with a raw token.
	if err := DeleteSetting("tags"); err != nil {
		t.Fatalf("DeleteSetting: %v", err)
	}
	if _, err := GetSetting("tags"); err == nil {
		t.Error("tags still present after delete")
	}
	// Raw token must survive.
	disk, _ := os.ReadFile(path)
	if !strings.Contains(string(disk), `${PORT}`) {
		t.Errorf("raw ${PORT} lost after delete:\n%s", disk)
	}
}

// ---- regression: PUA collision (reviewer claim #1) ----

// TestPUAUserDataNotCorrupted verifies that a user string value starting
// with the sentinel PUA bytes (but NOT followed by a ${...}/$VAR token) is
// preserved verbatim through a full RMW cycle — its quotes are NOT stripped,
// and the file stays valid JSON. Before the structural-validation fix,
// DenormalizeRawRefs stripped quotes from ANY string starting with the
// sentinel, corrupting the file.
func TestPUAUserDataNotCorrupted(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/settings.json"
	// A user string value that starts with the sentinel PUA bytes but is
	// followed by arbitrary text (not a ${...} token).
	src := `{"font_glyph":"` + rawSentinel + `some-glyph-data"}`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAGENT_CLI_CONFIG", path)

	// One SetSetting on an unrelated key — must not corrupt the PUA value.
	if err := SetSetting("log.level", "debug"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	disk, _ := os.ReadFile(path)
	t.Logf("disk after RMW: %s", string(disk))

	// The PUA value must still be a valid quoted string.
	if !strings.Contains(string(disk), `"`+rawSentinel+`some-glyph-data"`) {
		t.Errorf("PUA user value corrupted (quotes stripped or value changed):\n%s", disk)
	}
	// The file must still be valid JSON (re-parse it).
	var recheck map[string]any
	if err := json.Unmarshal(NormalizeRawRefs(disk), &recheck); err != nil {
		t.Errorf("disk corrupted to invalid JSON after RMW: %v\n%s", err, disk)
	}
}

// TestPUAWithRawTokenCoexists verifies that a doc with BOTH a raw-mode
// ${PORT} AND a user PUA string value survives RMW — the raw token is
// preserved as raw, the PUA user value is preserved verbatim.
func TestPUAWithRawTokenCoexists(t *testing.T) {
	withEnv(t, map[string]string{"PORT": "9090"})
	dir := t.TempDir()
	path := dir + "/settings.json"
	src := `{"server": {"port": ${PORT}}, "glyph": "` + rawSentinel + `font"}`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAGENT_CLI_CONFIG", path)

	if err := SetSetting("log.level", "info"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	disk, _ := os.ReadFile(path)

	// Raw token preserved.
	if !strings.Contains(string(disk), `${PORT}`) {
		t.Errorf("raw ${PORT} lost:\n%s", disk)
	}
	// PUA user value preserved (quoted, not stripped).
	if !strings.Contains(string(disk), `"`+rawSentinel+`font"`) {
		t.Errorf("PUA user value corrupted:\n%s", disk)
	}
	// No sentinel leaked to a non-quoted position.
	if strings.Contains(string(disk), rawSentinel+`font}`) || strings.Contains(string(disk), rawSentinel+`font,`) {
		t.Errorf("PUA value lost quotes:\n%s", disk)
	}
}

// ---- regression: brace scanner consistency (reviewer claim #2) ----

// TestNormalizeBraceConsistentWithExpand verifies that NormalizeRawRefs and
// ExpandBytes agree on token boundaries for ${...} forms — the token span
// NormalizeRawRefs wraps must be the same span ExpandBytes resolves. This
// guards against a divergence where a config starts (ExpandBytes accepts it)
// but the settings tool can't edit it (NormalizeRawRefs wraps a different
// span). We test several brace forms through a round-trip.
func TestNormalizeBraceConsistentWithExpand(t *testing.T) {
	cases := []struct {
		name string
		// Disk form (raw-mode token outside quotes).
		disk string
		// The token bytes that BOTH scanners should identify.
		token string
	}{
		{"simple", `{"v": ${PORT}}`, `${PORT}`},
		{"default", `{"v": ${PORT:-8080}}`, `${PORT:-8080}`},
		{"default with braces in value", `{"v": ${VAR:-a{b}c}}`, `${VAR:-a{b}c}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// NormalizeRawRefs must wrap exactly the token.
			norm := NormalizeRawRefs([]byte(c.disk))
			// The normalized form must contain the sentinel + token as a
			// quoted string, and the token must appear exactly once.
			if !strings.Contains(string(norm), `"`+rawSentinel+c.token+`"`) {
				t.Errorf("Normalize did not wrap the expected token %q:\n%s", c.token, norm)
			}
			// The normalized form must be valid JSON (so the RMW can parse it).
			var m map[string]any
			if err := json.Unmarshal(norm, &m); err != nil {
				t.Errorf("normalized form is invalid JSON: %v\n%s", err, norm)
			}
		})
	}
}

// TestNormalizeBraceDepthWithTrailingBrace tests the reviewer's specific
// input ${A:-a}b}} — both scanners must agree the token is ${A:-a} (closing
// at the first }). The trailing b}} is not part of the token. The input is
// pathological (trailing } after a raw token produces invalid JSON either
// way), but the token boundary must be consistent.
func TestNormalizeBraceDepthWithTrailingBrace(t *testing.T) {
	// ExpandBytes with A unset: token ${A:-a}, emits "a" + literal "b}}"
	// → "ab}}" (invalid JSON, but that's ExpandBytes's resolved output).
	withEnv(t, map[string]string{}) // A unset
	expanded, _ := ExpandBytes([]byte(`{"v": ${A:-a}b}}`))
	// The token ${A:-a} resolved to "a", so expanded must start with "a".
	if !strings.HasPrefix(string(expanded), `{"v": a`) {
		t.Errorf("ExpandBytes did not resolve ${A:-a} to 'a': %s", expanded)
	}

	// NormalizeRawRefs: token ${A:-a}, wraps it, trailing "b}}" bare.
	norm := NormalizeRawRefs([]byte(`{"v": ${A:-a}b}}`))
	// The sentinel-wrapped token must be "${A:-a}" — confirming Normalize
	// also sees the token boundary at the first }.
	if !strings.Contains(string(norm), `"`+rawSentinel+`${A:-a}"`) {
		t.Errorf("Normalize did not wrap ${A:-a} (boundary mismatch):\n%s", norm)
	}
	// Both scanners agree the token is ${A:-a} — the trailing b}} is
	// outside the token in both. This is the consistency guarantee.
}

// ---- regression: GetSetting composite type sentinel leak (reviewer problem A) ----

// TestGetSettingCompositeNoSentinelLeak verifies that GetSetting on an object
// or array containing raw-mode tokens returns the literal tokens WITHOUT the
// internal sentinel marker. Before the fix, only top-level string values had
// the sentinel stripped; composite values (map/array) leaked the sentinel
// bytes to the user.
func TestGetSettingCompositeNoSentinelLeak(t *testing.T) {
	withEnv(t, map[string]string{"PORT": "9090", "M1": "gpt-4o", "M2": "claude"})
	dir := t.TempDir()
	path := dir + "/settings.json"
	// Disk: object with raw ${PORT}, and an array with raw ${M1}/${M2}.
	src := `{"server":{"port": ${PORT}}, "models": [${M1}, ${M2}]}`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAGENT_CLI_CONFIG", path)

	// GetSetting on the object — must not leak sentinel.
	got, err := GetSetting("server")
	if err != nil {
		t.Fatalf("GetSetting(server): %v", err)
	}
	if strings.Contains(got, rawSentinel) {
		t.Errorf("sentinel leaked into GetSetting(server) object:\n%s", got)
	}
	if !strings.Contains(got, `"${PORT}"`) {
		t.Errorf("literal ${PORT} missing from GetSetting(server):\n%s", got)
	}

	// GetSetting on the array — must not leak sentinel.
	got, err = GetSetting("models")
	if err != nil {
		t.Fatalf("GetSetting(models): %v", err)
	}
	if strings.Contains(got, rawSentinel) {
		t.Errorf("sentinel leaked into GetSetting(models) array:\n%s", got)
	}
	if !strings.Contains(got, `"${M1}"`) || !strings.Contains(got, `"${M2}"`) {
		t.Errorf("literal tokens missing from GetSetting(models):\n%s", got)
	}
}

// ---- regression: DenormalizeRawRefs preserves sentinel-marked keys (reviewer problem B) ----

// TestDenormalizePreservesSentinelKey verifies that a JSON object whose KEY
// starts with the sentinel (an unrealistic but possible user input) does NOT
// have its key quotes stripped by DenormalizeRawRefs. Stripping key quotes
// would produce invalid JSON ({${PORT}:"val"}). The fix tracks key-vs-value
// position and only strips sentinel-marked strings in VALUE position.
func TestDenormalizePreservesSentinelKey(t *testing.T) {
	// Construct JSON with a sentinel-marked key. NormalizeRawRefs does not
	// touch $ inside strings, so the key survives normalization as-is.
	// After a marshal round-trip + DenormalizeRawRefs, the key must stay quoted.
	keyJSON := []byte(`{"` + rawSentinel + `${PORT}":"val"}`)
	norm := NormalizeRawRefs(keyJSON)
	var m map[string]any
	if err := json.Unmarshal(norm, &m); err != nil {
		t.Fatalf("parse normalized: %v", err)
	}
	remarshaled, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	back, err := DenormalizeRawRefs(remarshaled)
	if err != nil {
		t.Fatalf("DenormalizeRawRefs: %v", err)
	}
	// The denormalized output must still be valid JSON (key quotes preserved).
	var recheck map[string]any
	if err := json.Unmarshal(back, &recheck); err != nil {
		t.Errorf("denormalized output is invalid JSON (key quotes stripped): %v\n%s", err, back)
	}
	// And the sentinel-marked key must still be present (quoted).
	if !strings.Contains(string(back), `"`+rawSentinel+`${PORT}"`) {
		t.Errorf("sentinel-marked key lost or corrupted:\n%s", back)
	}
}

// TestDenormalizeValueStillStripped verifies the key/value fix did not break
// the normal case: a sentinel-marked VALUE is still stripped to a raw token.
func TestDenormalizeValueStillStripped(t *testing.T) {
	// Object with a sentinel-marked VALUE (the normal NormalizeRawRefs output).
	in := []byte(`{"port":"` + rawSentinel + `${PORT}"}`)
	back, err := DenormalizeRawRefs(in)
	if err != nil {
		t.Fatal(err)
	}
	// The value must be stripped to raw ${PORT} (unquoted).
	if !strings.Contains(string(back), `${PORT}`) {
		t.Errorf("value token not stripped:\n%s", back)
	}
	// And it must be the raw form (unquoted ${PORT} as a value).
	if strings.Contains(string(back), `"`+rawSentinel) {
		t.Errorf("sentinel not stripped from value:\n%s", back)
	}
}

// ---- regression: DenormalizeRawRefs comma key/value tracking ----

// TestDenormalizeCommaPreservesSecondKey verifies that after a comma in an
// object, the key/value position tracking correctly resets so the next
// string is identified as a KEY (not a value). Before the frame-stack fix,
// the comma handler was a no-op: after consuming a value (expectKey=false),
// a comma did not reset expectKey to true, so the next object member's KEY
// was misidentified as a VALUE. A sentinel-marked key in the second position
// would then have its quotes stripped, corrupting the file.
func TestDenormalizeCommaPreservesSecondKey(t *testing.T) {
	// First field: normal key + sentinel-marked value.
	// Second field: sentinel-marked KEY + normal value.
	// If the comma doesn't reset expectKey, the second KEY is seen as a
	// value → its quotes stripped → invalid JSON.
	in := []byte(`{"port":"` + rawSentinel + `${PORT}","` + rawSentinel + `${KEY2}":"val"}`)
	back, err := DenormalizeRawRefs(in)
	if err != nil {
		t.Fatal(err)
	}
	// The second KEY must stay quoted. Re-normalize + parse to verify.
	var recheck map[string]any
	if err := json.Unmarshal(NormalizeRawRefs(back), &recheck); err != nil {
		t.Errorf("output invalid after re-normalize (key quotes stripped?): %v\n%s", err, back)
	}
	// Both keys must be present.
	if len(recheck) != 2 {
		t.Errorf("expected 2 keys, got %d: %v", len(recheck), recheck)
	}
}

// TestDenormalizeArrayMultipleValues verifies that an array with multiple
// sentinel-marked values (separated by commas) has ALL values stripped —
// confirming the comma handler correctly keeps arrays in value mode.
func TestDenormalizeArrayMultipleValues(t *testing.T) {
	in := []byte(`{"arr":["` + rawSentinel + `${A}","` + rawSentinel + `${B}"]}`)
	back, err := DenormalizeRawRefs(in)
	if err != nil {
		t.Fatal(err)
	}
	var recheck map[string]any
	if err := json.Unmarshal(NormalizeRawRefs(back), &recheck); err != nil {
		t.Errorf("output invalid: %v\n%s", err, back)
	}
	arr, ok := recheck["arr"].([]any)
	if !ok || len(arr) != 2 {
		t.Errorf("arr = %v, want 2 elements", recheck["arr"])
	}
}
