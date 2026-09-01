package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeSettings writes src to a temp settings.json and sets
// OPENAGENT_CLI_CONFIG to point at it. Returns the path.
func writeSettings(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAGENT_CLI_CONFIG", path)
	return path
}

func TestValidateValidConfig(t *testing.T) {
	writeSettings(t, `{"log":{"level":"debug"},"server":{"port":8080}}`)
	report, err := ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	if len(report.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", report.Warnings)
	}
	if len(report.EnumViolations) != 0 {
		t.Errorf("enum violations = %v, want none", report.EnumViolations)
	}
}

func TestValidateMissingFile(t *testing.T) {
	// Point at a non-existent file — valid (defaults apply).
	dir := t.TempDir()
	t.Setenv("OPENAGENT_CLI_CONFIG", filepath.Join(dir, "nonexistent.json"))
	report, err := ValidateSettings()
	if err != nil {
		t.Fatalf("missing file should be valid (defaults), got: %v", err)
	}
	if report == nil {
		t.Fatal("report is nil")
	}
}

func TestValidateUnsetVarWarning(t *testing.T) {
	// ${MISSING_VAR} with no default → empty value, warning reported.
	// The config still parses (empty string is valid for a string field).
	os.Unsetenv("MISSING_VAR_X")
	writeSettings(t, `{"provider":{"openai":{"api_key":"${MISSING_VAR_X}"}}}`)
	report, err := ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	found := false
	for _, w := range report.Warnings {
		if w == "MISSING_VAR_X" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected warning for MISSING_VAR_X, got %v", report.Warnings)
	}
}

func TestValidateUnsetVarWithDefaultNoWarning(t *testing.T) {
	os.Unsetenv("UNSET_WITH_DEFAULT")
	writeSettings(t, `{"provider":{"openai":{"api_key":"${UNSET_WITH_DEFAULT:-sk-fallback}"}}}`)
	report, err := ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	for _, w := range report.Warnings {
		if w == "UNSET_WITH_DEFAULT" {
			t.Errorf("var with default should not warn, got %v", report.Warnings)
		}
	}
}

func TestValidateRawModeParseError(t *testing.T) {
	// ${PORT} = "abc" in raw mode → ExpandBytes produces invalid JSON
	// ({"server":{"port": abc}}) → Unmarshal fails.
	withEnv(t, map[string]string{"PORT": "abc"})
	writeSettings(t, `{"server":{"port": ${PORT}}}`)
	_, err := ValidateSettings()
	if err == nil {
		t.Fatal("expected parse error for raw-mode non-numeric token, got nil")
	}
}

func TestValidateRawModeIntOK(t *testing.T) {
	withEnv(t, map[string]string{"PORT": "9090"})
	writeSettings(t, `{"server":{"port": ${PORT}}}`)
	report, err := ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	if len(report.EnumViolations) != 0 {
		t.Errorf("enum violations = %v, want none", report.EnumViolations)
	}
}

func TestValidateEnumViolationLogLevel(t *testing.T) {
	writeSettings(t, `{"log":{"level":"verbose"}}`)
	report, err := ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v (enum violations are not parse errors)", err)
	}
	found := false
	for _, v := range report.EnumViolations {
		if containsStr(v, "log.level") && containsStr(v, "verbose") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected log.level enum violation, got %v", report.EnumViolations)
	}
}

func TestValidateEnumViolationMcpType(t *testing.T) {
	writeSettings(t, `{"mcp_servers":{"bad":{"command":"x","type":"remote"}}}`)
	report, err := ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	found := false
	for _, v := range report.EnumViolations {
		if containsStr(v, "mcp_servers.bad.type") && containsStr(v, "remote") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected mcp type enum violation, got %v", report.EnumViolations)
	}
}

func TestValidateEnumViolationMultiple(t *testing.T) {
	writeSettings(t, `{"log":{"level":"BOGUS"},"sandbox":{"network":"weird"},"telemetry":{"protocol":"ftp"}}`)
	report, err := ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	if len(report.EnumViolations) < 3 {
		t.Errorf("expected ≥3 violations, got %d: %v", len(report.EnumViolations), report.EnumViolations)
	}
}

func TestValidateEnumEmptyIsValid(t *testing.T) {
	// Empty enum values mean "use default" — valid, no violation.
	writeSettings(t, `{"log":{"level":""},"sandbox":{"network":""}}`)
	report, err := ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	if len(report.EnumViolations) != 0 {
		t.Errorf("empty enums should be valid, got %v", report.EnumViolations)
	}
}

