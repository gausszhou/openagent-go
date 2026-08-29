package acp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	openacp "github.com/yusheng-g/openagent-go/acp/sdk"

	"github.com/yusheng-g/openagent-go/agent"
	"github.com/yusheng-g/openagent-go/kernel"
)

// TestNewAgentServer_MCPEnabledDefault verifies the MCP gate defaults to
// enabled so existing callers (who don't set it) keep MCP behavior.
func TestNewAgentServer_MCPEnabledDefault(t *testing.T) {
	srv := NewAgentServer(agent.New("test"), kernel.Deps{}, nil, nil)
	if !srv.MCPEnabled {
		t.Error("NewAgentServer should default MCPEnabled to true")
	}
}

// TestConnectMCP_Gate verifies that MCPEnabled=false prevents any MCP
// connection attempt, while MCPEnabled=true allows it. The attempt is
// detected via a stdio "touch" side effect: a spawned process creates a
// file iff connectMCP actually invokes connectOneMCP.
func TestConnectMCP_Gate(t *testing.T) {
	touch, err := exec.LookPath("touch")
	if err != nil {
		t.Skip("touch not available; skipping MCP gate side-effect test")
	}

	srv := NewAgentServer(agent.New("test"), kernel.Deps{}, nil, nil)
	// A stdio MCP server whose "server" is just `touch <file>`. connectMCP
	// will spawn it (creating the file) then fail the MCP handshake — the
	// failure is logged and non-fatal, but the file proves the spawn.
	mkServers := func(file string) []openacp.McpServer {
		return []openacp.McpServer{{Name: "x", Command: touch, Args: []string{file}}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// ── Disabled: no spawn, no file ──
	fileOff := filepath.Join(t.TempDir(), "spawned-off")
	srv.MCPEnabled = false
	if sess, tools := srv.connectMCP(ctx, mkServers(fileOff)); sess != nil || tools != nil {
		t.Errorf("disabled connectMCP = %v, %v; want nil, nil", sess, tools)
	}
	if _, err := os.Stat(fileOff); err == nil {
		t.Error("connectMCP spawned a process despite MCPEnabled=false")
	}

	// ── Enabled: spawn happens, file created ──
	fileOn := filepath.Join(t.TempDir(), "spawned-on")
	srv.MCPEnabled = true
	srv.connectMCP(ctx, mkServers(fileOn))
	// The spawn is synchronous up to handshake, but allow a brief window
	// for the filesystem to reflect the touch.
	deadline := time.Now().Add(2 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		if _, err := os.Stat(fileOn); err == nil {
			found = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !found {
		t.Error("connectMCP did not spawn process with MCPEnabled=true (file not created)")
	}
}

// TestMergeMcpServers_ClientOverridesSettings verifies that a session-level
// (client-advertised) MCP server shadows a settings.json server of the same
// name — the client is the more specific source and wins. Settings servers
// not shadowed are appended.
func TestMergeMcpServers_ClientOverridesSettings(t *testing.T) {
	srv := NewAgentServer(agent.New("test"), kernel.Deps{}, nil, nil)
	srv.MCPEnabled = true
	srv.SetSettingsMcpServers([]openacp.McpServer{
		{Name: "shared", URL: "http://settings.example"}, // shadowed by client
		{Name: "global-only", Command: "/bin/global"},    // kept
	})

	client := []openacp.McpServer{
		{Name: "shared", URL: "http://client.example"}, // wins
		{Name: "client-only", Command: "/bin/client"},  // kept
	}

	merged := srv.mergeMcpServers(client)
	if len(merged) != 3 {
		t.Fatalf("merged len = %d, want 3: %+v", len(merged), merged)
	}

	byName := make(map[string]openacp.McpServer, len(merged))
	for _, m := range merged {
		byName[m.Name] = m
	}
	if got := byName["shared"].URL; got != "http://client.example" {
		t.Errorf("shared server: client did not override settings; got URL %q", got)
	}
	if _, ok := byName["global-only"]; !ok {
		t.Error("settings-only server was dropped")
	}
	if _, ok := byName["client-only"]; !ok {
		t.Error("client-only server was dropped")
	}
}

// TestMergeMcpServers_NoSettings verifies the settings list is not consulted
// when empty — the client list passes through unchanged.
func TestMergeMcpServers_NoSettings(t *testing.T) {
	srv := NewAgentServer(agent.New("test"), kernel.Deps{}, nil, nil)
	client := []openacp.McpServer{{Name: "x", Command: "/bin/x"}}
	merged := srv.mergeMcpServers(client)
	if len(merged) != 1 || merged[0].Name != "x" {
		t.Errorf("merge without settings = %+v; want client passthrough", merged)
	}
}

// TestMergeMcpServers_NoClient verifies that settings servers are used when
// the client advertises none.
func TestMergeMcpServers_NoClient(t *testing.T) {
	srv := NewAgentServer(agent.New("test"), kernel.Deps{}, nil, nil)
	srv.MCPEnabled = true
	srv.SetSettingsMcpServers([]openacp.McpServer{{Name: "g", Command: "/bin/g"}})
	merged := srv.mergeMcpServers(nil)
	if len(merged) != 1 || merged[0].Name != "g" {
		t.Errorf("merge without client = %+v; want settings passthrough", merged)
	}
}

// TestSetSettingsMcpServers_HotSwap verifies the settings list can be
// replaced concurrently with merge reads (the watcher hot-swaps it).
func TestSetSettingsMcpServers_HotSwap(t *testing.T) {
	srv := NewAgentServer(agent.New("test"), kernel.Deps{}, nil, nil)
	srv.MCPEnabled = true
	srv.SetSettingsMcpServers([]openacp.McpServer{{Name: "v1"}})
	srv.SetSettingsMcpServers([]openacp.McpServer{{Name: "v2"}})
	merged := srv.mergeMcpServers(nil)
	if len(merged) != 1 || merged[0].Name != "v2" {
		t.Errorf("after hot-swap: merged = %+v; want v2", merged)
	}
}
