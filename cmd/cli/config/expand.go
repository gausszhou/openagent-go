package config

import (
	"bytes"
	"os"
	"strings"
)

// rawSentinel marks an unquoted (raw-mode) env-var reference (${VAR} / $VAR)
// that NormalizeRawRefs has temporarily quoted so it can travel through
// encoding/json. DenormalizeRawRefs strips the sentinel + quotes to restore
// the raw token before writing to disk.
//
// The sentinel is the two-code-point sequence U+E000 U+E001 (Unicode Private
// Use Area). As UTF-8 it is the 6 bytes 0xEE 0x80 0x80 0xEE 0x80 0x81.
// json.Marshal emits these as raw UTF-8 inside a quoted string (not as
// \uXXXX escapes — verified by a round-trip probe), so the sequence survives
// a full marshal→unmarshal→marshal byte-identical, which is what makes the
// RMW work.
//
// Collision safety: a user string value starting with these two PUA code
// points followed by a ${...} or $VAR token would be misidentified as a
// normalized raw token and stripped. This requires the user to write a string
// whose value begins with U+E000 U+E001 immediately followed by "${" or a
// "$"+name-start byte — a sequence that does not occur in any realistic
// config value (API keys, URLs, paths, model names, font data, or plugin
// metadata). DenormalizeRawRefs additionally validates that the bytes after
// the sentinel form a complete ${...} or $VAR token before stripping, so a
// user PUA value that does not match this exact structure is preserved
// verbatim. The two-code-point form (vs a single U+E000) makes an accidental
// collision astronomically unlikely and is distinguishable from single-PUA
// font glyphs that do appear in real data.
const rawSentinel = "\ue000\ue001"

