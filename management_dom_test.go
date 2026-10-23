package main

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestStatusHTMLScriptHasNoCollapsedSeparators guards against the v0.4.0
// breakage class: patching the embedded TRANSLATIONS dictionaries can emit a
// doubled separator (",,"), a hard syntax error that kills the whole script —
// every button dies exactly like the v0.3.0 missing-id breakage, but no
// getElementById probe can catch it. The page script must not contain
// collapsed ASCII separators.
func TestStatusHTMLScriptHasNoCollapsedSeparators(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/status"}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d; body=%s", resp.StatusCode, resp.Body)
	}
	html := string(resp.Body)
	scriptStart := strings.Index(html, "<script>")
	if scriptStart < 0 {
		t.Fatal("page has no embedded script")
	}
	script := html[scriptStart:]
	for _, artifact := range []string{",,", "{,", "[,", ",}", ",]"} {
		if idx := strings.Index(script, artifact); idx >= 0 {
			start := idx - 60
			if start < 0 {
				start = 0
			}
			end := idx + 60
			if end > len(script) {
				end = len(script)
			}
			t.Fatalf("script contains collapsed separator %q near %q", artifact, script[start:end])
		}
	}
}

// TestDumpPageScript writes the embedded management page's script to the
// path in DUMP_PAGE_SCRIPT. CI runs `node --check` on the output: the script
// only ever executes in a real browser, so a syntax error (the v0.4.0
// double-comma/dropped-semicolon class) kills every button and no Go
// assertion on strings can catch it.
func TestDumpPageScript(t *testing.T) {
	out := os.Getenv("DUMP_PAGE_SCRIPT")
	if out == "" {
		t.Skip("set DUMP_PAGE_SCRIPT=<path> to export the page script")
	}
	store := NewPluginState(DefaultConfig())
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/status"}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d; body=%s", resp.StatusCode, resp.Body)
	}
	html := string(resp.Body)
	start := strings.Index(html, "<script>")
	end := strings.LastIndex(html, "</script>")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("page has no embedded script")
	}
	script := html[start+len("<script>") : end]
	if err := os.WriteFile(out, []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

// TestStatusHTMLScriptReferencesResolve guards against the v0.3.0 breakage
// class: the embedded script references an element id the template no longer
// defines (or never defined), the reference returns null, and the first
// property write kills applyStatus — hiding the whole protected area. Every
// getElementById reference in the script must resolve to a static id in the
// rendered HTML.
func TestStatusHTMLScriptReferencesResolve(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/status"}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d; body=%s", resp.StatusCode, resp.Body)
	}
	html := string(resp.Body)

	idPattern := regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`)
	ids := make(map[string]struct{})
	for _, match := range idPattern.FindAllStringSubmatch(html, -1) {
		ids[match[1]] = struct{}{}
	}
	refPattern := regexp.MustCompile(`getElementById\('([A-Za-z0-9_-]+)'\)`)
	refs := refPattern.FindAllStringSubmatch(html, -1)
	if len(refs) == 0 {
		t.Fatal("no getElementById references found; the script extraction is broken")
	}
	missing := make([]string, 0)
	for _, match := range refs {
		if _, ok := ids[match[1]]; !ok {
			missing = append(missing, match[1])
		}
	}
	if len(missing) > 0 {
		t.Fatalf("script references missing from the template: %s", strings.Join(missing, ", "))
	}
}