func TestValidateEnumCaseInsensitiveForLogAndTelemetry(t *testing.T) {
	// log.level and telemetry.protocol use case-INSENSITIVE validation
	// (their use-time parsers do strings.ToLower). "DEBUG" and "GRPC" are valid.
	writeSettings(t, `{"log":{"level":"DEBUG"},"telemetry":{"endpoint":"localhost:4318","protocol":"GRPC"}}`)
	report, err := ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	if len(report.EnumViolations) != 0 {
		t.Errorf("DEBUG/GRPC should be valid (case-insensitive), got %v", report.EnumViolations)
	}
}

func TestValidateEnumCaseSensitiveForMcpDefaultModeSandbox(t *testing.T) {
	// mcp_servers.*.type, default_mode, tui.mode, sandbox.network use
	// case-SENSITIVE validation (use-time does direct == / switch without
	// ToLower). "HTTP", "AUTO", "ISOLATED" are violations — they would
	// silently fall back to defaults at use-time.
	cases := []struct {
		name string
		cfg  string
		want string // substring of the violation message
	}{
		{"mcp type uppercase", `{"mcp_servers":{"x":{"command":"c","type":"HTTP"}}}`, "mcp_servers.x.type"},
		{"default_mode uppercase", `{"default_mode":"AUTO"}`, "default_mode"},
		{"tui mode uppercase", `{"tui":{"mode":"PLAN"}}`, "tui.mode"},
		{"sandbox network uppercase", `{"sandbox":{"network":"ISOLATED"}}`, "sandbox.network"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeSettings(t, c.cfg)
			report, err := ValidateSettings()
			if err != nil {
				t.Fatalf("ValidateSettings: %v", err)
			}
			found := false
			for _, v := range report.EnumViolations {
				if containsStr(v, c.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("expected violation for %s, got %v", c.want, report.EnumViolations)
			}
		})
	}
}

func TestCheckEnumsDirect(t *testing.T) {
	// Unit test CheckEnums without going through ValidateSettings.
	cfg := &Config{
		Log:     LogConfig{Level: "info"},
		Sandbox: SandboxConfig{Network: "host"},
		McpServers: map[string]McpServerConfig{
			"ok":  {Type: "stdio"},
			"bad": {Type: "remote"},
		},
	}
	v := CheckEnums(cfg)
	if len(v) != 1 {
		t.Fatalf("expected 1 violation, got %d: %v", len(v), v)
	}
	if !containsStr(v[0], "mcp_servers.bad.type") {
		t.Errorf("violation = %q, want mcp_servers.bad.type", v[0])
	}
}

func TestCheckEnumsAllValid(t *testing.T) {
	cfg := &Config{
		Log:         LogConfig{Level: "trace"},
		DefaultMode: "auto",
		TUI:         TUIConfig{Mode: "plan"},
		Sandbox:     SandboxConfig{Network: "isolated"},
		Telemetry:   TelemetryConfig{Protocol: "grpc"},
		McpServers: map[string]McpServerConfig{
			"s1": {Type: "http"},
			"s2": {Type: "sse"},
		},
	}
	v := CheckEnums(cfg)
	if len(v) != 0 {
		t.Errorf("expected 0 violations, got %v", v)
	}
}

// ---- regression: TUI.Mode derived from DefaultMode not double-reported ----

// TestValidateTuiModeNotDuplicatedFromDefaultMode verifies that when only
// default_mode is set (and invalid), CheckEnums reports ONE violation
// (default_mode), not two. ApplyDefaults derives TUI.Mode from DefaultMode,
// so the old code re-reported the same invalid value under tui.mode.
func TestValidateTuiModeNotDuplicatedFromDefaultMode(t *testing.T) {
	writeSettings(t, `{"default_mode":"BOGUS"}`)
	report, err := ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	if len(report.EnumViolations) != 1 {
		t.Errorf("expected 1 violation (default_mode only), got %d: %v",
			len(report.EnumViolations), report.EnumViolations)
	}
}

// TestValidateTuiModeDirectViolation verifies that when the user writes
// tui.mode directly (and it's invalid), it IS reported.
func TestValidateTuiModeDirectViolation(t *testing.T) {
	writeSettings(t, `{"tui":{"mode":"BOGUS"}}`)
	report, err := ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	found := false
	for _, v := range report.EnumViolations {
		if containsStr(v, "tui.mode") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected tui.mode violation, got %v", report.EnumViolations)
	}
}

