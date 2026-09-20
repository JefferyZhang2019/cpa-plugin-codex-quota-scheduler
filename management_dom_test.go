package main

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
