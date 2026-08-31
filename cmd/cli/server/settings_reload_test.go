package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yusheng-g/openagent-go/acp"
	"github.com/yusheng-g/openagent-go/agent"
	"github.com/yusheng-g/openagent-go/cmd/cli/config"
	"github.com/yusheng-g/openagent-go/kernel"
)

// TestReload_KeepsPluginInjectedProvider is the regression test for the
// plugin-injected provider disappearing after a settings reload.
//
// Setup simulates the runtime state right after startup with a cli:settings
// plugin: the AgentServer registry holds a plugin-injected provider
// ("my_plugin_provider/injected-model") that is NOT in settings.json, while
// sw.prev is the merged startup config (also carrying that provider). The
// watcher then reloads a settings file that:
//   - drops the plugin provider (file-only, as the disk file always is), and
//   - changes an unrelated key (log.level).
//
// Pre-fix, reconfigureModels' delete loop scrubbed the plugin provider
// because it was absent from the file-only newKeys. Post-fix (additive-only),
// the plugin provider survives and the unrelated key change is applied.
func TestReload_KeepsPluginInjectedProvider(t *testing.T) {
	ctx := context.Background()

	// Real AgentServer — the acp tests construct one this way.
	srv := acp.NewAgentServer(agent.New("test"), kernel.Deps{}, nil, nil)

	// Simulate a plugin-injected provider: registered via SetModel exactly
	// as loadPlugins → CallInit → buildModels → RegisterModel/SetModel does
	// at startup. This key never appears in the on-disk settings.json.
	const pluginKey = "my_plugin_provider/injected-model"
	srv.SetModel("my_plugin_provider", "injected-model", "sk-plugin-secret", "", 0, 0)
	if !containsModel(srv, pluginKey) {
		t.Fatalf("setup: plugin-injected model %q not registered", pluginKey)
	}

	// Write a settings file that does NOT list the plugin provider (file-
	// only) but DOES change an unrelated key (log.level). This is the exact
	// trigger: an unrelated edit that, pre-fix, wiped the plugin provider.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "settings.json")
	// First write: a base file so sw.prev can be seeded from it.
	if err := os.WriteFile(cfgPath, []byte(`{
		"log": {"level": "info"}
	}`), 0644); err != nil {
		t.Fatalf("write base settings: %v", err)
	}

	// Seed sw.prev from the file-only config — mirroring how reload sees it.
	// (Startup seeds sw.prev from the *merged* config; either way, the
	// provider-diff fires because newCfg is file-only. We use file-only here
	// to isolate the delete-loop bug from the sw.prev mismatch.)
	var prevCfg config.Config
	prevCfg.Log.Level = "info"

	sw := &settingsWatcher{
		cfgPath: cfgPath,
		prev:    &prevCfg,
		srv:     srv,
	}

	// Now rewrite the file: same providers (none), changed log.level.
	if err := os.WriteFile(cfgPath, []byte(`{
		"log": {"level": "debug"}
	}`), 0644); err != nil {
		t.Fatalf("write changed settings: %v", err)
	}

	sw.reload(ctx)

	// The plugin-injected provider MUST survive the reload.
	if !containsModel(srv, pluginKey) {
		t.Errorf("plugin-injected model %q was removed by reload — "+
			"reconfigureModels must be additive (bug regression)", pluginKey)
	}
}

// TestReload_AddsFileDeclaredProvider verifies the additive path still
// works: a provider newly added to settings.json appears in the registry
// after reload. This guards against an over-correction that drops the
// SetModel add/update pass along with the delete loop.
func TestReload_AddsFileDeclaredProvider(t *testing.T) {
	ctx := context.Background()
	srv := acp.NewAgentServer(agent.New("test"), kernel.Deps{}, nil, nil)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(cfgPath, []byte(`{
		"log": {"level": "info"}
	}`), 0644); err != nil {
		t.Fatalf("write base settings: %v", err)
	}

	var prevCfg config.Config
	prevCfg.Log.Level = "info"
	sw := &settingsWatcher{
		cfgPath: cfgPath,
		prev:    &prevCfg,
		srv:     srv,
	}

	// Rewrite adding a file-declared provider + changing log.level so the
	// provider-diff fires.
	if err := os.WriteFile(cfgPath, []byte(`{
		"log": {"level": "debug"},
		"provider": {
			"openai": {
				"api_key": "sk-test",
				"models": ["gpt-4o"]
			}
		}
	}`), 0644); err != nil {
		t.Fatalf("write settings with provider: %v", err)
	}

	sw.reload(ctx)

	if !containsModel(srv, "openai/gpt-4o") {
		t.Errorf("file-declared model openai/gpt-4o was not added by reload — "+
			"the SetModel add/update pass must still run")
	}
}

// containsModel reports whether srv has a model registered under key.
func containsModel(srv *acp.AgentServer, key string) bool {
	for _, id := range srv.ModelIDs() {
		if id == key {
			return true
		}
	}
	return false
}
