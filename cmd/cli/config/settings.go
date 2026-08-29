package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// settingsMu serializes read-modify-write cycles on the settings file
// process-wide. Every writer (feishu credentials, future settings
// commands) goes through UpdateSettings, so concurrent updates never
// lose each other's fields. The file write itself is atomic (temp +
// rename); the lock protects the whole load-edit-store cycle.
var settingsMu sync.Mutex

// UpdateSettings reads the settings file, applies fn to its JSON map,
// and writes it back atomically. Unknown and unrelated fields are
// preserved verbatim (the map is only touched by fn). Concurrent-safe.
//
// fn must only mutate the map passed to it. Returning an error aborts
// the update (nothing is written).
func UpdateSettings(fn func(raw map[string]json.RawMessage) error) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()

	raw := map[string]json.RawMessage{}
	if data, rerr := os.ReadFile(Path()); rerr == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("settings parse: %w", err)
		}
	} else if !os.IsNotExist(rerr) {
		return fmt.Errorf("settings read: %w", rerr)
	}
	if err := fn(raw); err != nil {
		return err
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("settings marshal: %w", err)
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return fmt.Errorf("settings dir: %w", err)
	}
	tmp, err := os.CreateTemp(Dir(), "settings-*.tmp")
	if err != nil {
		return fmt.Errorf("settings temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("settings write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("settings close: %w", err)
	}
	if err := os.Rename(tmpName, Path()); err != nil {
		return fmt.Errorf("settings save: %w", err)
	}
	return nil
}

// ── settings set/get/list/append/delete ──

// SetSetting sets a nested key in settings.json using dotted-path notation
// (e.g. "telemetry.endpoint", "provider.openai.api_key",
// "provider.custom.models.0.id"). Numeric segments index into arrays.
// Creates intermediate objects/arrays when they don't exist. The value is
// parsed as JSON when valid (numbers, bools, objects, arrays); otherwise
// treated as a plain string. Atomic write via UpdateSettings.
func SetSetting(key, value string) error {
	var val any
	if err := json.Unmarshal([]byte(value), &val); err != nil {
		val = value
	}
	path := strings.Split(key, ".")
	return mutateSettings(func(doc map[string]any) error {
		return setNestedAny(doc, path, val)
	})
}

// AppendSetting appends a value to the array at the given dotted path.
// The array is created if it doesn't exist. The value is parsed as JSON
// when valid; otherwise treated as a plain string.
func AppendSetting(key, value string) error {
	var val any
	if err := json.Unmarshal([]byte(value), &val); err != nil {
		val = value
	}
	path := strings.Split(key, ".")
	return mutateSettings(func(doc map[string]any) error {
		return appendNested(doc, path, val)
	})
}

// DeleteSetting removes a key or array element at the given dotted path.
// Deleting an array element shifts subsequent elements down. Deleting a
// non-existent path is a no-op (not an error).
func DeleteSetting(key string) error {
	path := strings.Split(key, ".")
	return mutateSettings(func(doc map[string]any) error {
		deleteNestedAny(doc, path)
		return nil
	})
}

// deleteNestedAny removes the element at path from the JSON tree. For
// array elements, it returns the truncated slice so the parent can
// re-assign. For map keys, deletion is in-place. No-op if the path
// doesn't exist.
func deleteNestedAny(node any, path []string) (any, bool) {
	if len(path) == 0 {
		return node, false
	}
	seg := path[0]
	if len(path) == 1 {
		switch n := node.(type) {
		case map[string]any:
			if _, ok := n[seg]; ok {
				delete(n, seg)
				return n, true
			}
			return n, false
		case []any:
			idx, err := arrayIndex(seg, n)
			if err != nil {
				return n, false
			}
			return append(n[:idx], n[idx+1:]...), true
		default:
			return node, false
		}
	}
	switch n := node.(type) {
	case map[string]any:
		child, ok := n[seg]
		if !ok {
			return n, false
		}
		newChild, changed := deleteNestedAny(child, path[1:])
		if changed {
			n[seg] = newChild
		}
		return n, changed
	case []any:
		idx, err := arrayIndex(seg, n)
		if err != nil {
			return n, false
		}
		newChild, changed := deleteNestedAny(n[idx], path[1:])
		if changed {
			n[idx] = newChild
		}
		return n, changed
	default:
		return node, false
	}
}

