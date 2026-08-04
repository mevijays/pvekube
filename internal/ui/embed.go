// Package ui embeds all server-rendered templates into the binary so
// PVEKube ships as a single self-contained executable — no external asset
// directory to lose track of.
package ui

import (
	"embed"
	"fmt"
	"html/template"
)

//go:embed templates/*.html templates/partials/*.html
var templatesFS embed.FS

// Every page template defines {{define "content"}}...{{end}} and is parsed
// together with layout.html into its OWN template.Template instance (keyed
// by page name below). That's deliberate: html/template resolves
// {{template "content"}} by name within one parsed set, so if we parsed all
// pages into a single set every page's "content" block would collide.
// Rendering a page = pages[name].ExecuteTemplate(w, "layout", data).
var pageFiles = map[string]string{
	"login":             "templates/login.html",
	"setup":             "templates/setup.html",
	"dashboard":         "templates/dashboard.html",
	"prereqs":           "templates/prereqs.html",
	"proxmox":           "templates/proxmox.html",
	"templates":         "templates/templates.html",
	"clusters":          "templates/clusters.html",
	"cluster_detail":    "templates/cluster_detail.html",
	"cluster_resources": "templates/cluster_resources.html",
}

var pages = mustParsePages()

func mustParsePages() map[string]*template.Template {
	out := make(map[string]*template.Template, len(pageFiles))
	for name, file := range pageFiles {
		t, err := template.New("layout").Funcs(FuncMap).ParseFS(templatesFS, "templates/layout.html", "templates/nav.html", file)
		if err != nil {
			panic(fmt.Sprintf("ui: parsing page %q (%s): %v", name, file, err))
		}
		out[name] = t
	}
	return out
}

// Render executes the named page's layout with data.
func Render(w interface {
	Write([]byte) (int, error)
}, name string, data any) error {
	t, ok := pages[name]
	if !ok {
		return fmt.Errorf("ui: unknown page %q", name)
	}
	return t.ExecuteTemplate(w, "layout", data)
}

var FuncMap = template.FuncMap{}

// Partials are HTMX fragment responses: no <html>/layout wrapper, just the
// swapped-in element. Each partial file defines {{define "<name>"}}.
var partials = template.Must(template.New("partials").Funcs(FuncMap).ParseFS(templatesFS, "templates/partials/*.html"))

func RenderPartial(w interface {
	Write([]byte) (int, error)
}, name string, data any) error {
	return partials.ExecuteTemplate(w, name, data)
}
