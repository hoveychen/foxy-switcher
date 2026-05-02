package tui

import (
	"strings"
	"testing"
	"time"
)

// Activity events arriving from the daemon may contain newlines in their
// Message field — e.g. when the daemon stores a raw HTTP response body for a
// 429. The rendered table row must collapse those into single-line content,
// otherwise one event consumes multiple terminal rows and the body grows past
// p.height, scrolling the chip strip / table header off the top of the page.
func TestActivityRowSingleLine(t *testing.T) {
	p := newActivityPage()
	p.setSize(120, 24)
	ev := ActivityEvent{
		Timestamp: 0,
		Type:      "error.usage",
		Severity:  "error",
		Message: `Usage poll for x@y.com failed: /api/oauth/usage: 429 {
  "error": {
    "type": "rate_limit_error",
    "message": "Rate limit exceeded"
  }
}`,
	}
	row := p.renderRow(ev, p.tableColumns())
	if strings.Contains(row, "\n") {
		t.Fatalf("renderRow produced a multi-line cell — table layout will overflow.\nrow=%q", row)
	}
}

// Reinforces the same invariant at the page level: total view height must
// match p.height regardless of whether events carry embedded newlines. With
// many multi-line events, an unsanitised renderer would emit far more lines
// than the page area, scrolling chips and the table header off the top.
func TestActivityViewHeightWithMultilineMessage(t *testing.T) {
	p := newActivityPage()
	p.setSize(120, 24)
	multiline := "Usage poll failed: 429 {\n  \"error\": {\n    \"type\": \"rate_limit_error\"\n  }\n}"
	for i := 0; i < 30; i++ {
		p.events = append(p.events, ActivityEvent{
			Type:     "error.usage",
			Severity: "error",
			Message:  multiline,
		})
	}
	p.loadedAt = time.Now()
	v := p.view()
	got := strings.Count(v, "\n") + 1
	if got != 24 {
		t.Fatalf("view height = %d, want 24 (multi-line messages must not push body past p.height)", got)
	}
}