// ExpandBytes resolves environment-variable references in JSON data and
// returns a new byte slice with the references replaced by their resolved
// values. It is JSON-string-aware and follows the Docker-Compose pattern
// (adapted to JSON):
//
//	"${VAR}"   (inside quotes) → the value, JSON-escaped, as a string.
//	${VAR}     (outside quotes) → the value as a raw JSON token, so a
//	           numeric/bool/null env value becomes a native int/bool/null
//	           in the parsed config. The user conveys the intended type by
//	           quoting, exactly as Compose users do in YAML.
//
// Supported syntax:
//
//	${VAR}           os.Getenv("VAR"); empty if unset
//	${VAR:-default}  VAR if set and non-empty, else "default" — the default
//	                 is recursively resolved if it contains ${...} refs
//	                 (e.g. ${OUTER:-${INNER}} → INNER's value when OUTER
//	                 is empty), matching Docker Compose semantics.
//	$VAR             same as ${VAR} (bare form; name is [A-Za-z_][A-Za-z0-9_]*)
//	$$               a literal "$"
//
// Substitution rules:
//
//   - Inside a JSON string literal, the resolved value is JSON-escaped, so a
//     value containing quotes, backslashes, or newlines (e.g. a multi-line
//     PEM private key) cannot break the surrounding JSON.
//   - Outside a string literal, the resolved value is written as raw bytes —
//     a native JSON token (number/bool/null). A non-numeric/bool/null value
//     (e.g. PORT=abc used as ${PORT}) produces malformed JSON and
//     json.Unmarshal fails with a clear error. This is fail-safe: no shell,
//     no silent corruption.
//
// warnings lists the names of variables that were referenced without a
// default and were unset or empty in the environment. Each name appears at
// most once. Callers decide whether to log them: startup logs once; hot
// reload suppresses to avoid spam on every file touch.
//
// SECURITY: after json.Unmarshal of the expanded bytes, the resulting Config
// holds RESOLVED secrets (API keys, tokens) in memory. Do not log the Config
// struct or its string fields at debug level. The on-disk file stays literal
// — GetSetting/ListSettings return literal ${...} references (never resolved
// secrets), and UpdateSettings writes literal bytes back to disk. The
// validation probe in mutateSettings DOES call ExpandBytes, but only on the
// in-memory mutated doc (never writes the result to disk) to verify the
// resolved config would parse into Config.
func ExpandBytes(data []byte) (expanded []byte, warnings []string) {
	if len(data) == 0 {
		return data, nil
	}
	var (
		out      strings.Builder
		inString bool
		missing  = map[string]struct{}{}
	)
	out.Grow(len(data)) // preallocate for efficiency
	i := 0
	for i < len(data) {
		c := data[i]

		// Track whether we are inside a JSON string literal. A backslash
		// escapes the next byte, so \" does not close the string. We copy
		// escape sequences verbatim — only the string/open state matters for
		// deciding how to emit a substitution.
		if c == '\\' && inString && i+1 < len(data) {
			out.WriteByte(c)
			out.WriteByte(data[i+1])
			i += 2
			continue
		}
		if c == '"' {
			inString = !inString
			out.WriteByte(c)
			i++
			continue
		}

		if c != '$' {
			out.WriteByte(c)
			i++
			continue
		}

		// c == '$'. Decide what follows.
		if i+1 < len(data) && data[i+1] == '$' {
			// $$ → literal $
			out.WriteByte('$')
			i += 2
			continue
		}

		if i+1 < len(data) && data[i+1] == '{' {
			// ${...} — brace form. Parse to the depth-0 closing brace.
			start := i + 2
			name, def, hasDef, end, ok := parseBraced(data, start, inString)
			if !ok {
				// No closing brace: emit "$" and continue past it (leave "{"
				// to be handled as a normal byte next iteration).
				out.WriteByte('$')
				i++
				continue
			}
			val, _, warned, nestedWarns := resolve(name, def, hasDef)
			emit(&out, val, inString)
			if warned {
				missing[name] = struct{}{}
			}
			// Surface nested-default warnings: if the default contained a
			// var ref (${OTHER}) and OTHER was unset/empty, the recursive
			// ExpandBytes inside resolve returned its name in nestedWarns.
			// Prefix with the outer name so the operator can trace the chain
			// (e.g. "(nested default of OUTER) INNER").
			for _, nw := range nestedWarns {
				missing["(nested default of "+name+") "+nw] = struct{}{}
			}
			i = end
			continue
		}

		// $VAR — bare form. Collect [A-Za-z_][A-Za-z0-9_]*.
		j := i + 1
		if j < len(data) && isNameStart(data[j]) {
			j++
			for j < len(data) && isNamePart(data[j]) {
				j++
			}
			name := string(data[i+1 : j])
			val, _, warned, _ := resolve(name, "", false)
			emit(&out, val, inString)
			if warned {
				missing[name] = struct{}{}
			}
			i = j
			continue
		}

		// Lone "$" (end of input, or followed by a non-name byte): literal.
		out.WriteByte('$')
		i++
	}

	if len(missing) == 0 {
		return []byte(out.String()), nil
	}
	warnings = make([]string, 0, len(missing))
	for k := range missing {
		warnings = append(warnings, k)
	}
	// Stable order for deterministic logs.
	sortStrings(warnings)
	return []byte(out.String()), warnings
}

