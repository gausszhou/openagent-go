---
name: powerpoint
description: Create designed, editable PowerPoint .pptx presentations with PptxGenJS-Plus. Use when the user asks to create, generate, update, or inspect a deck, slide deck, presentation, or .pptx file.
---

# PowerPoint

Use this skill whenever a PowerPoint deck is involved. For new decks, pass a PptxGenJS build script to the `pptx_write` tool — inline via `script` (preferred, for small decks) or via `script_path` for large decks whose script is written to a `.mjs` file. For filling or editing an existing template, call `pptx_template_analyze` first and then `pptx_template_fill` with the exact IDs returned by analysis.

## Workflow

1. Decide the deck outline and choose a visual system: palette, typography, repeated motif, and slide rhythm.
2. Write JavaScript module content that exports `default async function build(pptx, ctx)` or named `build(pptx, ctx)`.
3. In the script, add slides directly with PptxGenJS. Do not generate HTML for this workflow.
4. Pass the script to `pptx_write`. **Prefer inline** (`script`) for small decks — pass `path`, `script`, optional `assets_dir`, and optional `data`. For large decks (many slides), writing the whole script inline bloats the tool call; instead write it to a `.mjs` file with the write tool (first call creates it, subsequent calls use `append=true` to add chunks), then pass `path` + `script_path`.
5. Verify the result with `pptx_read`; for visual QA, convert the PPTX to images if the environment has LibreOffice and Poppler.

## Template Workflow

- Use `pptx_template_analyze` when the user provides a `.pptx` template or wants to preserve existing layouts, charts, images, tables, or SmartArt.
- Build a `template_fill_pptx_plan.v1` plan from the returned slide IDs and object IDs, then call `pptx_template_fill`.
- For SmartArt, use `smartarts[*].smartart_id` and `smartarts[*].nodes[*].node_id` in `smartart_edits`. This edits existing node text only; it does not create, delete, or relayout SmartArt nodes.

## Script Creation

- **Build environment.** The script runs in a Node.js worker spawned from a temp directory that has no `node_modules`. PptxGenJS-Plus, JSZip, and pako are already bundled into the worker, so they reach the script only as the `pptx` instance and the `ctx` argument of `build(pptx, ctx)` — not as importable modules. Node built-ins (`node:fs`, `node:path`, …) resolve from the temp file; npm packages do not, since there is no `node_modules` to resolve against.
- **Inline (preferred)**: put the complete JavaScript module in the `script` argument. Best for small decks where the script is short.
- **File path (large decks)**: when the script is large, writing it inline bloats the tool call and the model's thinking with script source. Instead use the write tool to create a `.mjs` file — first call writes it, subsequent calls pass `append=true` to append chunks — then pass its path as `script_path`. Either way the build function signature is the same: `default async function build(pptx, ctx)`.
- If revising a deck, update the script (inline `script` or the `.mjs` file) and call `pptx_write` again.

## Tool Contract

Inline (small deck):

```json
{
  "tool": "pptx_write",
  "arguments": {
    "path": "deck.pptx",
    "script": "export default async function build(pptx, ctx) {\\n  pptx.layout = \"LAYOUT_WIDE\";\\n}",
    "assets_dir": "/absolute/path/to/assets",
    "data": {"title": "Quarterly Review"}
  }
}
```

File path (large deck — script written to a `.mjs` file via the write tool):

```json
{
  "tool": "pptx_write",
  "arguments": {
    "path": "deck.pptx",
    "script_path": "/tmp/deck.mjs",
    "data": {"title": "Quarterly Review"}
  }
}
```

Pass exactly one of `script` or `script_path`. The worker creates the PptxGenJS instance and writes the output file. The script only adds slides and content.

```javascript
export default async function build(pptx, ctx) {
  pptx.layout = "LAYOUT_WIDE";
  pptx.author = "OpenAgent";

  const slide = pptx.addSlide();
  slide.background = { color: "FFFFFF" };
  slide.addText("Title", {
    x: 0.6, y: 0.4, w: 8, h: 0.6,
    fontSize: 36, bold: true, color: "1F2937",
    margin: 0,
  });
  slide.addNotes("speaker notes");
}
```

`ctx` includes:

