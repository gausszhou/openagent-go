package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/cmd/cli/config"
)

// ── settings ──
//
// The settings tool lets the agent read and modify settings.json — the
// same operations as the `openagent settings` CLI subcommand (set/get/
// list/append/delete), against the same on-disk file. Writes are atomic
// (temp file + rename); a running server's fsnotify watcher picks up the
// change and hot-reloads applicable config (telemetry, log level, models,
// mcp_servers) within ~500ms.
//
// The tool is NOT read-only — it is classified Dangerous so every action
// (including get/list) routes through the approver in manual mode.
// settings.json carries apikeys and other secrets; even reading it should
// be visible to the user, and sub-agents are excluded from it entirely.

type settingsTool struct{}

type settingsParams struct {
	Action string `json:"action" jsonschema:"description=Operation: set|get|list|append|delete"`
	Key    string `json:"key,omitempty" jsonschema:"description=Dotted-path key (e.g. telemetry.endpoint, provider.openai.apikey, mcp_servers.weather.command). Required for set/get/append/delete; ignored for list. Numeric segments index into arrays (e.g. provider.openai.models.0.id)."`
	Value  string `json:"value,omitempty" jsonschema:"description=Value for set/append. Parsed as JSON when valid (number/bool/object/array); otherwise treated as a plain string. Ignored for get/list/delete."`
}

func (t *settingsTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name: "settings",
		Description: "Read and modify the agent's settings.json configuration (providers, models, telemetry, log level, mcp_servers, capabilities, sandbox, etc.). " +
			"DO NOT proactively modify settings unless the user explicitly asks to change runtime configuration. " +
			"Legitimate use cases are narrow: adding/removing an MCP server (mcp_servers.*), changing the model/provider (provider.*, model), adjusting log level (log.level), or enabling telemetry (telemetry.*). " +
			"For anything else (capabilities, sandbox, embedding, channels, env, ...), tell the user to edit settings.json and restart — those are NOT hot-reloadable. " +
			"\n\n" +
			"BEFORE any write, call action=list first to see the current state and the file path — never assume a key exists or guess its shape. " +
			"\n\n" +
			"Actions: set <key> <value>, get <key>, list, append <key> <value> (to an array), delete <key>. " +
			"Keys are dotted paths (telemetry.endpoint, provider.openai.apikey, mcp_servers.<name>.command, log.level). " +
			"\n\n" +
			"MECHANISM: writes are atomic (temp file + rename, never a half-written file) and validated against the Config schema before save — a type-mismatched value (e.g. log.level=123) is rejected, the file is not touched. A running server watches the file via fsnotify and hot-reloads within ~500ms. " +
			fmt.Sprintf("FILE LOCATIONS: settings.json is at %s. %s", config.Path(), logLocationClause()) +
			"HOT-RELOAD (no restart): telemetry.*, log.level, provider.* (models), mcp_servers.* — mcp_servers affects new/reloaded sessions; active sessions keep their connected tools. " +
			"RESTART-REQUIRED: sandbox.*, capabilities.*, embedding.*, openviking.*, context_providers.*, sensitive.*, channels.*, server.*, env, plugins, default_mode, tui.* — changing these is a silent no-op until restart; tell the user. " +
			"\n\n" +
			"PLUGIN-INJECTED KEYS: WASM plugins (see the `plugins` array from list) may inject or override keys at startup. get/list reads the DISK file only — plugin-injected values are NOT visible. If a key is plugin-managed, set writes the disk value but the plugin's value may still win at runtime. " +
			"DESTRUCTIVE: deleting a provider.* entry can remove the only model (all turns fail until re-added); deleting mcp_servers.* drops a server other sessions depend on. JSON corruption is NOT possible (atomic + validated), but review with list before deleting. " +
			"\n\n" +
			"Use this INSTEAD of hand-editing settings.json with the write tool. Typical MCP add: set mcp_servers.<name>.command <bin> (plus .args/.env/.url/.type as needed).",
		Parameters: openagent.SchemaOf[settingsParams](),
	}
}

func (t *settingsTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	p, err := openagent.ParseArgs[settingsParams](args)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("settings: %w", err), false, "")
	}
	action := strings.TrimSpace(p.Action)
	switch action {
	case "set":
		if strings.TrimSpace(p.Key) == "" {
			return openagent.ErrorResult(fmt.Errorf("settings set: key is required"), false, "")
		}
		if err := config.SetSetting(p.Key, p.Value); err != nil {
			return openagent.ErrorResult(fmt.Errorf("settings set: %w", err), true, "")
		}
		return &openagent.ToolResult{Content: fmt.Sprintf("set %s = %s (hot-reloaded if a server is running)", p.Key, p.Value)}
	case "get":
		if strings.TrimSpace(p.Key) == "" {
			return openagent.ErrorResult(fmt.Errorf("settings get: key is required"), false, "")
		}
		val, err := config.GetSetting(p.Key)
		if err != nil {
			return openagent.ErrorResult(fmt.Errorf("settings get: %w", err), false, "")
		}
		return &openagent.ToolResult{Content: val}
	case "list":
		val, err := config.ListSettings()
		if err != nil {
			return openagent.ErrorResult(fmt.Errorf("settings list: %w", err), false, "")
		}
		return &openagent.ToolResult{Content: val}
	case "append":
		if strings.TrimSpace(p.Key) == "" {
			return openagent.ErrorResult(fmt.Errorf("settings append: key is required"), false, "")
		}
		if err := config.AppendSetting(p.Key, p.Value); err != nil {
			return openagent.ErrorResult(fmt.Errorf("settings append: %w", err), true, "")
		}
		return &openagent.ToolResult{Content: fmt.Sprintf("appended to %s (hot-reloaded if a server is running)", p.Key)}
	case "delete":
		if strings.TrimSpace(p.Key) == "" {
			return openagent.ErrorResult(fmt.Errorf("settings delete: key is required"), false, "")
		}
		if err := config.DeleteSetting(p.Key); err != nil {
			return openagent.ErrorResult(fmt.Errorf("settings delete: %w", err), true, "")
		}
		return &openagent.ToolResult{Content: fmt.Sprintf("deleted %s (hot-reloaded if a server is running)", p.Key)}
	default:
		return openagent.ErrorResult(fmt.Errorf("settings: unknown action %q (want set|get|list|append|delete)", action), false, "")
	}
}

// newSettingsTool returns the settings tool. It is server-level (not
// scoped to a session cwd) because settings.json is a global file.
func newSettingsTool() openagent.Tool {
	return &settingsTool{}
}

// logLocationClause returns the log-file sentence for the tool description,
// using the path captured at SetupLog time. Empty path means logging is
// discarded (no log file configured).
func logLocationClause() string {
	p := logFilePath.Load()
	if p == nil || *p == "" {
		return "Logging is currently discarded (no log.file configured)."
	}
	return fmt.Sprintf("The log file is %s — read it with the read tool to inspect server output.", *p)
}