// NormalizeRawRefs makes a settings.json document that contains unquoted
// (raw-mode) env-var references safe for encoding/json. Every ${...} or $VAR
// token appearing OUTSIDE a JSON string literal is wrapped in quotes and
// prefixed with rawSentinel, so `{"port": ${PORT}}` becomes
// `{"port": "<sentinel>${PORT}"}` — valid JSON that json.Unmarshal,
// map[string]json.RawMessage, json.Marshal, and the settings-tool RMW all
// handle. String-mode references ("${VAR}" inside quotes) are left untouched:
// they are already valid JSON, and quoting them again would corrupt them.
//
// This is the inverse of DenormalizeRawRefs. The pair lets the settings tool
// read/mutate/write a literal-on-disk file that contains raw-mode tokens
// without ever resolving them to secrets (disk stays literal) and without
// tripping the json tokenizer (raw ${PORT} is otherwise invalid JSON).
//
// Used at every settings-tool entry point that reads raw disk bytes.
func NormalizeRawRefs(data []byte) []byte {
	if !bytes.ContainsRune(data, '$') {
		return data // fast path: no possible references
	}
	var out strings.Builder
	out.Grow(len(data) + 16)
	inString := false
	i := 0
	for i < len(data) {
		c := data[i]

		// Respect JSON string escapes: \" (and any \x) is a two-byte run that
		// must not be split, and the escaped quote must not toggle inString.
		if c == '\\' && inString && i+1 < len(data) {
			out.WriteByte(c)
			out.WriteByte(data[i+1])
			i += 2
			continue
		}
		if c == '"' {
			inString = !inString
			out.WriteByte(c)
			i++
			continue
		}

		// Only references OUTSIDE a string need normalization. Inside a
		// string, ${VAR} is already valid JSON and must stay literal so the
		// on-disk representation is preserved exactly.
		if c != '$' || inString {
			out.WriteByte(c)
			i++
			continue
		}

		// c == '$' outside a string. Collect the reference span (${...} or
		// $VAR) so we can quote-wrap it as a single unit. We reuse parseBraced
		// (the same scanner ExpandBytes uses) for the ${...} form so the two
		// scanners can never disagree on brace boundaries — a divergence here
		// would mean a token ExpandBytes accepts but NormalizeRawRefs rejects
		// (or vice versa), producing a config that starts but can't be edited
		// via the settings tool. parseBraced is called with inString=false
		// (we only reach here outside a string).
		start := i
		end := i // exclusive
		if i+1 < len(data) && data[i+1] == '{' {
			// ${...} — parseBraced starts at the byte after '{'.
			_, _, _, brEnd, ok := parseBraced(data, i+2, false)
			if ok {
				end = brEnd
			}
		} else if i+1 < len(data) && isNameStart(data[i+1]) {
			// $VAR — bare form. Collect [A-Za-z_][A-Za-z0-9_]*.
			j := i + 2
			for j < len(data) && isNamePart(data[j]) {
				j++
			}
			end = j
		}

		if end <= start {
			// Lone '$' (end of input, or followed by a non-name byte): emit
			// literally and continue. Not a reference.
			out.WriteByte('$')
			i++
			continue
		}

		// Wrap the raw token: " <sentinel> <token bytes> "
		out.WriteByte('"')
		out.WriteString(rawSentinel)
		out.Write(data[start:end])
		out.WriteByte('"')
		i = end
	}
	return []byte(out.String())
}