- `ctx.data`: JSON data passed from the tool call.
- `ctx.assetsDir`: resolved asset directory.
- `ctx.outPath`: final PPTX path.
- `ctx.resolveAsset("image.png")`: absolute path under `assets_dir`.
- `ctx.imageData("image.png")`: base64 image data URL.
- `ctx.iconSvgData("check", "16A34A")`: Font Awesome solid icon as SVG data.

## Design Rules

- **Set slide background explicitly on every slide.** PptxGenJS defaults to a black background; if you do not set `slide.background = { color: "FFFFFF" }`, slides will render with a black background. Always set the background color even for "white" slides.
- Avoid plain white bullet decks. Every slide should have a visual element: shape, image, chart, icon, timeline, stat callout, or diagram.
- Vary layouts across the deck: title, divider, two-column, card grid, process flow, quote/callout, and conclusion.
- Pick topic-specific colors. Use one dominant color, one or two supporting tones, and one accent.
- Use strong hierarchy: titles around 36-44 pt, section labels around 20-24 pt, body text around 14-18 pt.
- Keep at least 0.5 inch margins and consistent gaps around 0.3-0.5 inch.
- Use editable text wherever practical; use images for photos, screenshots, logos, or complex visual backgrounds.
- Add speaker notes when useful; `pptx_read` can surface them later.

## PptxGenJS Reference

Use this reference when writing the JavaScript build script for the `pptx_write` tool.

### Basic Structure

```javascript
export default async function build(pptx, ctx) {
  pptx.layout = "LAYOUT_WIDE";
  pptx.title = ctx.data?.title || "Presentation";

  const slide = pptx.addSlide();
  slide.background = { color: "FFFFFF" };
  slide.addText("Hello", { x: 0.6, y: 0.5, w: 8, h: 0.6, fontSize: 36 });
}
```

Useful layouts: `LAYOUT_WIDE` (13.333 x 7.5 in), `LAYOUT_16X9` (10 x 5.625 in), `LAYOUT_4X3` (10 x 7.5 in). Dimensions accept inches (plain numbers) or unit-suffixed strings:

```javascript
// inches (default — plain number)
slide.addText("Title", { x: 0.6, y: 0.4, w: 8, h: 0.6, fontSize: 36 });

// centimeters / millimeters / points
slide.addText("Title", { x: "1.5cm", y: "1cm", w: "20cm", h: "1.5cm", fontSize: 36 });
slide.addShape(pptx.ShapeType.rect, { x: "0mm", y: "0mm", w: "338mm", h: "190mm" });
```

### Text

```javascript
slide.addText("Main title", {
  x: 0.6, y: 0.4, w: 8.5, h: 0.6,
  fontFace: "Aptos Display", fontSize: 38, bold: true, color: "111827", margin: 0,
});

slide.addText([
  { text: "First point", options: { bullet: true, breakLine: true } },
  { text: "Second point", options: { bullet: true } },
], { x: 0.8, y: 1.4, w: 5.4, h: 1.2, fontSize: 17, color: "374151", paraSpaceAfterPt: 8 });
```

- Use `margin: 0` when aligning text with shapes or icons.
- Use PptxGenJS bullets; do not type bullet glyphs into strings.
- Use `breakLine: true` in rich text arrays when items must appear on separate lines.

### Shapes

```javascript
slide.addShape(pptx.ShapeType.rect, {
  x: 0, y: 0, w: 13.333, h: 7.5, fill: { color: "F8FAFC" }, line: { color: "F8FAFC" },
});

slide.addShape(pptx.ShapeType.roundRect, {
  x: 0.7, y: 1.2, w: 3.5, h: 1.2, rectRadius: 0.08, fill: { color: "FFFFFF" },
  line: { color: "E5E7EB", width: 1 },
  shadow: { type: "outer", color: "000000", opacity: 0.12, blur: 2, angle: 45, distance: 1 },
});
```

- Hex colors must not include `#`. Do not use 8-character hex for transparency — use `transparency` or `opacity`.
- Use a fresh options object for each shape; PptxGenJS mutates some option values internally.
- Preset shadows: `shadow: { type: "preset", preset: "shdw1" }` (shdw1–shdw20).
- Picture fills on shapes: `fill: { type: "image", image: { data: ctx.imageData("bg.png"), sizing: { type: "cover" } } }`.
- Connectors: `slide.addConnector({ x: 0.7, y: 6.8, w: 11.8, h: 0, line: { color: "CBD5E1", width: 1 } })`.

