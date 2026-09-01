package config

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
)

// ValidationReport holds the result of ValidateSettings. Warnings are
// informational (env vars referenced without a default that are unset —
// the config still parses, but the value is empty). EnumViolations are
// soft errors (a field has a value outside its valid set — the config
// parses, but use-time silently downgrades to a default).
type ValidationReport struct {
	// Warnings lists env-var names referenced without a default that were
	// unset or empty in the environment. The config still parses; the
	// affected fields resolve to empty values.
	Warnings []string
	// EnumViolations lists fields whose value is outside the valid set
	// (e.g. log.level="verbose"). The config parses, but use-time silently
	// falls back to a default. Each entry is a human-readable string:
	// "field: \"value\" is not one of a|b|c".
	EnumViolations []string
}

// ValidateSettings reads the on-disk settings.json, runs the same
// ExpandBytes → json.Unmarshal → ApplyDefaults pipeline as startup
// (main.go) and reload (shared.go), and checks enum-like fields that
// json.Unmarshal accepts but use-time silently downgrades. Returns a report
// (warnings + enum violations) and/or a hard parse error.
//
// Read-only: never writes to disk, never resolves secrets to disk. The
// on-disk file stays literal. Missing file = valid (defaults apply).
func ValidateSettings() (*ValidationReport, error) {
	raw, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return &ValidationReport{}, nil // no file = defaults, valid
		}
		return nil, fmt.Errorf("read settings: %w", err)
	}
	// Same pipeline as startup (main.go:104) and reload (shared.go:633).
	expanded, warns := ExpandBytes(raw)
	var cfg Config
	if err := json.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parse settings: %w", err)
	}
	ApplyDefaults(&cfg, Path())
	return &ValidationReport{
		Warnings:       warns,
		EnumViolations: CheckEnums(&cfg),
	}, nil
}

// CheckEnums inspects the enum-like string fields in cfg and returns a list
// of violations — fields whose non-empty value is outside the valid set.
// Empty values are valid (they mean "use the default"). Exported so the
// reload path (shared.go) can gate on it without re-running the full
// ExpandBytes+Unmarshal pipeline (reload already has the parsed Config).
//
// TAG-DRIVEN: enum fields are declared via a `valid` struct tag on the field
// itself, e.g.:
//
//	Level string `json:"level,omitempty" valid:"enum=trace|debug|info|warn|error;case=ci"`
//
// The tag syntax is `key=val;key=val`:
//   - enum=a|b|c: the valid values (pipe-separated).
//   - case=ci (case-insensitive) or case=cs (case-sensitive): whether the
//     comparison lowercases the value. Use ci when the use-time parser does
//     strings.ToLower (so "DEBUG" == "debug"); use cs when it compares
//     directly (so "AUTO" would silently fall back and must be flagged).
//   - skipif=field: skip this field if its value equals the sibling field's
//     value (cross-field dedup — e.g. tui.mode is derived from default_mode
//     by ApplyDefaults, so skipif=default_mode avoids double-reporting).
//
// Adding a new enum field requires ONLY adding a `valid` tag to the struct
// field — CheckEnums discovers it automatically via reflection. No edits to
// validate.go needed.
func CheckEnums(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	root := reflect.ValueOf(cfg).Elem()
	var violations []string
	walkEnums(root, root, "", &violations)
	return violations
}

// walkEnums recursively walks a reflect.Value, finding string fields with a
// `valid` struct tag and validating them. It descends into nested structs
// and map[string]Struct values (for mcp_servers). path accumulates the
// dotted path for human-readable violation messages. root is the top-level
// Config value, passed so skipif can look up sibling fields at any depth
// (e.g. tui.mode is in Config.TUI but its skipif target default_mode is in
// Config — a cross-struct sibling lookup).
func walkEnums(v, root reflect.Value, path string, violations *[]string) {
	// Dereference pointers.
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			fieldVal := v.Field(i)
			// Build the dotted path: use the json tag name if present,
			// else the Go field name.
			jsonName := jsonTagName(field.Tag.Get("json"))
			seg := jsonName
			if seg == "" {
				seg = field.Name
			}
			childPath := seg
			if path != "" {
				childPath = path + "." + seg
			}

			// Check if this field has a `valid` tag.
			validTag := field.Tag.Get("valid")
			if validTag != "" && fieldVal.Kind() == reflect.String {
				checkEnumField(fieldVal.String(), validTag, childPath, root, violations)
			}
			// Recurse into nested structs (even if they also have a valid
			// tag — a struct can't be both a string enum and a container,
			// but the recurse is harmless for non-struct fields).
			walkEnums(fieldVal, root, childPath, violations)
		}

	case reflect.Map:
		// map[string]Struct (e.g. McpServers map[string]McpServerConfig).
		// Each value is a struct — walk it with the key as the path segment.
		// Sort keys for deterministic output order (map iteration is random
		// in Go; sorted keys make `settings validate` output and logs stable).
		keys := make([]string, 0, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			keys = append(keys, iter.Key().String())
		}
		sortStrings(keys)
		for _, k := range keys {
			elem := v.MapIndex(reflect.ValueOf(k))
			childPath := fmt.Sprintf("%s.%s", path, k)
			walkEnums(elem, root, childPath, violations)
		}

	case reflect.Slice, reflect.Array:
		// []Struct (e.g. ProviderConfig.Models []ModelConfig). Walk each
		// element with its index as the path segment, so a valid-tagged
		// field inside a slice element is checked (not silently skipped).
		for i := 0; i < v.Len(); i++ {
			childPath := fmt.Sprintf("%s.%d", path, i)
			walkEnums(v.Index(i), root, childPath, violations)
		}
	}
}