// DenormalizeRawRefs inverts NormalizeRawRefs: it finds every JSON string
// value that begins with rawSentinel and replaces the whole quoted string
// (sentinel + token) with the raw token bytes, restoring the literal
// on-disk form `{"port": ${PORT}}` from the normalized in-memory form
// `{"port": "<sentinel>${PORT}"}`.
//
// The input is JSON that encoding/json produced (via Marshal/MarshalIndent),
// so sentinel-marked values appear as exactly `"<sentinel>${...}"` (sentinel
// as raw UTF-8 bytes, not \uXXXX — verified by a round-trip probe). We parse
// the JSON structure to find string values reliably rather than blind string
// replacement, which would mis-handle a sentinel that happened to appear in
// the middle of a normal string or in a key.
//
// Used before writing to disk and before the validation probe, so both see
// the true literal document with raw tokens.
func DenormalizeRawRefs(data []byte) ([]byte, error) {
	if !bytes.Contains(data, []byte(rawSentinel)) {
		return data, nil // fast path: nothing was normalized
	}
	var out bytes.Buffer
	out.Grow(len(data))
	// Walk the JSON with a minimal state machine. We only need to find
	// string *values* that start with the sentinel; we do not need to fully
	// validate the document (encoding/json already did). Keys are strings
	// too, but a sentinel-marked key is not produced by NormalizeRawRefs
	// (it only quotes values outside strings). A user who hand-writes a key
	// starting with the sentinel PUA bytes must NOT have its quotes stripped
	// (that would corrupt the file to invalid JSON). We track key-vs-value
	// position: inside an object, a string immediately following '{' or ','
	// is a key; the string after ':' is a value. Inside an array, every
	// string is a value. A sentinel-marked string is only stripped when it
	// is in VALUE position.
	i := 0
	// Stack of per-depth container kinds: false = object (expect key next
	// after '{' or ','), true = array (always value). depth 0 (top level)
	// is a sentinel "not a container" — strings here are values.
	// After consuming a key (in an object), we set the top to a transient
	// "expecting value" state via the ':' handler; after consuming a value
	// in an object, the ',' handler resets to "expect key".
	// We use a separate isObj stack to know the container type, and an
	// expectKey flag derived from it + whether we just saw ':' or ','.
	type frame struct {
		isObj     bool // true=object, false=array
		expectKey bool // in objects: next string is a key? toggled by ':'/','
	}
	stack := []frame{{isObj: false, expectKey: false}} // top level
	for i < len(data) {
		c := data[i]

		switch c {
		case '{':
			stack = append(stack, frame{isObj: true, expectKey: true})
			out.WriteByte(c)
			i++
			continue
		case '[':
			stack = append(stack, frame{isObj: false, expectKey: false})
			out.WriteByte(c)
			i++
			continue
		case '}':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			out.WriteByte(c)
			i++
			continue
		case ']':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			out.WriteByte(c)
			i++
			continue
		case ',':
			// After a comma: in an object, next is a key; in an array, a value.
			top := &stack[len(stack)-1]
			top.expectKey = top.isObj
			out.WriteByte(c)
			i++
			continue
		case ':':
			// After a colon (object only), the next string is a value.
			stack[len(stack)-1].expectKey = false
			out.WriteByte(c)
			i++
			continue
		}

		// Pass through whitespace and non-string, non-structural tokens.
		if c != '"' {
			out.WriteByte(c)
			i++
			continue
		}

		// c == '"' — start of a string (key or value). Scan to the closing
		// quote, respecting escape sequences, collecting the raw inner bytes.
		isKey := stack[len(stack)-1].expectKey
		i++ // skip opening quote
		start := i
		for i < len(data) {
			if data[i] == '\\' && i+1 < len(data) {
				i += 2
				continue
			}
			if data[i] == '"' {
				break
			}
			i++
		}
		inner := data[start:i] // bytes between the quotes, escapes intact
		i++                    // skip closing quote

		// After a key string, the next string (after ':') is a value. After
		// a value string, the ',' handler resets expectKey — no action here.
		if isKey {
			stack[len(stack)-1].expectKey = false
		}

		// Only strip sentinel-marked strings in VALUE position. A
		// sentinel-marked KEY (user typo or adversarial input) is preserved
		// verbatim — stripping its quotes would produce invalid JSON
		// ({${PORT}: "val"}).
		if !isKey && bytes.HasPrefix(inner, []byte(rawSentinel)) {
			rest := inner[len(rawSentinel):]
			if tokEnd := rawTokenEnd(rest); tokEnd == len(rest) {
				// The entire remainder is one raw token: strip the sentinel
				// and the surrounding quotes, emitting the bare token bytes
				// (e.g. ${PORT}). These are the literal on-disk form.
				out.Write(rest)
				continue
			}
			// Sentinel present but the remainder is not a complete token —
			// this is user data that collides with the sentinel prefix. Keep
			// the string verbatim (do NOT strip quotes) to avoid corrupting
			// the file into invalid JSON.
		}
		// Normal string, key, or colliding user data: re-emit quotes + inner.
		out.WriteByte('"')
		out.Write(inner)
		out.WriteByte('"')
	}
	return out.Bytes(), nil
}

// rawTokenEnd returns the index just past a complete raw-mode env-var token
// at the start of s, or 0 if s does not begin with a valid token. A token is
// either ${...} (brace form, to the depth-0 closing brace) or $VAR (bare
// form, [A-Za-z_][A-Za-z0-9_]*). This is the structural check
// DenormalizeRawRefs uses to confirm a sentinel-prefixed string is a
// normalized raw token (and not user data that merely starts with the PUA
// sentinel bytes): the bytes after the sentinel must be EXACTLY one complete
// token with nothing trailing.
func rawTokenEnd(s []byte) int {
	if len(s) == 0 || s[0] != '$' {
		return 0
	}
	if len(s) >= 2 && s[1] == '{' {
		// ${...} — find the depth-0 closing brace.
		depth := 0
		for j := 0; j < len(s); j++ {
			switch s[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return j + 1
				}
			}
		}
		return 0 // no closing brace — not a complete token
	}
	// $VAR — bare form.
	if len(s) >= 2 && isNameStart(s[1]) {
		j := 2
		for j < len(s) && isNamePart(s[j]) {
			j++
		}
		return j
	}
	return 0
}