### Images and Icons

```javascript
slide.addImage({ data: ctx.imageData("photo.png"), x: 7.1, y: 1.0, w: 5.4, h: 3.4,
  sizing: { type: "cover", x: 7.1, y: 1.0, w: 5.4, h: 3.4 } });

slide.addImage({ data: ctx.iconSvgData("chart-line", "2563EB"), x: 0.8, y: 1.2, w: 0.35, h: 0.35 });
```

- `ctx.resolveAsset()`: file path for PptxGenJS APIs that need a path.
- `ctx.imageData()`: base64 data URL for local PNG/JPG/GIF/SVG.
- `ctx.iconSvgData(name, color)`: Font Awesome **free-solid** icon as SVG. Only icons in `@fortawesome/free-solid-svg-icons` are available — using an icon name that does not exist in that package will fail. Safe icon names include: `check`, `check-circle`, `check-double`, `xmark`, `circle-xmark`, `triangle-exclamation`, `circle-info`, `circle-question`, `chart-line`, `chart-bar`, `chart-pie`, `arrow-right`, `arrow-up`, `arrow-down`, `arrow-left`, `chevron-right`, `chevron-up`, `chevron-down`, `plus`, `minus`, `star`, `heart`, `bolt`, `fire`, `lightbulb`, `gear`, `cog`, `wrench`, `code`, `terminal`, `database`, `server`, `cloud`, `shield-halved`, `lock`, `key`, `user`, `users`, `building`, `briefcase`, `rocket`, `target`, `flag`, `trophy`, `magnifying-glass`, `download`, `upload`, `trash`, `pen`, `clipboard`, `book`, `book-open`, `graduation-cap`, `certificate`, `id-card`, `envelope`, `phone`, `globe`, `link`, `clock`, `calendar`, `map`, `location-dot`, `camera`, `image`, `video`, `microphone`, `headphones`, `play`, `pause`, `stop`, `forward`, `backward`, `volume-high`, `wifi`, `plug`, `battery-full`, `bell`, `comment`, `comments`, `message`, `thumbs-up`, `thumbs-down`, `face-smile`, `face-frown`. When in doubt, use `circle-info` or omit the icon.

### Tables and Charts

```javascript
slide.addTable([["Metric", "Current", "Target"], ["Activation", "42%", "55%"]], {
  x: 0.7, y: 1.5, w: 6.0, h: 1.2, border: { pt: 1, color: "E5E7EB" }, fontSize: 12,
});

slide.addChart(pptx.ChartType.bar, [{ name: "Revenue", labels: ["Q1","Q2","Q3","Q4"], values: [12,16,21,28] }], {
  x: 0.7, y: 1.2, w: 6.2, h: 3.8, barDir: "col", chartColors: ["2563EB"], showValue: true,
});
```

ChartEx types (PowerPoint 2016+): `pptx.ChartExType.funnel`, `treemap`, `sunburst`, `waterfall`, `histogram`, `boxWhisker`:

```javascript
slide.addChart(pptx.ChartExType.funnel, [{ name: "Pipeline", labels: ["Leads","Qualified","Demo","Close"], values: [1000,400,120,30] }], {
  x: 0.7, y: 1.2, w: 6.2, h: 3.8, chartColors: ["2563EB","3B82F6","60A5FA","93C5FD"],
});
```

- Table diagonal borders: `borderDiagonalDown: { color: "E5E7EB", pt: 1 }`.
- Text fields for page numbers: `slide.addText({ text: "1", options: { field: "slidenum" } })`.

### Layout Ideas

Cover (full color/image + large title), Agenda (sidebar + numbered sections), Two-column (text + visual), Card grid (2x2/3x2 with icon+header+body), Timeline/process (numbered steps), Data slide (large chart + stat callouts), Closing (statement/next action).

## Required QA

- Run `pptx_read` on the generated file and check slide order, missing text, typo risk, and notes.
- Inspect generated XML or render slides when visual precision matters.
- Watch for overlap, text overflow, low contrast, cramped spacing, repeated layouts, and leftover placeholder text.
- If a visual issue is found, edit the `.mjs` script and rewrite the PPTX.
