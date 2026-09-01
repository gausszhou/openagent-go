package kernel

import (
	"fmt"
	"strings"
)

// FormatSubAgentNote renders a sub-agent completion notification as a
// <system-reminder> block. The note is delivered to the model via the SDK
// mux's TriggerTurn (an idle turn fully serialized with user turns via
// sessionLocks). It becomes the prompt input so the model processes the
// sub-agent's result immediately, not "whenever the user comes back".
func FormatSubAgentNote(agentID, description, result, stopReason string) string {
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	if description != "" {
		fmt.Fprintf(&b, "[SUB-AGENT COMPLETED]\nSub-agent %s (%s) has completed.", agentID, description)
	} else {
		fmt.Fprintf(&b, "[SUB-AGENT COMPLETED]\nSub-agent %s has completed.", agentID)
	}
	if result != "" {
		b.WriteString("\nResult:\n")
		b.WriteString(result)
	}
	if stopReason == "max_turns" {
		b.WriteString("\n[INCOMPLETE: this sub-agent hit its turn limit — the result above is partial, not a finished answer.]")
	}
	if stopReason == "error" {
		b.WriteString("\n[ERROR: this sub-agent failed to complete.]")
	}
	b.WriteString("\n</system-reminder>")
	return b.String()
}

// FormatSettingsChangeNote renders a settings-changed notification as a
// <system-reminder> block. Delivered to the model via triggerIdleTurn when
// fsnotify detects an external edit to settings.json. The model decides
// whether to call the settings tool's reload action.
func FormatSettingsChangeNote() string {
	return "<system-reminder>\n" +
		"[SETTINGS CHANGED]\n" +
		"settings.json was modified externally. " +
		"If you did not just modify it yourself, call the settings tool with " +
		"action=reload to apply the changes (or action=list to inspect first). " +
		"If you just called set and reload, you can ignore this.\n" +
		"</system-reminder>"
}
