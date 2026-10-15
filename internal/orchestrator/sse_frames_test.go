package orchestrator

import (
	"strings"
	"testing"
	"time"
)

func TestElementsFrameModeEscapesNewlines(t *testing.T) {
	got := elementsFrameMode("replace", "#field", "<p>hello\r\nevent: injected\rdata: bad\nworld</p>")
	want := "event: datastar-patch-elements\ndata: mode replace\ndata: selector #field\ndata: elements <p>hello\ndata: elements event: injected\ndata: elements data: bad\ndata: elements world</p>\n\n"
	if got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}
}

func TestRemoveFrame(t *testing.T) {
	if got, want := removeFrame("dialog-abc"), "event: datastar-patch-elements\ndata: mode remove\ndata: selector #dialog-abc\n\n"; got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}
}

func TestOperationID(t *testing.T) {
	first, second := newOperationID(), newOperationID()
	if !validOperation(first) || !validOperation(second) || first == second {
		t.Fatal("expected unique valid operations")
	}
	for _, op := range []string{"", strings.Repeat("a", 31), strings.Repeat("A", 32), strings.Repeat("a", 31) + "\n"} {
		if validOperation(op) {
			t.Fatalf("accepted %q", op)
		}
	}
}

func TestRelTimeExprThresholds(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, tt := range []struct {
		age  time.Duration
		want string
	}{
		{44 * time.Second, "just now"}, {45 * time.Second, "1m ago"}, {89 * time.Second, "1m ago"}, {90 * time.Second, "1m ago"},
		{2699 * time.Second, "44m ago"}, {2700 * time.Second, "1h ago"}, {5399 * time.Second, "1h ago"}, {5400 * time.Second, "1h ago"},
		{129599 * time.Second, "35h ago"}, {129600 * time.Second, "1d ago"},
	} {
		if got := relativeTime(now.Add(-tt.age), now); got != tt.want {
			t.Fatalf("%v: %q, want %q", tt.age, got, tt.want)
		}
	}
	expr := relTimeExpr(now)
	for _, threshold := range []string{"s<45?", "s<90?", "s<2700?", "s<5400?", "s<129600?", "/86400", "1700000000000"} {
		if !strings.Contains(expr, threshold) {
			t.Fatalf("missing %s in %s", threshold, expr)
		}
	}
}
