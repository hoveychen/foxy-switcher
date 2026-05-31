package anthropic

import (
	"testing"
	"time"
)

// TestParseRetryAfter_CapAllowsHourLongBackoff guards the per-account backoff
// cap. Anthropic was observed handing a single rate-limited account a
// Retry-After of 3516s (~59m) on /api/oauth/usage; the old 30m cap truncated
// that to 30m, so the poller retried ~29m before the real cooldown ended and
// got re-throttled every cycle. The cap must be large enough to honor a
// ~1h Retry-After verbatim, while still bounding anything beyond 1h.
func TestParseRetryAfter_CapAllowsHourLongBackoff(t *testing.T) {
	// Real observed value: must pass through unchanged (below the 1h cap).
	if got := parseRetryAfter("3516", 60*time.Second); got != 3516*time.Second {
		t.Errorf("parseRetryAfter(3516s) = %v, want 3516s (must not truncate below the real cooldown)", got)
	}
	// Anything beyond the 1h cap is still bounded to 1h.
	if got := parseRetryAfter("7200", 60*time.Second); got != time.Hour {
		t.Errorf("parseRetryAfter(7200s) = %v, want 1h (cap)", got)
	}
}
