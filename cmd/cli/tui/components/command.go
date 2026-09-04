package components

// Command describes a slash-command entry shown in panels.
type Command struct {
	Title  string
	Key    string
	Slash  string
	Alias  string
	Icon   string
	Desc   string
	Enable bool
	Space  bool // toggle with space key
}

// VisibleConfig holds the transcript visibility toggles. Only the fields the
// view layer reads are kept; the toggle interactions are out of scope.
type VisibleConfig struct {
	ShowThinking   bool
	ShowToolSkill  bool
	ShowToolShell  bool
	ShowToolDetail bool
}

func NewCommandList() []Command {
	return []Command{
		{Title: "Switch session", Key: "", Slash: "/sessions", Alias: "", Icon: "", Enable: true},
		{Title: "New session", Key: "", Slash: "/new", Alias: "", Icon: "", Enable: true},
		{Title: "Switch model", Key: "", Slash: "/models", Alias: "", Icon: "", Enable: true},
		{Title: "Switch mode", Key: "", Slash: "/toggle_mode", Alias: "", Icon: "", Enable: true},
		{Title: "Toggle thinking content", Key: "", Slash: "/toggle_thinking", Alias: "", Icon: "○", Enable: true, Space: true},
		{Title: "Toggle skill tools", Key: "", Slash: "/toggle_skill", Alias: "", Icon: "○", Enable: true, Space: true},
		{Title: "Toggle shell tools", Key: "", Slash: "/toggle_shell", Alias: "", Icon: "○", Enable: true, Space: true},
		{Title: "Toggle tool call detail", Key: "", Slash: "/toggle_toolcall", Icon: "○", Alias: "", Enable: true, Space: true},
		{Title: "Update skills", Key: "", Slash: "/update_skills", Alias: "", Icon: "", Enable: true},
		{Title: "Exit the app", Key: "", Slash: "/exit", Alias: "quit", Icon: "", Enable: true},
	}
}
