package orchestrator

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSeparatedViewsAndSnapshotScope(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	p, err := a.s.CreateProjectDetails("Kit", "github", "https://github.com/example/kit", "main")
	if err != nil {
		t.Fatal(err)
	}
	other, err := a.s.CreateProjectDetails("Other", "github", "https://github.com/example/other", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.s.CreateCard(p.ID, "Selected card", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = a.s.CreateCard(other.ID, "Other project card", ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, root string
		absent     []string
	}{
		{"/board?project=" + p.ID, "board-content", []string{`id="runs-content"`, `id="agents-content"`, `id="settings-content"`, `data-title="Other project card"`}},
		{"/runs", "runs-content", []string{`id="board-content"`, `id="agents-content"`, `id="settings-content"`}},
		{"/agents", "agents-content", []string{`id="board-content"`, `id="runs-content"`, `id="settings-content"`}},
		{"/settings", "settings-content", []string{`id="board-content"`, `id="runs-content"`, `id="agents-content"`}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := request(a, "GET", tc.path, nil, session, csrf)
			status(t, w, 200)
			body := w.Body.String()
			if !strings.Contains(body, `id="`+tc.root+`"`) || !strings.Contains(body, `id="agent-status"`) {
				t.Fatal("missing view root or live agent chip")
			}
			for _, s := range tc.absent {
				if strings.Contains(body, s) {
					t.Fatalf("unrelated view content: %s", s)
				}
			}
		})
	}
	for _, v := range []viewKind{viewBoard, viewRuns, viewAgents, viewSettings} {
		r := httptest.NewRequest("GET", "/snapshot?view="+string(v)+"&project="+p.ID, nil)
		r.AddCookie(csrf)
		html, err := Snapshot(r, a.s, a.workflow, parseViewParams(r, ""))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(html, `id="agent-status"`) || strings.Contains(html, `class="shell"`) {
			t.Fatal("snapshot replaced shell or omitted chip")
		}
		if v == viewBoard && strings.Contains(html, `data-title="Other project card"`) {
			t.Fatal("project scope leaked")
		}
	}
}

func TestViewParamsRejectUntrustedInput(t *testing.T) {
	r := httptest.NewRequest("GET", "/events?view=evil&project=javascript:alert(1)&state=unknown&card=../../etc", nil)
	v := parseViewParams(r, "")
	if v.View != viewBoard || v.Project != "" || v.Card != "" || v.State != "all" {
		t.Fatalf("unsafe view values: %+v", v)
	}
	if v.Path() != "/board" {
		t.Fatal(v.Path())
	}
}
