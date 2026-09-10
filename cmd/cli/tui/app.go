package tui

import (
	"context"
	"os"
	"sort"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	openacp "github.com/yusheng-g/openagent-go/acp/sdk"
	"github.com/yusheng-g/openagent-go/cmd/cli/config"
	"github.com/yusheng-g/openagent-go/cmd/cli/server"
	"github.com/yusheng-g/openagent-go/cmd/cli/tui/components"
	"github.com/yusheng-g/openagent-go/cmd/cli/tui/theme"
	"github.com/yusheng-g/openagent-go/cmd/cli/tui/views/chat"
	"github.com/yusheng-g/openagent-go/version"
)

// Mouse tracking runs in bubbletea's cell-motion mode (1002h + SGR 1006):
// clicks, wheel, drags and motion are all delivered to the app. This powers
// the scrollbar drag and the in-transcript box selection, whose highlight is
// drawn by the app and whose copy lands in the clipboard via OSC 52
// (tea.SetClipboard). The app-owned mode replaces the terminal's native text
// selection for the duration of the session; terminals honoring the xterm
// convention keep plain drag native while Shift is held. bubbletea writes the
// mode switches inside its own frame buffer (per-View diff) and resets them
// on exit, so no hand-written sequences are involved here.

// StartInteractiveTUI launches the fullscreen interactive TUI. It runs the
// ACP server in-process via os.Pipe (no subprocess), connects as an ACP
// client, and streams agent responses into the chat transcript.
//
// ctx is the parent context (from main.go's signal handler); the TUI derives
// a cancelable child so ctrl+c kills both the TUI and the ACP server.
// cfg provides everything: models, memory, capabilities, and the TUI section.
func StartInteractiveTUI(ctx context.Context, cfg config.Config) error {
	// Some environments (SSH sessions, tmux-derived TERMs, sanitized
	// sandboxes) lack a usable TERM; fall back to xterm-256color so
	// lipgloss renders colors and box drawing correctly. An existing
	// valid value is kept untouched.
	if os.Getenv("TERM") == "" {
		_ = os.Setenv("TERM", "xterm-256color")
	}

	tuiCfg := cfg.TUI
	ver := version.Version

	theme.ApplyOverrides(tuiColorMap(tuiCfg.Colors))
	components.SetSuggestions(tuiCfg.Suggestions)
	components.SetLogo(tuiCfg.Logo)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workDir, _ := os.Getwd()

	model := chat.NewModel(ctx, cancel, workDir, ver, tuiCfg.Mode, tuiCfg.Colors.LogoColor, tuiCfg.LogoGradient)
	// Seed the welcome footer's MCP indicator from settings; the wire
	// mcp_servers_update (session create/load) replaces it with live
	// connect outcomes. Sorted for a stable display order.
	configured := make([]openacp.McpServerStatus, 0, len(cfg.McpServers))
	for name, mc := range cfg.McpServers {
		configured = append(configured, openacp.McpServerStatus{Name: name, Type: mc.Type})
	}
	sort.Slice(configured, func(i, j int) bool { return configured[i].Name < configured[j].Name })
	model.SetConfiguredMcpServers(configured)
	// Force truecolor: the TUI theme is 24-bit hex, and bubbletea's default
	// colorprofile.Detect can resolve to NoTTY/ASCII on some PTYs (e.g. a
	// headless/terminal-use emulator), which makes the renderer strip every
	// foreground and background color from the frame. Pinning TrueColor keeps
	// the theme (transcript cards, panel surfaces, selected-row highlight)
	// rendering as authored. See cmd/cli/tui/views/chat for the styling.
	p := tea.NewProgram(model, tea.WithColorProfile(colorprofile.TrueColor))
	model.SetProgram(p)

	go startACPInProcess(ctx, p, cfg, ver)

	_, err := p.Run()
	return err
}

// startACPInProcess creates two os.Pipe pairs for client↔server communication.
// The ACP server runs in a goroutine via RunACPTransport; the client connects
// via ConnectIO, performs the initialize/newSession handshake, and injects the
// session into the model through the event loop (Program.Send), so the model's
// backend handle is never written from this goroutine.
func startACPInProcess(ctx context.Context, p *tea.Program, cfg config.Config, ver string) {
	// os.Pipe (buffered, 64KB) lets the client write requests before the
	// server finishes building; they buffer until RunTransport reads.
	serverR, clientW, err := os.Pipe()
	if err != nil {
		p.Send(chat.AcpErrorMsg(err))
		return
	}
	clientR, serverW, err := os.Pipe()
	if err != nil {
		p.Send(chat.AcpErrorMsg(err))
		return
	}

	// ACP server: build + run in a goroutine.
	go func() {
		if err := server.RunACPTransport(ctx, &cfg, serverW, serverR); err != nil {
			p.Send(chat.AcpErrorMsg(err))
		}
		_ = serverW.Close()
		_ = serverR.Close()
	}()

	// ACP client: connect and handshake.
	client := openacp.NewClient("openagent-tui", ver)
	sess := client.ConnectIO(ctx, clientW, clientR)

	if _, err := sess.Initialize(ctx, openacp.InitializeRequest{
		ProtocolVersion: 1,
		ClientInfo:      &openacp.Implementation{Name: "openagent-tui", Version: ver},
	}); err != nil {
		p.Send(chat.AcpErrorMsg(err))
		return
	}

	handler := chat.NewAcpEventHandler(p)
	sess.SetEventHandler(handler)
	sess.SetClientRequestHandler(handler)
	p.Send(chat.AcpSessionConnectedMsg(sess))
	// The ACP session is created lazily on the user's first prompt — merely
	// opening the program (and the welcome page) must not persist a session.
	// ActiveSessionID stays empty until the first NewSession lands.
	p.Send(chat.AcpSessionReadyMsg("", nil))
}

// tuiColorMap translates config.TUIColors into the flat map shape
// theme.ApplyOverrides expects (snake_case keys → hex strings). Empty
// fields are dropped so ApplyOverrides keeps the built-in default.
func tuiColorMap(c config.TUIColors) map[string]string {
	m := map[string]string{}
	if c.BgNormal != "" {
		m["bg_normal"] = c.BgNormal
	}
	if c.BgSecondary != "" {
		m["bg_secondary"] = c.BgSecondary
	}
	if c.BgSurface != "" {
		m["bg_surface"] = c.BgSurface
	}
	if c.Primary != "" {
		m["primary"] = c.Primary
	}
	if c.Success != "" {
		m["success"] = c.Success
	}
	if c.Warning != "" {
		m["warning"] = c.Warning
	}
	if c.Danger != "" {
		m["danger"] = c.Danger
	}
	if c.TextNormal != "" {
		m["text_normal"] = c.TextNormal
	}
	if c.TextAsh != "" {
		m["text_ash"] = c.TextAsh
	}
	if c.BorderGray != "" {
		m["border_gray"] = c.BorderGray
	}
	if c.LogoColor != "" {
		m["logo_color"] = c.LogoColor
	}
	if c.SelectionBg != "" {
		m["selection_bg"] = c.SelectionBg
	}
	return m
}
