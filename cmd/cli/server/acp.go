package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/acp"
	openacpsdk "github.com/yusheng-g/openagent-go/acp/sdk"
	"github.com/yusheng-g/openagent-go/agent"
	"github.com/yusheng-g/openagent-go/keyring"
	"github.com/yusheng-g/openagent-go/sandbox/native"
	"github.com/yusheng-g/openagent-go/summarizer"
	"github.com/yusheng-g/openagent-go/version"

	wasm "github.com/yusheng-g/openagent-go/plugin/agent/wasm"
	"github.com/yusheng-g/openagent-go/plugin/wasmhost"
	"github.com/yusheng-g/openagent-go/scheduler"

	"github.com/yusheng-g/openagent-go/cmd/cli/config"
	ctxpkg "github.com/yusheng-g/openagent-go/context"
)

// RunACP starts the agent in ACP mode over stdio.
//
// Lifecycle:
//  1. Open memory + session store (SQLite).
//  2. Build models from config.
//  3. Create sandbox + standard tools.
//  4. Wire summarizer for long-conversation compression.
//  5. Construct the agent.
//  6. Wrap in AgentServer, launch ACP protocol mux on stdin/stdout.
func RunACP(ctx context.Context, cfg *config.Config) error {
	server, cleanup, err := BuildACPServer(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	slog.Info("ACP server starting on stdio")
	return server.Run(ctx)
}

// BuildACPServer constructs the ACP server (memory, models, tools, agent,
// channels) and returns it with a cleanup func. Used by both RunACP (stdio)
// and RunACPTransport (in-process pipe for the TUI).
func BuildACPServer(ctx context.Context, cfg *config.Config) (*openacpsdk.Server, func(), error) {
	caps := cfg.Capabilities
	ms, knowledge, sessionStore, cleanup, err := buildMemory(cfg.Embedding, caps.OnEmbedder())
	if err != nil {
		return nil, nil, err
	}

	_, modelInfos := buildModels(cfg.Provider)
	if len(modelInfos) == 0 {
		slog.Warn("no models configured, ACP server will start but prompt turns will fail")
	}

	modelMap := make(map[string]openagent.Model, len(modelInfos))
	for _, mi := range modelInfos {
		key := mi.ID
		if mi.Provider != "" {
			key = mi.Provider + "/" + mi.ID
		}
		modelMap[key] = mi.Model
	}

	// Summarizer and Memory are enabled by default; allow --summarizer=off
	// and --memory=off to disable them.
	var firstM openagent.Model
	if len(modelInfos) > 0 {
		firstM = modelInfos[0].Model
	}

	// Tools and sandbox are created once per session (buildRuntimeForSession)
	// scoped to the session's cwd. Agent configuration is pure (model,
	// prompts, limits, guards, skills); runtime capabilities live in
	// kernel.Deps.
	opts := []agent.Option{
		agent.WithSystemPrompts(resolveProfiles("")...),
		agent.WithMaxTurns(500),
	}
	opts, skillProvider := buildOpts(opts, caps, firstM)
	agentCfg := agent.New(version.Name, opts...)

	holder, _, telemetryShutdown, err := setupTelemetry(ctx, *cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry init: %w", err)
	}
	// NOTE: telemetryShutdown is NOT deferred here — BuildACPServer returns
	// immediately and its defers would fire before server.Run starts, shutting
	// down the TracerProvider prematurely. It is wired into the returned
	// cleanup func so the caller defers it at the right scope.

	deps := buildRuntimeDeps(caps, cfg.Sensitive, holder)
	deps.SkillProvider = skillProvider
	// Pass nil Mem when --memory=off so the AgentServer skips history
	// replay and memory cleanup (all s.Mem uses are nil-guarded). The
	// sessionStore (session metadata) is separate and unaffected.
	if caps.OnMemory() {
		deps.SessionStore = ms
		deps.Compressor = ms
		deps.MemoryProvider = knowledge
	}

	var sumz *summarizer.Compressor
	if caps.OnMemory() && caps.OnSummarizer() && firstM != nil {
		sumz = summarizer.New(firstM).WithMaxTokens(agentCfg.MaxCompressedTokens)
		ms.WithSummarizer(sumz)
		// Share the summarizer with sub-agent children so their in-memory
		// stores get compaction parity with the parent.
		deps.Summarizer = sumz
	}

	// Plugin manager — loads agent:tools and agent:observers plugins.
	// Discover before constructing the server so a plugin observer can be
	// merged into deps.Observer before the AgentServer snapshots it.
	var pluginMgr *wasm.Manager
	pluginDir := resolvePluginsDir()
	sch := scheduler.New()
	go sch.Run(ctx)
	mgr := wasm.NewManager(pluginDir).
		WithHostAPI(wasmhost.NewHostAPI(keyring.NewKeyring())).
		WithScheduler(sch)
	if err := mgr.Discover(ctx); err != nil {
		slog.Warn("plugin discover failed", "error", err)
	} else {
		pluginMgr = mgr
		if obs := mgr.Observer(); obs != nil {
			if deps.Observer != nil {
				deps.Observer = openagent.MultiObserver(deps.Observer, obs)
			} else {
				deps.Observer = obs
			}
			slog.Info("plugin observer wired", "source", "wasm")
		}
	}

	if err := applyContextProviders(cfg, &deps); err != nil {
		return nil, nil, err
	}
	// The extractor captures the MemoryProvider it writes to — build it
	// AFTER applyContextProviders so the effective provider is used.
	// Building it earlier would fork writes to the local sqlite store
	// while Recall reads the OpenViking index (silent knowledge loss).
	var extractor *ctxpkg.AsyncExtractor
	if caps.OnMemory() && firstM != nil && deps.MemoryProvider != nil {
		extractor = ctxpkg.NewAsyncExtractor(ctxpkg.NewLLMExtractor(func() openagent.Model { return firstM }, deps.MemoryProvider))
		deps.Extractor = extractor
	}
	srv := acp.NewAgentServer(agentCfg, deps, sessionStore, modelMap)

	// Switch the extractor to dynamic model lookup so it always uses the
	// server's current default model (picked up from the registry, which
	// SetModel updates at runtime). This covers the case where the user
	// changes api_key/base_url in settings without switching models.
	if extractor != nil {
		extractor.SetModelFn(func() openagent.Model {
			if id := srv.GetDefaultModelID(); id != "" {
				if m, ok := srv.LookupModel(id); ok {
					return m
				}
			}
			return firstM
		})
	}
	srv.AgentName = version.Name
	srv.AgentVersion = version.Version
	srv.MCPEnabled = caps.OnMCP()
	srv.DefaultMode = cfg.DefaultMode
	srv.PluginMgr = pluginMgr
	srv.Summarizer = sumz
	srv.Extractor = extractor
	srv.ProfileResolver = func(cwd string) []string {
		return resolveProfiles(cwd)
	}

	// Register model configs for runtime_set_model_config.
	for _, mi := range modelInfos {
		key := mi.ID
		if mi.Provider != "" {
			key = mi.Provider + "/" + mi.ID
		}
		srv.RegisterModel(key, mi.Provider, mi.ID, mi.APIKey, mi.BaseURL, acp.ModelPricing{
			MaxOutputTokens:        mi.MaxOutputTokens,
			InputCostPerToken:      mi.InputCostPerToken,
			InputCacheCostPerToken: mi.InputCacheCostPerToken,
			OutputCostPerToken:     mi.OutputCostPerToken,
		})
	}

	// settings "model" ("<provider>/<modelID>") wins as the default;
	// fall back to the first registered model.
	if cfg.Model != "" {
		if !srv.SetDefaultModelID(cfg.Model) {
			slog.Warn("settings model not in provider list, using first registered", "model", cfg.Model)
		}
	}

	policy := sandboxPolicy(cfg.Sandbox)
	baseToolList := []string{"shell", "read", "write", "ls", "grep", "websearch", "webfetch"}
	if caps.OnBrowser() {
		baseToolList = append(baseToolList, "browser")
	}
	if caps.OnOffice() {
		baseToolList = append(baseToolList, "office")
	}
	srv.ToolFactory = func(cwd string) []openagent.Tool {
		sb, err := native.NewWithPolicy(cwd, policy)
		if err != nil {
			slog.Warn("tool factory: sandbox creation failed; execution tools disabled", "cwd", cwd, "error", err)
			return nil
		}
		return buildTools(sb, cwd, baseToolList)
	}
	server := openacpsdk.NewServer(version.Name, version.Version, srv)
	server.SetLogger(slog.Default())

	// Start settings watcher for hot-reload (telemetry/log-level/models).
	go watchSettings(ctx, &settingsWatcher{
		cfgPath:  config.Path(),
		prev:     cfg,
		holder:   holder,
		shutdown: telemetryShutdown,
		srv:      srv,
	})

	// Channel agent: clone the template and inject a default Model + Tools
	// so the IM bot can run standalone (the ACP path resolves the model per
	// session, but channels call kernel.New(...).RunStream directly).
	channelCfg := agentCfg.Clone()
	for _, mi := range modelInfos {
		if mi.Model != nil {
			channelCfg.Model = mi.Model
			break
		}
	}
	channelDeps := deps
	cwd, _ := os.Getwd()
	if sb, err := native.NewWithPolicy(cwd, policy); err == nil {
		channelDeps.Tools = buildTools(sb, cwd, baseToolList)
	}

	if _, _, _, err := RunChannels(ChannelEnv{
		Ctx:         ctx,
		Cfg:         channelCfg,
		Deps:        channelDeps,
		DefaultMode: cfg.DefaultMode,
		WorkDir:     cwd,
		MetaStore:   sessionStore,
	}, cfg.Channels); err != nil {
		slog.Warn("channel error", "error", err)
	}

	// Wrap cleanup to also shutdown telemetry (TracerProvider flush).
	// This runs when the caller defers cleanup() — after server.Run exits.
	teardown := func() {
		cleanup()
		telemetryShutdown()
	}
	return server, teardown, nil
}

// RunACPTransport builds the ACP server (same as RunACP) but serves on
// custom I/O streams instead of os.Stdin/os.Stdout. Used by the TUI to
// run the ACP server in-process via io.Pipe — no subprocess needed.
func RunACPTransport(ctx context.Context, cfg *config.Config, w io.Writer, r io.Reader) error {
	server, cleanup, err := BuildACPServer(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	return server.RunTransport(ctx, w, r)
}
