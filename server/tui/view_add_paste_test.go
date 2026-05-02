package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// TestViewAddPasteShowsFullURL guards against the truncation bug: the OAuth
// authorize URL is much wider than the panel, so a single-line render would
// elide the tail with `…` and leave the user unable to copy it. Wrapping
// across lines keeps every character visible.
func TestViewAddPasteShowsFullURL(t *testing.T) {
	longURL := "https://claude.ai/oauth/authorize?client_id=9d1c250a-e61b-44d9-8888-1234567890ab&redirect_uri=https://claude.ai/callback&response_type=code&scope=user&state=abc123def456ghi789"
	ti := textinput.New()
	ti.Placeholder = "Paste code#state from browser"
	m := &model{
		width:      120,
		height:     30,
		mode:       modeAddPaste,
		pendingURL: longURL,
		addPaste:   ti,
	}

	out := m.viewAddPaste()
	plain := ansiRE.ReplaceAllString(out, "")

	if strings.Contains(plain, "…") {
		t.Fatalf("rendered output still truncates with ellipsis:\n%s", plain)
	}

	// Reconstruct the URL by stripping panel borders, padding spaces, and
	// newlines. The URL itself contains none of these, so concatenating the
	// remaining glyphs must yield the original string.
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '│', '╭', '╮', '╰', '╯', '─':
			return -1
		}
		return r
	}, plain)

	if !strings.Contains(cleaned, longURL) {
		t.Fatalf("URL not fully visible after wrapping; cleaned output:\n%s\n\nwant substring:\n%s", cleaned, longURL)
	}
}

// TestViewAddPasteRendersCopyConfirmation guards the visible feedback shown
// after ctrl+y: without a status line in the modal the user has no way to
// tell whether the clipboard write actually happened.
func TestViewAddPasteRendersCopyConfirmation(t *testing.T) {
	ti := textinput.New()
	m := &model{
		width:           120,
		height:          30,
		mode:            modeAddPaste,
		pendingURL:      "https://example.com/oauth?x=1",
		addPaste:        ti,
		statusMsg:       "URL copied to clipboard",
		statusExpiresAt: time.Now().Add(5 * time.Second),
	}

	out := m.viewAddPaste()
	plain := ansiRE.ReplaceAllString(out, "")

	if !strings.Contains(plain, "URL copied to clipboard") {
		t.Fatalf("add-paste view does not surface statusMsg toast after ctrl+y:\n%s", plain)
	}
}

// TestViewAddPasteRendersCopyError guards the error-path feedback so a
// clipboard failure on a sandboxed/headless host doesn't appear as a silent
// no-op.
func TestViewAddPasteRendersCopyError(t *testing.T) {
	ti := textinput.New()
	m := &model{
		width:           120,
		height:          30,
		mode:            modeAddPaste,
		pendingURL:      "https://example.com/oauth?x=1",
		addPaste:        ti,
		statusErr:       "copy URL: clipboard unavailable",
		statusExpiresAt: time.Now().Add(5 * time.Second),
	}

	out := m.viewAddPaste()
	plain := ansiRE.ReplaceAllString(out, "")

	if !strings.Contains(plain, "copy URL: clipboard unavailable") {
		t.Fatalf("add-paste view does not surface statusErr after failed copy:\n%s", plain)
	}
}
