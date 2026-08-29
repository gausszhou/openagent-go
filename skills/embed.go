// Package skills embeds the built-in skill directory tree into the binary
// unconditionally. The builtin/ subtree is bundled via //go:embed so the
// agent ships with its built-in skills with no build tag required.
package skills

import (
	"embed"
	"io/fs"
)

//go:embed builtin
var builtinSkillsFS embed.FS

// BuiltinFS returns the embedded built-in skills as an fs.FS (each top-level
// directory under builtin/ is one skill). The embedded source is always
// present; disk roots (user/project) discovered at runtime override it.
func BuiltinFS() fs.FS {
	sub, err := fs.Sub(builtinSkillsFS, "builtin")
	if err != nil {
		return nil
	}
	return sub
}