// GetSetting reads a nested key from the on-disk settings.json (not the
// in-memory merged config — plugin-injected values are not visible here).
// Returns the value as a pretty-printed JSON string. Numeric segments
// index into arrays.
func GetSetting(key string) (string, error) {
	raw, err := os.ReadFile(Path())
	if err != nil {
		return "", fmt.Errorf("settings read: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", fmt.Errorf("settings parse: %w", err)
	}
	path := strings.Split(key, ".")
	val, ok := getNestedAny(data, path)
	if !ok {
		return "", fmt.Errorf("key %q not found", key)
	}
	b, err := json.MarshalIndent(val, "", "  ")
	if err != nil {
		return "", fmt.Errorf("settings get: marshal: %w", err)
	}
	return string(b), nil
}

// ListSettings returns the full on-disk settings.json as pretty-printed
// JSON.
func ListSettings() (string, error) {
	raw, err := os.ReadFile(Path())
	if err != nil {
		return "", fmt.Errorf("settings read: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", fmt.Errorf("settings parse: %w", err)
	}
	if len(data) == 0 {
		return "{}", nil
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", fmt.Errorf("settings list: marshal: %w", err)
	}
	return string(b), nil
}

// ── internal: any-based JSON tree navigation ──

// mutateSettings is the shared read-mutate-write cycle for set/append/delete.
// It deserializes settings.json into map[string]any, applies fn, and writes
// back atomically via UpdateSettings.
func mutateSettings(fn func(doc map[string]any) error) error {
	return UpdateSettings(func(raw map[string]json.RawMessage) error {
		// Deserialize raw map into any tree.
		data, err := json.Marshal(raw)
		if err != nil {
			return fmt.Errorf("settings: marshal doc: %w", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("settings: parse doc: %w", err)
		}
		if err := fn(doc); err != nil {
			return err
		}
		// Re-serialize back to map[string]json.RawMessage.
		newData, err := json.Marshal(doc)
		if err != nil {
			return fmt.Errorf("settings: marshal result: %w", err)
		}
		// Validate BEFORE writing: the mutated doc must still parse into
		// Config without type errors (e.g. log.level must stay a string,
		// sandbox.enabled a bool). A type mismatch here means the mutation
		// would produce a config that fails to reload (degraded defaults)
		// or fails to start on next boot (log.Fatalf). Reject it now so
		// the on-disk file is never left in a semantically broken state.
		// Unknown keys are tolerated (Config uses omitempty + no DisallowUnknownFields).
		var probe Config
		if err := json.Unmarshal(newData, &probe); err != nil {
			return fmt.Errorf("settings: validation failed (would break config parse): %w", err)
		}
		return json.Unmarshal(newData, &raw)
	})
}

// isNumeric reports whether s is a non-negative integer string.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// arrayIndex parses seg as an array index and bounds-checks it.
func arrayIndex(seg string, arr []any) (int, error) {
	idx, err := strconv.Atoi(seg)
	if err != nil {
		return 0, fmt.Errorf("expected array index, got %q", seg)
	}
	if idx < 0 || idx >= len(arr) {
		return 0, fmt.Errorf("array index %d out of range (len %d)", idx, len(arr))
	}
	return idx, nil
}

// newChildFor decides whether to create a map or slice for the next path
// segment. Numeric → slice, otherwise → map.
func newChildFor(nextSeg string) any {
	if isNumeric(nextSeg) {
		return []any{}
	}
	return map[string]any{}
}

// setNestedAny recursively navigates the path in a deserialized JSON tree
// (map[string]any / []any / scalars) and sets the final key. Numeric
// segments index into arrays (out-of-range → error). Non-numeric segments
// navigate/create objects.
func setNestedAny(node any, path []string, val any) error {
	if len(path) == 0 {
		return fmt.Errorf("empty key path")
	}
	seg := path[0]
	if len(path) == 1 {
		switch n := node.(type) {
		case map[string]any:
			n[seg] = val
			return nil
		case []any:
			idx, err := arrayIndex(seg, n)
			if err != nil {
				return err
			}
			n[idx] = val
			return nil
		default:
			return fmt.Errorf("cannot set %q on %T", seg, node)
		}
	}
	switch n := node.(type) {
	case map[string]any:
		child, ok := n[seg]
		if !ok {
			child = newChildFor(path[1])
			n[seg] = child
		}
		return setNestedAny(child, path[1:], val)
	case []any:
		idx, err := arrayIndex(seg, n)
		if err != nil {
			return err
		}
		return setNestedAny(n[idx], path[1:], val)
	default:
		return fmt.Errorf("cannot navigate into %q on %T", seg, node)
	}
}

// appendNested navigates to the parent of the final path segment, then
// appends val to the array at that segment. Creates the array (and
// intermediate objects) if they don't exist. Writes back through the
// parent so the append persists in the JSON tree.
func appendNested(node any, path []string, val any) error {
	if len(path) == 0 {
		return fmt.Errorf("empty key path")
	}
	seg := path[0]
	if len(path) == 1 {
		// Final segment: append to the array at this key.
		switch n := node.(type) {
		case map[string]any:
			existing, ok := n[seg]
			if ok {
				arr, ok := existing.([]any)
				if !ok {
					return fmt.Errorf("key %q is not an array", seg)
				}
				n[seg] = append(arr, val)
				return nil
			}
			// Create new array with the value.
			n[seg] = []any{val}
			return nil
		default:
			return fmt.Errorf("cannot append to %q on %T", seg, node)
		}
	}
	// Intermediate: navigate or create, then recurse.
	switch n := node.(type) {
	case map[string]any:
		child, ok := n[seg]
		if !ok {
			child = newChildFor(path[1])
			n[seg] = child
		}
		if err := appendNested(child, path[1:], val); err != nil {
			return err
		}
		n[seg] = child
		return nil
	case []any:
		idx, err := arrayIndex(seg, n)
		if err != nil {
			return err
		}
		if err := appendNested(n[idx], path[1:], val); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("cannot navigate into %q on %T", seg, node)
	}
}

// getNestedAny recursively navigates the path in a deserialized JSON tree.
// Supports numeric segments for array indexing. Returns (value, true) if
// found, (nil, false) otherwise.
func getNestedAny(node any, path []string) (any, bool) {
	if len(path) == 0 {
		return nil, false
	}
	seg := path[0]
	switch n := node.(type) {
	case map[string]any:
		v, ok := n[seg]
		if !ok {
			return nil, false
		}
		if len(path) == 1 {
			return v, true
		}
		return getNestedAny(v, path[1:])
	case []any:
		idx, err := arrayIndex(seg, n)
		if err != nil {
			return nil, false
		}
		if len(path) == 1 {
			return n[idx], true
		}
		return getNestedAny(n[idx], path[1:])
	default:
		return nil, false
	}
}