// StripSentinel removes the rawSentinel prefix from a decoded JSON string
// value, used by GetSetting/ListSettings to return the literal on-disk token
// (e.g. "${PORT}") rather than the sentinel-marked form. The value passed in
// is a single decoded JSON string (already unquoted by the caller).
//
// As with DenormalizeRawRefs, the sentinel is only stripped when the
// remainder is a complete ${...} or $VAR token — a user string that merely
// starts with the PUA sentinel bytes (e.g. font data) is returned unchanged.
func StripSentinel(s string) string {
	if !strings.HasPrefix(s, rawSentinel) {
		return s
	}
	rest := s[len(rawSentinel):]
	if end := rawTokenEnd([]byte(rest)); end == len(rest) {
		return rest
	}
	return s // sentinel present but not a token — user data, keep verbatim
}

// parseBraced parses the content of a "${...}" expression starting at index
// start (the byte after "{"). It returns:
//   - name: the variable name
//   - def: the default value text (literal, unescaped)
//   - hasDef: whether a ":-" separator was present
//   - end: the index just past the closing "}" (where to resume scanning)
//   - ok: false if no closing "}" is found before end-of-input or (when
//     inString) before the enclosing JSON string closes; the caller then
//     emits "$" literally and re-scans the "{" as an ordinary byte.
//
// The name/default split is on the first depth-0 ":-". Brace nesting is
// tracked so "${VAR:-a}b}" parses sanely — the default extends to the
// depth-0 close. NESTED ${...} INSIDE THE DEFAULT IS RECURSIVELY RESOLVED
// by resolve (which calls ExpandBytes on the default text when it contains
// a var ref): "${OUTER:-${INNER}}" with OUTER empty yields the value of
// INNER, not the literal text "${INNER}". This matches Docker Compose
// semantics. Deeper nesting (${A:-${B:-${C}}}) works via the natural
// recursion of ExpandBytes → resolve → ExpandBytes.
//
// When inString is true, the ${...} opened inside a JSON string literal. A
// closing "}" that is not found before the JSON string terminates (i.e. a
// closing quote is reached) means the ${ was never actually closed — this is
// malformed input, so ok=false is returned and the caller emits "$"
// literally (then the "{" and following text are scanned as ordinary bytes,
// producing a faithful passthrough rather than a greedy over-read). While
// scanning inside a string, embedded \" escapes are respected so a quote
// inside the variable name/default does not terminate the scan.
func parseBraced(data []byte, start int, inString bool) (name, def string, hasDef bool, end int, ok bool) {
	depth := 1
	i := start
	sepIdx := -1 // index of the ':' of the first ":-" at depth 1
	for i < len(data) {
		c := data[i]
		// Respect JSON string escapes while scanning inside a string literal:
		// \" (and any \x) is a two-byte sequence that must not be split, and
		// the escaped quote must not terminate the enclosing string.
		if inString && c == '\\' && i+1 < len(data) {
			i += 2
			continue
		}
		// If we are inside a JSON string and hit an unescaped quote, the
		// ${...} was never closed before the string ended — treat as
		// unterminated so the caller falls back to literal "$".
		if inString && c == '"' {
			return "", "", false, 0, false
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				content := data[start:i]
				if sepIdx >= 0 {
					name = string(content[:sepIdx-start])
					def = string(content[sepIdx+2-start:])
					hasDef = true
				} else {
					name = string(content)
				}
				return name, def, hasDef, i + 1, true
			}
		case ':':
			// Look for ":-" at depth 1 only (the outermost separator).
			if depth == 1 && sepIdx < 0 && i+1 < len(data) && data[i+1] == '-' {
				sepIdx = i
			}
		}
		i++
	}
	return "", "", false, 0, false
}

