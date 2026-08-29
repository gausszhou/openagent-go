package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	openagent "github.com/yusheng-g/openagent-go"
	openacpsdk "github.com/yusheng-g/openagent-go/acp/sdk"
	"github.com/yusheng-g/openagent-go/mcp"
	"github.com/yusheng-g/openagent-go/version"

	"github.com/yusheng-g/openagent-go/cmd/cli/config"
)

// connectMcpFromConfig connects to all configured MCP servers and returns
// their tools plus a cleanup function that closes all sessions.
// Failed connections are logged but not fatal.
func connectMcpFromConfig(ctx context.Context, servers map[string]config.McpServerConfig) ([]openagent.Tool, func()) {
	if len(servers) == 0 {
		return nil, func() {}
	}

	client := mcp.NewClient(version.Name, version.Version)
	var tools []openagent.Tool
	var sessions []*mcp.Session

	for name, s := range servers {
		sess, err := connectMcpOne(ctx, client, s)
		if err != nil {
			slog.Warn("mcp connect failed", "name", name, "error", err)
			continue
		}
		// Named so tools are "mcp__<server>__<tool>" (Claude Code
		// convention) — unique across servers, self-describing.
		list, err := sess.Named(name).Tools(ctx)
		if err != nil {
			sess.Close()
			slog.Warn("mcp list tools failed", "name", name, "error", err)
			continue
		}
		sessions = append(sessions, sess)
		tools = append(tools, list...)
		slog.Info("mcp connected", "name", name, "tools", len(list))
	}

	return tools, func() {
		for _, s := range sessions {
			s.Close()
		}
	}
}

func connectMcpOne(ctx context.Context, client *mcp.Client, cfg config.McpServerConfig) (*mcp.Session, error) {
	switch cfg.Type {
	case "http", "sse":
		if cfg.URL == "" {
			return nil, fmt.Errorf("missing url")
		}
		return client.ConnectHTTP(ctx, cfg.URL)
	default:
		if cfg.Command == "" {
			return nil, fmt.Errorf("missing command")
		}
		cmd := cfg.Command
		if strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "../") {
			if abs, err := filepath.Abs(cmd); err == nil {
				cmd = abs
			}
		}
		var env []string
		for k, v := range cfg.Env {
			env = append(env, k+"="+v)
		}
		env = append(env, os.Environ()...)
		return client.ConnectStdioWithEnv(ctx, cmd, cfg.Args, env)
	}
}

// convertMcpServers translates settings.json MCP server configs (map keyed
// by name, Env as map[string]string) into the openacp.McpServer protocol
// type (slice with Name, Env as []EnvVariable). The map key becomes the
// server Name — the same field connectMCP uses for tool namespacing
// ("mcp__<name>__<tool>") and duplicate detection.
func convertMcpServers(servers map[string]config.McpServerConfig) []openacpsdk.McpServer {
	if len(servers) == 0 {
		return nil
	}
	out := make([]openacpsdk.McpServer, 0, len(servers))
	for name, c := range servers {
		envVars := make([]openacpsdk.EnvVariable, 0, len(c.Env))
		for k, v := range c.Env {
			envVars = append(envVars, openacpsdk.EnvVariable{Name: k, Value: v})
		}
		out = append(out, openacpsdk.McpServer{
			Name:    name,
			Type:    c.Type,
			Command: c.Command,
			Args:    c.Args,
			Env:     envVars,
			URL:     c.URL,
		})
	}
	return out
}
