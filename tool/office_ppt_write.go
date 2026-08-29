package tool

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	openagent "github.com/yusheng-g/openagent-go"
)

//go:embed pptx-worker/worker.bundle.mjs
var pptxWorkerBundleFS embed.FS

// ── pptx_write ──

// pptxWriteTool creates a .pptx file by running an LLM-generated JavaScript
// build script (PptxGenJS) in a Node.js subprocess. The worker.bundle.mjs is
// embedded in the binary and extracted to a temp file at runtime — no npm
// install needed, only a Node.js runtime.
//
// If Node.js is absent, the tool returns a clear error so the model can
// install Node via the shell tool and retry.
type pptxWriteTool struct {
	workDir string
}

type pptxWriteParams struct {
	Path       string          `json:"path" jsonschema:"description=Output path for the .pptx file."`
	Script     string          `json:"script,omitempty" jsonschema:"description=Inline JavaScript module exporting default async function build(pptx, ctx) or a named build function. Use this for small decks (few slides)."`
	ScriptPath string          `json:"script_path,omitempty" jsonschema:"description=Path to a .mjs build script file. For large decks: write the script to a tmp file (e.g. /tmp/deck.mjs) with the write tool — first call creates it, subsequent calls use append=true to add chunks — then pass the path here."`
	Data       json.RawMessage `json:"data,omitempty" jsonschema:"description=Optional JSON value passed to ctx.data inside the script"`
	AssetsDir  string          `json:"assets_dir,omitempty" jsonschema:"description=Optional base directory for ctx.resolveAsset() and ctx.imageData() calls"`
}

func (t *pptxWriteTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name: "pptx_write",
		Description: "Create a PowerPoint (.pptx) file from scratch by running a PptxGenJS-Plus build script in Node.js. " +
			"The script exports default async function build(pptx, ctx) and adds slides to the provided PptxGenJS instance. " +
			"Supports ChartEx types (funnel/treemap/waterfall), preset shadows, picture fills, connectors, text fields, and unit-suffixed dimensions. " +
			"Requires Node.js (install via shell if missing). " +
			"WORKFLOW: small decks (few slides) — pass the script inline via the script param. Large decks (many slides) — write the script to a .mjs file using the write tool (use append=true to build it in chunks so each call carries only a slice, avoiding thinking bloat), then pass script_path. " +
			"DO NOT add a branding/credit/\"powered by\" slide at the end unless the user explicitly asks for one — the deck is the user's product, not a promo for the tool. Only add slides the user requested. " +
			"Use this when creating a deck with no template; when a template is available, prefer pptx_template_fill (no Node needed, preserves design).",
		Parameters: openagent.SchemaOf[pptxWriteParams](),
	}
}

