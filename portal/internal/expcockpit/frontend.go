package expcockpit

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"path"
	"strings"
)

const (
	defaultFrontendAssetBase = "/stellar/assets"
	defaultSnapshotPath      = "/api/stellar/snapshot"
	defaultSeriesPath        = "/api/stellar/series"
	defaultRefreshInterval   = "30000"
)

//go:embed assets/app.css assets/app.js
var frontendAssets embed.FS

type FrontendOptions struct {
	Target          string
	Workspace       string
	Project         string
	Metric          string
	AssetBase       string
	SnapshotPath    string
	SeriesPath      string
	Source          string
	RefreshInterval string
	// Embedded tightens the landing layout for same-origin iframe hosts (the
	// unified portal's Experiments board) so it fills the narrower frame instead
	// of keeping the standalone full-page margins. Full-page /stellar leaves it
	// false.
	Embedded bool
}

type FrontendAsset struct {
	Content     []byte
	ContentType string
}

type frontendShellData struct {
	Target          string
	Workspace       string
	Project         string
	Metric          string
	AssetBase       string
	CSSAssetURL     string
	JSAssetURL      string
	CriticalCSS     template.CSS
	SnapshotPath    string
	SeriesPath      string
	Source          string
	SourceLabel     string
	SourceTitle     string
	RefreshInterval string
	Embedded        bool
}

