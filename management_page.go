package main

// management_page.go embeds the management UI (management_page.html) and
// renders it as the status page. Handlers live in management.go. The UI file
// is separate because its script uses JS template literals, which cannot be
// embedded in a Go raw string.

import (
	"encoding/json"
	"html/template"
	_ "embed"
)

// management_page.html is the embedded management UI. It lives in its own file
// because the page script uses JavaScript template literals (backticks), which
// cannot appear inside a Go raw string literal.
//
//go:embed management_page.html
var statusTemplateSource string

var statusTemplateV2 = template.Must(template.New("status-v5").Funcs(template.FuncMap{
	"json": jsonForTemplate,
}).Parse(statusTemplateSource))

func jsonForTemplate(v any) template.JS {
	raw, err := json.Marshal(v)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(raw)
}