// containsStr is a tiny helper for substring checks in tests (avoids
// pulling in strings just for one call).
func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && indexOfStr(s, sub) >= 0
}

func indexOfStr(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// ---- regression: map iteration order deterministic (problem E) ----

// TestCheckEnumsMapOrderDeterministic verifies that CheckEnums returns
// mcp_servers violations in sorted key order, not random map-iteration
// order. This makes `settings validate` output and logs stable.
func TestCheckEnumsMapOrderDeterministic(t *testing.T) {
	cfg := &Config{
		McpServers: map[string]McpServerConfig{
			"gamma": {Type: "bad3"},
			"alpha": {Type: "bad1"},
			"beta":  {Type: "bad2"},
		},
	}
	// Run multiple times — order must be stable and sorted.
	var first []string
	for i := 0; i < 10; i++ {
		v := CheckEnums(cfg)
		if i == 0 {
			first = v
		} else {
			for j := range v {
				if j >= len(first) || v[j] != first[j] {
					t.Fatalf("run %d: order changed: %v vs %v", i, first, v)
				}
			}
		}
	}
	// Verify sorted: alpha before beta before gamma.
	if len(first) < 3 {
		t.Fatalf("expected 3 violations, got %d: %v", len(first), first)
	}
	if !containsStr(first[0], "alpha") || !containsStr(first[1], "beta") || !containsStr(first[2], "gamma") {
		t.Errorf("expected alpha/beta/gamma order, got %v", first)
	}
}

// ---- regression: slice of struct walked (problem D) ----

// TestCheckEnumsSliceWalked verifies that walkEnums descends into
// []Struct fields (e.g. ProviderConfig.Models []ModelConfig). ModelConfig
// currently has no valid-tagged fields, so the test verifies no panic and
// correct descent — if a valid tag is added to ModelConfig later, it will
// be checked automatically.
func TestCheckEnumsSliceWalked(t *testing.T) {
	cfg := &Config{
		Provider: map[string]ProviderConfig{
			"test": {Models: []ModelConfig{
				{ID: "m1"},
				{ID: "m2"},
			}},
		},
	}
	// Must not panic — slice elements are walked.
	v := CheckEnums(cfg)
	// No violations expected (ModelConfig has no valid tags yet).
	if len(v) != 0 {
		t.Errorf("expected 0 violations, got %v", v)
	}
}

// ---- secret key detection (tag-driven, reflect-based) ----

// TestIsSecretKey verifies that IsSecretKey correctly identifies fields tagged
// `sensitive:"true"` via reflection, including map-key wildcards. Adding a
// new secret field with the tag automatically makes it recognized — no edits
// to settings_tool.go needed.
func TestIsSecretKey(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Secret fields (tagged sensitive:"true"):
		{"provider.openai.api_key", true},
		{"provider.glm.api_key", true},
		{"provider.anyname.api_key", true},
		{"openviking.api_key", true},
		{"embedding.api_key", true},
		{"channels.feishu.app_secret", true},
		{"channels.wechat.token", true},
		{"channels.wecom.secret", true},
		// Non-secret fields:
		{"provider.openai.base_url", false},
		{"provider.openai.models", false},
		{"log.level", false},
		{"server.port", false},
		{"channels.feishu.app_id", false},
		{"telemetry.endpoint", false},
		{"telemetry.protocol", false},
		{"sandbox.network", false},
	}
	for _, c := range cases {
		got := IsSecretKey(c.path)
		if got != c.want {
			t.Errorf("IsSecretKey(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestSecretPathsCollected verifies all 6 tagged secret fields are discovered.
func TestSecretPathsCollected(t *testing.T) {
	paths := collectSecretPaths(reflect.TypeOf(Config{}), "")
	wantPaths := []string{
		"provider.*.api_key",
		"channels.feishu.app_secret",
		"channels.wechat.token",
		"channels.wecom.secret",
		"embedding.api_key",
		"openviking.api_key",
	}
	for _, w := range wantPaths {
		found := false
		for _, p := range paths {
			if p == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected secret path %q in collected paths: %v", w, paths)
		}
	}
	if len(paths) != len(wantPaths) {
		t.Errorf("expected %d secret paths, got %d: %v", len(wantPaths), len(paths), paths)
	}
}