func RenderFrontendHTML(opts FrontendOptions) ([]byte, error) {
	target := strings.TrimSpace(opts.Target)
	metric := strings.TrimSpace(opts.Metric)
	assetBase := normalizedFrontendPath(opts.AssetBase, defaultFrontendAssetBase)
	cssAssetURL, err := frontendAssetURL(assetBase, "app.css")
	if err != nil {
		return nil, err
	}
	jsAssetURL, err := frontendAssetURL(assetBase, "app.js")
	if err != nil {
		return nil, err
	}
	source := strings.TrimSpace(opts.Source)
	data := frontendShellData{
		Target:          target,
		Workspace:       strings.TrimSpace(opts.Workspace),
		Project:         strings.TrimSpace(opts.Project),
		Metric:          metric,
		AssetBase:       assetBase,
		CSSAssetURL:     cssAssetURL,
		JSAssetURL:      jsAssetURL,
		CriticalCSS:     template.CSS(frontendCriticalCSS),
		SnapshotPath:    normalizedFrontendPath(opts.SnapshotPath, defaultSnapshotPath),
		SeriesPath:      normalizedFrontendPath(opts.SeriesPath, defaultSeriesPath),
		Source:          source,
		SourceLabel:     frontendSourceLabel(source),
		SourceTitle:     frontendSourceTitle(source),
		RefreshInterval: normalizedRefreshInterval(opts.RefreshInterval, defaultRefreshInterval),
		Embedded:        opts.Embedded,
	}
	tmpl, err := template.New("stellar-frontend").Parse(frontendShellTemplate)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func frontendAssetURL(assetBase, name string) (string, error) {
	asset, ok, err := ReadFrontendAsset(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("frontend asset %q is not embedded", name)
	}
	sum := sha256.Sum256(asset.Content)
	version := hex.EncodeToString(sum[:])[:12]
	return strings.TrimRight(assetBase, "/") + "/" + name + "?v=" + version, nil
}

func ReadFrontendAsset(name string) (FrontendAsset, bool, error) {
	name = strings.TrimSpace(strings.TrimPrefix(name, "/"))
	if name == "" || strings.Contains(name, "..") || strings.Contains(name, `\`) {
		return FrontendAsset{}, false, nil
	}
	clean := path.Clean(name)
	if clean != name {
		return FrontendAsset{}, false, nil
	}
	contentType := ""
	switch clean {
	case "app.css":
		contentType = "text/css; charset=utf-8"
	case "app.js":
		contentType = "text/javascript; charset=utf-8"
	default:
		return FrontendAsset{}, false, nil
	}
	content, err := frontendAssets.ReadFile("assets/" + clean)
	if err != nil {
		return FrontendAsset{}, false, err
	}
	return FrontendAsset{Content: content, ContentType: contentType}, true, nil
}

func normalizedFrontendPath(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	if !strings.HasPrefix(value, "/") {
		return "/" + value
	}
	return value
}

func normalizedRefreshInterval(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func frontendSourceLabel(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "kusto":
		return "Kusto/ADX"
	case "local":
		return "local expstore"
	case "auto":
		return "auto"
	default:
		return ""
	}
}

func frontendSourceTitle(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "kusto":
		return "Hosted scalar source: authoritative ADX/Kusto rows."
	case "local":
		return "Local/offline source: expstore packets, artifacts, and recovery state."
	case "auto":
		return "Auto source mode can merge local expstore data and Kusto scalar rows."
	default:
		return ""
	}
}

const frontendShellTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Stellar sweep</title>
<style>{{ .CriticalCSS }}</style>
<link rel="preload" href="{{ .CSSAssetURL }}" as="style" onload="this.onload=null;this.rel='stylesheet'">
<noscript><link rel="stylesheet" href="{{ .CSSAssetURL }}"></noscript>
</head>
<body{{ if .Embedded }} class="embedded"{{ end }}>
<noscript>
  Stellar requires JavaScript.
</noscript>
<main
  id="stellar-root"
  class="{{ if .Target }}stellar-app{{ end }}"
  data-target="{{ .Target }}"
  data-workspace="{{ .Workspace }}"
  data-project="{{ .Project }}"
  data-metric="{{ .Metric }}"
  data-snapshot-path="{{ .SnapshotPath }}"
  data-series-path="{{ .SeriesPath }}"
  data-source="{{ .Source }}"
  data-refresh-interval="{{ .RefreshInterval }}"
>
  {{ if .Target }}
  <div class="app-shell stellar-loading-shell" aria-busy="true">
    <header class="app-topbar">
      <div class="topbar-home">
        <strong>Stellar</strong>
      </div>
      <div class="topbar-title">
        <strong>{{ .Target }}</strong>
        <span>Loading experiment dashboard</span>
      </div>
      <div class="topbar-actions">
        {{ if .SourceLabel }}<span class="meta-pill source-status" title="{{ .SourceTitle }}"><b>{{ .SourceLabel }}</b> source</span>{{ end }}
        <span class="refresh-status loading">auto <b>loading</b></span>
      </div>
    </header>
    <div class="workspace">
      <aside class="variables-rail" aria-label="Loading dashboard controls">
        <section class="rail-top">
          <span class="rail-label">Runs</span>
          <div class="stellar-loading-line wide"></div>
          <div class="stellar-loading-line"></div>
        </section>
        <section class="rail-section">
          <p class="rail-label">Metric focus</p>
          <div class="stellar-loading-line wide"></div>
          <div class="stellar-loading-line short"></div>
        </section>
      </aside>
      <main class="report-canvas">
        <section class="panel-grid">
          <article class="panel wide stellar-loading-panel"><div class="panel-content"><p>Fetching metrics and chart summaries.</p></div></article>
          <article class="panel stellar-loading-panel"><div class="panel-content"><p>Preparing run controls.</p></div></article>
          <article class="panel stellar-loading-panel"><div class="panel-content"><p>Loading evidence.</p></div></article>
        </section>
      </main>
    </div>
  </div>
  {{ else }}
  <div class="boot-shell">
    <section class="boot-hero" aria-live="polite">
      <p class="eyebrow">Stellar</p>
      <h1>Loading Stellar</h1>
      <p>Loading experiments.</p>
    </section>
  </div>
  {{ end }}
</main>
<script type="module" src="{{ .JSAssetURL }}"></script>
</body>
</html>
`

const frontendCriticalCSS = `
:root {
  color-scheme: light;
  --bg: #f7f7f4;
  --rail: #fbfbf8;
  --panel: #ffffff;
  --line: #e3e0d8;
  --line-strong: #d1cdc2;
  --text: #242424;
  --muted: #6f6b63;
  --faint: #9a958b;
  --accent: #1aa6c8;
  --aim-primary: #1473e6;
  --aim-primary-10: #e8f1fc;
  --shadow: 0 1px 2px rgba(0, 0, 0, 0.04);
  font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  min-width: 0;
  background: var(--bg);
  color: var(--text);
  font-size: 14px;
}
button, input, select { font: inherit; }
a { color: var(--accent); }
.stellar-error {
  width: min(720px, calc(100vw - 48px));
  margin: 14vh auto;
  padding: 28px;
  border: 1px solid var(--line);
  border-radius: 8px;
  background: var(--panel);
  box-shadow: var(--shadow);
}
.boot-shell {
  width: min(1180px, calc(100vw - 96px));
  margin: 0 auto;
  padding: 24px 0;
  box-sizing: border-box;
}
body.embedded .boot-shell {
  width: 100%;
  padding: 16px;
}
.boot-hero {
  display: grid;
  gap: 14px;
  padding: 30px;
  border: 1px solid var(--line);
  border-radius: 20px;
  background: #fff;
  box-shadow: var(--shadow);
}
.boot-hero h1 {
  margin: 0;
  color: #242424;
  font-size: 36px;
  letter-spacing: -0.03em;
}
.boot-hero p {
  max-width: 680px;
  margin: 0;
  color: var(--muted);
  font-size: 15px;
  line-height: 1.5;
}
.stellar-app,
.app-shell {
  min-height: 100vh;
  background: var(--bg);
}
.app-topbar {
  display: grid;
  grid-template-columns: minmax(200px, 0.9fr) minmax(240px, 1fr) auto;
  align-items: center;
  gap: 12px;
  height: 46px;
  padding: 0 14px;
  border-bottom: 1px solid var(--line);
  background: #fff;
}
.topbar-home {
  display: flex;
  gap: 12px;
  align-items: center;
  min-width: 0;
  margin: 0;
  padding: 4px 6px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: inherit;
  font: inherit;
  text-align: left;
  cursor: pointer;
  transition: background 120ms ease;
}
.topbar-title {
  display: flex;
  gap: 10px;
  align-items: baseline;
  justify-content: center;
  min-width: 0;
  color: #3b3935;
}
.topbar-title strong,
.topbar-title span {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.topbar-title strong {
  flex: 0 0 auto;
  font-weight: 750;
}
.topbar-title span {
  min-width: 0;
  color: var(--muted);
  font-size: 13px;
  font-weight: 600;
}
.topbar-actions {
  display: flex;
  gap: 12px;
  align-items: center;
  min-width: 0;
  color: var(--muted);
  font-size: 13px;
}
.meta-pill {
  display: inline-flex;
  gap: 4px;
  align-items: center;
  min-height: 24px;
  padding: 3px 8px;
  border: 1px solid var(--line);
  border-radius: 999px;
  background: #fff;
  color: var(--muted);
  font-size: 12px;
  white-space: nowrap;
}
.meta-pill b {
  color: var(--text);
}
.meta-pill.warning {
  border-color: #f0a9a9;
  background: #fff5f5;
}
.refresh-status {
  display: inline-flex;
  gap: 7px;
  align-items: center;
  padding: 3px 4px 3px 9px;
  border: 1px solid var(--line);
  border-radius: 999px;
  background: #fff;
  color: var(--muted);
  white-space: nowrap;
}
.refresh-status.loading {
  border-color: #b7e5ef;
  background: #f0fbfd;
}
.refresh-status span {
  font-weight: 750;
}
.refresh-status b {
  color: #3f3b34;
  font-size: 12px;
  font-weight: 650;
}
.workspace {
  display: grid;
  grid-template-columns: clamp(224px, 22vw, 264px) minmax(0, 1fr);
  min-height: calc(100vh - 46px);
}
.variables-rail {
  position: sticky;
  top: 46px;
  height: calc(100vh - 46px);
  overflow-y: auto;
  border-right: 1px solid var(--line);
  background: #fff;
}
.rail-top {
  padding: 13px 12px 12px;
  border-bottom: 1px solid var(--line);
  background: linear-gradient(180deg, #fff, #fbfbf8);
}
.rail-section {
  padding: 16px 14px;
  border-bottom: 1px solid var(--line);
}
.rail-label {
  display: block;
  margin: 0 0 8px;
  color: var(--muted);
  font-size: 12px;
  font-weight: 650;
}
.rail-top .rail-label {
  display: inline-block;
  margin: 0;
  color: #3f3b34;
  font-size: 13px;
  font-weight: 760;
}
.report-canvas {
  min-width: 0;
  padding: 14px 14px 48px;
}
.eyebrow {
  margin: 0 0 3px;
  color: var(--faint);
  font-size: 11px;
  font-weight: 750;
  letter-spacing: 0.08em;
  text-transform: uppercase;
}
.panel-grid {
  display: grid;
  grid-template-columns: repeat(6, minmax(0, 1fr));
  gap: 14px;
}
.panel {
  grid-column: span 2;
  min-width: 0;
  overflow: hidden;
  border: 1px solid var(--line);
  border-radius: 5px;
  background: var(--panel);
  box-shadow: var(--shadow);
}
.panel.wide { grid-column: span 4; }
.panel-content { padding: 14px; }
.stellar-loading-panel {
  min-height: 168px;
}
.stellar-loading-panel p {
  margin: 0;
  color: var(--muted);
  line-height: 1.35;
}
.stellar-loading-line {
  height: 10px;
  width: 68%;
  margin: 10px 0;
  border-radius: 999px;
  background: linear-gradient(90deg, #ece8df, #f7f6f2, #ece8df);
}
.stellar-loading-line.wide { width: 100%; }
.stellar-loading-line.short { width: 44%; }
@media (max-width: 1320px) {
  .app-topbar { grid-template-columns: minmax(0, 1fr) auto; height: auto; min-height: 46px; }
  .app-topbar > .experiment-search { grid-column: 1 / -1; }
  .topbar-actions { flex-wrap: wrap; gap: 8px; justify-content: flex-end; }
  .panel { grid-column: span 3; }
  .panel.wide { grid-column: span 6; }
}
@media (max-width: 820px) {
  .report-canvas { padding: 10px 10px 36px; }
  .panel-grid { grid-template-columns: minmax(0, 1fr); }
  .panel,
  .panel.wide { grid-column: auto; }
}
@media (max-width: 640px) {
  .app-topbar { grid-template-columns: minmax(0, 1fr); }
  .app-topbar > .experiment-search { grid-column: auto; }
  .topbar-actions { justify-content: flex-start; }
  .workspace { grid-template-columns: minmax(0, 1fr); }
  .variables-rail { position: relative; top: auto; height: auto; border-right: 0; border-bottom: 1px solid var(--line); }
}
`