// resolve looks up name in the environment and returns the value to emit,
// whether the default was used, and whether this reference should count as
// a "missing" warning. A reference with a default never reports missing (it
// has a fallback). A reference without a default reports missing only when
// the var is unset or empty.
//
// NESTED DEFAULTS ARE RECURSIVELY RESOLVED: when the default is used and it
// contains a variable reference (${OTHER} or $OTHER, not $$), the default
// text is re-expanded via ExpandBytes before being returned. So
// ${OUTER:-${INNER}} with OUTER empty yields the value of INNER, not the
// literal text "${INNER}". This matches Docker Compose semantics and makes
// nested-default raw-mode tokens produce valid JSON (the resolved value is
// emitted, not a literal ${INNER} that would be invalid as a raw token).
// Deeper nesting (${A:-${B:-${C}}}) works via the natural recursion of
// ExpandBytes calling resolve calling ExpandBytes.
//
// nestedWarns carries the names of variables that were referenced inside a
// nested default and were unset/empty (so the caller can surface them). It
// is non-empty only when the default was used and contained var refs.
func resolve(name, def string, hasDef bool) (val string, usedDefault, warned bool, nestedWarns []string) {
	if hasDef {
		// ${VAR:-default}: use VAR if set AND non-empty, else default.
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v, false, false, nil
		}
		// Default is used. If it contains a nested variable reference,
		// recursively expand it so ${OUTER:-${INNER}} resolves INNER rather
		// than emitting the literal "${INNER}". If the default has no var
		// refs (the common case: a literal fallback), just fold $$ → $.
		if containsVarRef(def) {
			expanded, warns := ExpandBytes([]byte(def))
			return string(expanded), true, false, warns
		}
		return foldDollarDollar(def), true, false, nil
	}
	// ${VAR} / $VAR: empty if unset. Warn if unset/empty.
	v := os.Getenv(name)
	if v == "" {
		return "", false, true, nil
	}
	return v, false, false, nil
}

// emit writes val to out, JSON-escaped when inside a string literal and raw
// otherwise.
func emit(out *strings.Builder, val string, inString bool) {
	if !inString {
		out.WriteString(val)
		return
	}
	writeJSONEscaped(out, val)
}

// writeJSONEscaped appends val to out with JSON string escaping: '"' and '\'
// are escaped, control chars (< 0x20) become \n, \t, \r, or \uXXXX. This
// guarantees a substituted value can never break out of the enclosing JSON
// string literal.
func writeJSONEscaped(out *strings.Builder, val string) {
	for i := 0; i < len(val); i++ {
		c := val[i]
		switch {
		case c == '"':
			out.WriteString(`\"`)
		case c == '\\':
			out.WriteString(`\\`)
		case c == '\n':
			out.WriteString(`\n`)
		case c == '\t':
			out.WriteString(`\t`)
		case c == '\r':
			out.WriteString(`\r`)
		case c < 0x20:
			// \uXXXX for other control bytes.
			const hex = "0123456789abcdef"
			out.WriteString(`\u00`)
			out.WriteByte(hex[c>>4])
			out.WriteByte(hex[c&0xf])
		default:
			out.WriteByte(c)
		}
	}
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNamePart(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

// foldDollarDollar collapses every "$$" in s to a single "$", matching the
// main scanner's escape semantics (see the $$ case at lines 85-90). It is
// applied to a default value when that default is actually emitted, so a
// default like "${VAR:-$$cost}" yields "$cost" — the same result "$$cost"
// would yield in the main scan body. Without it the default's emit path
// would leave "$$" intact while every other path collapses it.
func foldDollarDollar(s string) string {
	if !strings.Contains(s, "$$") {
		return s
	}
	return strings.ReplaceAll(s, "$$", "$")
}

// containsVarRef reports whether s contains an unescaped env-var reference
// (${VAR} or $VAR, but not $$). Used by resolve to detect a nested-reference
// default like "${OTHER}" inside ${OUTER:-${OTHER}} — when the default is
// used and contains a var ref, resolve recursively expands it via ExpandBytes
// so ${OTHER} resolves to OTHER's value rather than being emitted verbatim.
func containsVarRef(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			i++ // skip escaped $$
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' {
			return true
		}
		if i+1 < len(s) && isNameStart(s[i+1]) {
			return true
		}
	}
	return false
}

// sortStrings sorts s in place in ascending order. The warning list is tiny
// (a handful of distinct var names), so an inline insertion sort avoids
// pulling in "sort" for a single call.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