// checkEnumField validates a single string field against its `valid` tag.
// The tag is `key=val;key=val` with keys: enum, case, skipif. root is the
// top-level Config for cross-struct skipif sibling lookups.
func checkEnumField(val, tag, path string, root reflect.Value, violations *[]string) {
	rules := parseValidTag(tag)

	// skipif: if this field's value equals the sibling field's value (looked
	// up from the root Config), skip — cross-field dedup. e.g. tui.mode is
	// derived from default_mode by ApplyDefaults, so skipif=default_mode
	// avoids double-reporting when the user only wrote default_mode.
	if skipField := rules["skipif"]; skipField != "" {
		sibling := root.FieldByName(toExported(skipField))
		if sibling.IsValid() && sibling.Kind() == reflect.String && sibling.String() == val {
			return // derived value — skip to avoid double-reporting
		}
	}

	// Empty = use default = valid.
	if val == "" {
		return
	}

	enumStr := rules["enum"]
	if enumStr == "" {
		return // no enum rule — nothing to check
	}
	validSet := strings.Split(enumStr, "|")

	caseMode := rules["case"]
	matched := false
	if caseMode == "ci" {
		lower := strings.ToLower(val)
		for _, v := range validSet {
			if lower == v {
				matched = true
				break
			}
		}
	} else {
		// case=cs (default): direct comparison.
		for _, v := range validSet {
			if val == v {
				matched = true
				break
			}
		}
	}
	if !matched {
		*violations = append(*violations,
			fmt.Sprintf("%s: %q is not one of %s", path, val, enumStr))
	}
}

// parseValidTag parses a `valid` tag value "key=val;key=val" into a map.
func parseValidTag(tag string) map[string]string {
	m := make(map[string]string)
	for _, part := range strings.Split(tag, ";") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return m
}

// jsonTagName extracts the name from a json tag like "level,omitempty".
// Returns "" for "-" (explicitly unexported) or empty.
func jsonTagName(jsonTag string) string {
	if jsonTag == "" || jsonTag == "-" {
		return ""
	}
	parts := strings.Split(jsonTag, ",")
	return parts[0]
}

// toExported converts a snake_case or lowercase field name to the exported
// Go field name (e.g. "default_mode" → "DefaultMode"). Used for skipif
// sibling lookup. Handles the known field names; unknown names return the
// input Title-cased as a fallback.
func toExported(s string) string {
	// Common skipif targets are declared as exported Go fields; the valid
	// tag uses the json name (default_mode). Map known json names → Go names.
	switch s {
	case "default_mode":
		return "DefaultMode"
	case "level":
		return "Level"
	case "mode":
		return "Mode"
	case "protocol":
		return "Protocol"
	case "network":
		return "Network"
	case "type":
		return "Type"
	}
	// Fallback: Title-case the first letter.
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// IsSecretKey reports whether the dotted path (e.g. "provider.openai.api_key")
// targets a field tagged `sensitive:"true"` in the Config struct tree. Used by
// the settings tool to detect when the agent is writing a secret and prompt it
// to use ${ENV_VAR} instead of a literal value.
//
// TAG-DRIVEN: secret fields are declared via a `sensitive:"true"` struct tag,
// e.g.:
//
//	APIKey string `json:"api_key" sensitive:"true"`
//
// Adding a new secret field requires ONLY adding the tag — IsSecretKey
// discovers it automatically via reflection, mirroring CheckEnums. No edits
// to settings_tool.go needed.
//
// The path may use wildcards for map keys (* matches any key) so callers can
// check "provider.*.api_key" patterns. But the common case is a concrete path
// from the settings tool (e.g. "provider.glm.api_key"), which is matched
// directly. Map keys in the path match any key at that position.
func IsSecretKey(path string) bool {
	secretPaths := collectSecretPaths(reflect.TypeOf(Config{}), "")
	for _, sp := range secretPaths {
		if pathMatchesPattern(path, sp) {
			return true
		}
	}
	return false
}

// collectSecretPaths walks the Config TYPE tree (not values — so nil maps
// and empty slices are still traversed via their element types) and returns
// the dotted JSON paths of all fields with `sensitive:"true"`. Map and slice
// fields use "*" as the key/index wildcard (e.g. "provider.*.api_key").
func collectSecretPaths(t reflect.Type, path string) []string {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	var paths []string
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			jsonName := jsonTagName(field.Tag.Get("json"))
			seg := jsonName
			if seg == "" {
				seg = field.Name
			}
			childPath := seg
			if path != "" {
				childPath = path + "." + seg
			}
			if field.Tag.Get("sensitive") == "true" && field.Type.Kind() == reflect.String {
				paths = append(paths, childPath)
			}
			paths = append(paths, collectSecretPaths(field.Type, childPath)...)
		}
	case reflect.Map:
		// Use "*" for the map key, descend into the element type.
		paths = append(paths, collectSecretPaths(t.Elem(), path+".*")...)
	case reflect.Slice, reflect.Array:
		paths = append(paths, collectSecretPaths(t.Elem(), path+".*")...)
	}
	return paths
}

// pathMatchesPattern checks if a concrete path (e.g. "provider.glm.api_key")
// matches a pattern that may contain "*" wildcards (e.g. "provider.*.api_key").
func pathMatchesPattern(path, pattern string) bool {
	pathParts := strings.Split(path, ".")
	patternParts := strings.Split(pattern, ".")
	if len(pathParts) != len(patternParts) {
		return false
	}
	for i := range pathParts {
		if patternParts[i] != "*" && patternParts[i] != pathParts[i] {
			return false
		}
	}
	return true
}
