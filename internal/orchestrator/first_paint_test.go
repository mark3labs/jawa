package orchestrator

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func TestSharedIconIntrinsicGeometry(t *testing.T) {
	for _, helper := range []struct {
		name     string
		render   func(string) templ.Component
		variants []string
		size     int
	}{
		{"icon", icon, strings.Fields("archive eye board runs agents settings plus more search branch pr issue external copy warning check trash reset play retry logout key server"), 16},
		{"lane", laneIcon, strings.Fields("Todo Planning Fix Building Done unknown"), 15},
		{"tone", toneIcon, strings.Fields("running blocked failed ready queued unknown"), 15},
	} {
		for _, variant := range helper.variants {
			t.Run(helper.name+"/"+variant, func(t *testing.T) {
				var out bytes.Buffer
				if err := helper.render(variant).Render(t.Context(), &out); err != nil {
					t.Fatal(err)
				}
				root := strings.SplitN(out.String(), ">", 2)[0]
				for _, attr := range []string{fmt.Sprintf(`width="%d"`, helper.size), fmt.Sprintf(`height="%d"`, helper.size), `viewBox="0 0 24 24"`, `aria-hidden="true"`} {
					if !strings.Contains(root, attr) {
						t.Errorf("SVG root missing %s: %s", attr, root)
					}
				}
			})
		}
	}
}

func TestSharedFirstPaintHead(t *testing.T) {
	for name, component := range map[string]templ.Component{
		"head": head("Test"), "board": appPage(pageData{v: viewParams{View: viewBoard}}),
		"archive": appPage(pageData{v: viewParams{View: viewArchive}}), "auth": auth("csrf", "login", ""),
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := component.Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			html := out.String()
			start, end, stylesheet := strings.Index(html, "<style>"), strings.Index(html, "</style>"), strings.Index(html, `<link rel="stylesheet" href="/assets/style.css"`)
			if start < 0 || end < start || stylesheet < end {
				t.Fatal("fallback must precede ordinary stylesheet")
			}
			style := html[start:end]
			for _, rule := range []string{"background: #130f0b", "color: #eee2cf", "margin: 0", "13px/1.5", "system-ui", ".ic", ".lane-ic", ".tone-ic", "flex: none"} {
				if !strings.Contains(style, rule) {
					t.Errorf("fallback missing %q", rule)
				}
			}
			for _, hidden := range []string{"display: none", "visibility: hidden", "opacity: 0", "onload="} {
				if strings.Contains(style, hidden) {
					t.Errorf("fallback uses reveal gate %q", hidden)
				}
			}
			if !strings.Contains(html, `<script type="module" src="/assets/app.js">`) {
				t.Fatal("ordinary module script missing")
			}
			if name == "board" || name == "archive" {
				index := strings.Index(html, `href="/archive"`)
				if index < 0 {
					t.Fatal("Archive navigation link missing")
				}
				nav := html[index:]
				nav = strings.SplitN(nav, "</a>", 2)[0]
				if !strings.Contains(nav, `width="16" height="16"`) || !strings.Contains(nav, `M5 8v12h14V8M10 12h4`) {
					t.Fatal("Archive navigation glyph missing intrinsic geometry")
				}
			}
		})
	}
}