func (t *pptxWriteTool) Execute(ctx context.Context, args json.RawMessage) *openagent.ToolResult {
	p, err := openagent.ParseArgs[pptxWriteParams](args)
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("pptx_write: %w", err), false, "")
	}
	if strings.TrimSpace(p.Path) == "" {
		return officeToolError("pptx_write", "missing required parameter: path")
	}
	scriptPathInput := strings.TrimSpace(p.ScriptPath)
	inlineScript := strings.TrimSpace(p.Script)
	if scriptPathInput == "" && inlineScript == "" {
		return officeToolError("pptx_write", "missing required parameter: provide script_path (preferred) or script")
	}

	// Resolve Node.js.
	nodePath, err := exec.LookPath("node")
	if err != nil {
		return officeToolError("pptx_write", "Node.js was not found; install Node.js (v18+) to enable PowerPoint generation, then retry")
	}

	// Resolve assets_dir if provided.
	assetsDir := strings.TrimSpace(p.AssetsDir)
	if assetsDir != "" {
		if !filepath.IsAbs(assetsDir) {
			assetsDir, err = filepath.Abs(assetsDir)
			if err != nil {
				return officeToolError("pptx_write", fmt.Sprintf("invalid assets_dir: %s", err.Error()))
			}
		}
		info, err := os.Stat(assetsDir)
		if err != nil {
			return officeToolError("pptx_write", fmt.Sprintf("invalid assets_dir: %s", err.Error()))
		}
		if !info.IsDir() {
			return officeToolError("pptx_write", "invalid assets_dir: must be a directory")
		}
	}

	outputPath, err := ValidatePath(t.workDir, strings.TrimSpace(p.Path))
	if err != nil {
		return openagent.ErrorResult(fmt.Errorf("pptx_write: %w", err), false, "")
	}

	// Resolve the build script: prefer a file path (written by the write
	// tool) over inline script. A file path keeps large scripts out of the
	// tool call so the model doesn't bloat its thinking with script source.
	var scriptPath string
	var tempScriptCleanup func()
	if scriptPathInput != "" {
		scriptPath, err = ValidatePath(t.workDir, scriptPathInput)
		if err != nil {
			return openagent.ErrorResult(fmt.Errorf("pptx_write: script_path: %w", err), false, "")
		}
		if info, err := os.Stat(scriptPath); err != nil || info.IsDir() {
			return officeToolError("pptx_write", fmt.Sprintf("script_path not found or not a file: %s", scriptPath))
		}
	} else {
		// Inline script: write to a temp .mjs file.
		scriptFile, err := os.CreateTemp("", "openagent-pptx-build-*.mjs")
		if err != nil {
			return officeToolError("pptx_write", fmt.Sprintf("failed to create build script: %s", err.Error()))
		}
		scriptPath = scriptFile.Name()
		tempScriptCleanup = func() { os.Remove(scriptPath) }
		if _, err := scriptFile.WriteString(p.Script); err != nil {
			scriptFile.Close()
			tempScriptCleanup()
			return officeToolError("pptx_write", fmt.Sprintf("failed to write build script: %s", err.Error()))
		}
		if err := scriptFile.Close(); err != nil {
			tempScriptCleanup()
			return officeToolError("pptx_write", fmt.Sprintf("failed to close build script: %s", err.Error()))
		}
	}
	if tempScriptCleanup != nil {
		defer tempScriptCleanup()
	}

	// Extract the embedded worker bundle to a temp file.
	bundlePath, err := extractPptxWorkerBundle()
	if err != nil {
		return officeToolError("pptx_write", fmt.Sprintf("failed to extract worker: %s", err.Error()))
	}
	defer os.Remove(bundlePath)

	// Build the worker spec and write it to a temp JSON file (avoids
	// fragile command-line escaping of nested data).
	spec := struct {
		Path       string          `json:"path"`
		ScriptPath string          `json:"script_path"`
		AssetsDir  string          `json:"assets_dir,omitempty"`
		Data       json.RawMessage `json:"data,omitempty"`
	}{
		Path:       outputPath,
		ScriptPath: scriptPath,
		AssetsDir:  assetsDir,
		Data:       p.Data,
	}
	specFile, err := os.CreateTemp("", "openagent-pptx-spec-*.json")
	if err != nil {
		return officeToolError("pptx_write", fmt.Sprintf("failed to create spec file: %s", err.Error()))
	}
	defer os.Remove(specFile.Name())
	if err := json.NewEncoder(specFile).Encode(spec); err != nil {
		specFile.Close()
		return officeToolError("pptx_write", fmt.Sprintf("failed to write spec: %s", err.Error()))
	}
	if err := specFile.Close(); err != nil {
		return officeToolError("pptx_write", fmt.Sprintf("failed to close spec: %s", err.Error()))
	}

	// Run the Node worker.
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, nodePath, bundlePath, specFile.Name())
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		// Try to parse a structured error from stdout first.
		var result pptxWriteWorkerResult
		if len(bytes.TrimSpace(stdout)) > 0 && json.Unmarshal(bytes.TrimSpace(stdout), &result) == nil && result.Error != "" {
			return officeToolError("pptx_write", fmt.Sprintf("worker failed: %s", result.Error))
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return officeToolError("pptx_write", fmt.Sprintf("failed to run Node worker: %s", detail))
	}

	var result pptxWriteWorkerResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &result); err != nil {
		return officeToolError("pptx_write", fmt.Sprintf("failed to parse worker output: %s", err.Error()))
	}
	if !result.OK {
		return officeToolError("pptx_write", fmt.Sprintf("failed to write PowerPoint: %s", result.Error))
	}

	// Report file size for the model.
	var size int64
	if info, err := os.Stat(result.Path); err == nil {
		size = info.Size()
	}
	return officeToolText(fmt.Sprintf("Successfully wrote PowerPoint file: %s\n%d slide(s) written (%d bytes)",
		result.Path, result.SlideCount, size))
}

type pptxWriteWorkerResult struct {
	OK         bool   `json:"ok"`
	Path       string `json:"path"`
	SlideCount int    `json:"slideCount"`
	Mode       string `json:"mode"`
	Error      string `json:"error"`
}

// extractPptxWorkerBundle writes the embedded worker.bundle.mjs to a temp
// file and returns its path. The caller must remove it when done.
func extractPptxWorkerBundle() (string, error) {
	data, err := pptxWorkerBundleFS.ReadFile("pptx-worker/worker.bundle.mjs")
	if err != nil {
		return "", fmt.Errorf("read embedded worker: %w", err)
	}
	f, err := os.CreateTemp("", "openagent-pptx-worker-*.mjs")
	if err != nil {
		return "", fmt.Errorf("create temp worker file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write temp worker: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("close temp worker: %w", err)
	}
	return f.Name(), nil
}
