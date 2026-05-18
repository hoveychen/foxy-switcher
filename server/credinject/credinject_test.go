package credinject

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

func TestBuildOAuthBlob_FieldShape(t *testing.T) {
	a := &store.Account{
		AccessToken:      "sk-ant-oat01-aaaa",
		RefreshToken:     "sk-ant-ort01-bbbb",
		ExpiresAt:        1700000000000,
		Scopes:           "user:inference user:profile",
		SubscriptionType: "max",
		OrganizationUUID: "11111111-2222-3333-4444-555555555555",
	}
	raw, err := buildOAuthBlob(a)
	if err != nil {
		t.Fatalf("buildOAuthBlob: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["organizationUuid"] != a.OrganizationUUID {
		t.Errorf("organizationUuid: got %q want %q", got["organizationUuid"], a.OrganizationUUID)
	}
	oauth, ok := got["claudeAiOauth"].(map[string]any)
	if !ok {
		t.Fatalf("claudeAiOauth missing or wrong type: %#v", got["claudeAiOauth"])
	}
	if oauth["accessToken"] != a.AccessToken {
		t.Errorf("accessToken mismatch")
	}
	if oauth["refreshToken"] != a.RefreshToken {
		t.Errorf("refreshToken mismatch")
	}
	// JSON deserialises numbers as float64; cast back for comparison.
	if int64(oauth["expiresAt"].(float64)) != a.ExpiresAt {
		t.Errorf("expiresAt mismatch: %v", oauth["expiresAt"])
	}
	if oauth["clientId"] != claudeCodeClientID {
		t.Errorf("clientId not pinned: got %q", oauth["clientId"])
	}
	if oauth["rateLimitTier"] != "default_claude_max_5x" {
		t.Errorf("rateLimitTier for max: got %q", oauth["rateLimitTier"])
	}
	if oauth["subscriptionType"] != "max" {
		t.Errorf("subscriptionType passthrough failed")
	}
	scopes, ok := oauth["scopes"].([]any)
	if !ok {
		t.Fatalf("scopes not an array: %#v", oauth["scopes"])
	}
	if len(scopes) != 2 || scopes[0] != "user:inference" || scopes[1] != "user:profile" {
		t.Errorf("scopes parse: got %v", scopes)
	}
}

func TestParseScopes(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{"user:inference", "user:profile"}},
		{"   ", []string{"user:inference", "user:profile"}},
		{"user:inference", []string{"user:inference"}},
		{"user:inference user:profile org:read", []string{"user:inference", "user:profile", "org:read"}},
		{"  user:inference   user:profile  ", []string{"user:inference", "user:profile"}},
	}
	for _, tc := range cases {
		if got := parseScopes(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseScopes(%q): got %v want %v", tc.in, got, tc.want)
		}
	}
}

func TestDeriveRateLimitTier(t *testing.T) {
	cases := map[string]string{
		"max":          "default_claude_max_5x",
		"team":         "default_claude_max_5x",
		"team_premium": "default_claude_max_20x",
		"pro":          "default_claude_pro",
		"":             "",
		"weird-value":  "",
	}
	for sub, want := range cases {
		if got := deriveRateLimitTier(sub); got != want {
			t.Errorf("deriveRateLimitTier(%q): got %q want %q", sub, got, want)
		}
	}
}

func TestExtractAccessToken(t *testing.T) {
	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-XYZ","refreshToken":"r"}}`)
	if got := extractAccessToken(blob); got != "sk-ant-oat01-XYZ" {
		t.Errorf("extractAccessToken: got %q", got)
	}
	if got := extractAccessToken([]byte("not json")); got != "" {
		t.Errorf("malformed input: expected empty, got %q", got)
	}
}

func TestExtractRotation(t *testing.T) {
	blob := []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1700000000000}}`)
	at, rt, exp, ok := extractRotation(blob)
	if !ok {
		t.Fatalf("extractRotation: ok=false")
	}
	if at != "a" || rt != "r" || exp != 1700000000000 {
		t.Errorf("extractRotation: got (%q,%q,%d)", at, rt, exp)
	}
}

// mockMarkerBackend is a Backend stub used by the VerifyMarker tests. It only
// implements CredentialsPath meaningfully; the other methods exist to satisfy
// the interface and are not invoked by VerifyMarker.
type mockMarkerBackend struct {
	path string
}

func (m *mockMarkerBackend) ReadOAuthBlob() ([]byte, bool, error)     { return nil, false, nil }
func (m *mockMarkerBackend) WriteOAuthBlob(blob []byte) error         { return nil }
func (m *mockMarkerBackend) DeleteOAuthBlob() error                   { return nil }
func (m *mockMarkerBackend) ReadManagedAPIKey() (string, bool, error) { return "", false, nil }
func (m *mockMarkerBackend) WriteManagedAPIKey(key string) error      { return nil }
func (m *mockMarkerBackend) DeleteManagedAPIKey() error               { return nil }
func (m *mockMarkerBackend) CredentialsPath() string                  { return m.path }

func newMarkerCoordinator(t *testing.T) (*Coordinator, *mockMarkerBackend, string) {
	t.Helper()
	dir := t.TempDir()
	mb := &mockMarkerBackend{path: filepath.Join(dir, "credentials.json")}
	c := &Coordinator{
		backend: mb,
		dataDir: dir,
		clock:   time.Now,
	}
	return c, mb, dir
}

func TestInjectFoxyMarker_AddsTopLevelMarker(t *testing.T) {
	original := []byte(`{"claudeAiOauth":{"accessToken":"a"},"organizationUuid":"u"}`)
	out, id, injected := injectFoxyMarker(original)
	if !injected {
		t.Fatalf("expected injected=true on valid JSON object")
	}
	if len(id) != 16 {
		t.Errorf("marker id length: got %d want 16", len(id))
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("rewritten blob not valid JSON: %v", err)
	}
	if parsed[markerKey] != id {
		t.Errorf("marker not at top level: %#v", parsed)
	}
	if _, ok := parsed["claudeAiOauth"].(map[string]any); !ok {
		t.Errorf("claudeAiOauth lost: %#v", parsed)
	}
	if parsed["organizationUuid"] != "u" {
		t.Errorf("organizationUuid lost: %#v", parsed["organizationUuid"])
	}
}

func TestInjectFoxyMarker_RejectsNonObject(t *testing.T) {
	cases := [][]byte{
		[]byte("not json at all"),
		[]byte(`["array","not","object"]`),
		[]byte(`"a bare string"`),
		[]byte("null"),
	}
	for _, blob := range cases {
		out, id, injected := injectFoxyMarker(blob)
		if injected {
			t.Errorf("expected injected=false for %q", blob)
		}
		if id != "" {
			t.Errorf("expected empty id, got %q", id)
		}
		if string(out) != string(blob) {
			t.Errorf("expected unchanged blob, got %q", out)
		}
	}
}

func TestVerifyMarker_Intact(t *testing.T) {
	c, mb, dir := newMarkerCoordinator(t)
	const markerID = "abc1234567890def"
	blob := []byte(`{"claudeAiOauth":{"accessToken":"a"},"__foxy_marker":"` + markerID + `"}`)
	if err := os.WriteFile(mb.path, blob, 0o600); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
	if err := writeLastWrite(dir, lastWriteFile{MarkerID: markerID, AccountID: 42, WrittenAt: 1}); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}
	state, err := c.VerifyMarker()
	if err != nil {
		t.Fatalf("VerifyMarker: %v", err)
	}
	if state != MarkerStateIntact {
		t.Errorf("state: got %s want intact", state)
	}
}

func TestVerifyMarker_Overwritten(t *testing.T) {
	c, mb, dir := newMarkerCoordinator(t)
	if err := writeLastWrite(dir, lastWriteFile{MarkerID: "ours", AccountID: 7}); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}
	// Credentials file exists but marker is missing — Claude Code rewrote it.
	if err := os.WriteFile(mb.path, []byte(`{"claudeAiOauth":{"accessToken":"new"}}`), 0o600); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
	state, err := c.VerifyMarker()
	if err != nil {
		t.Fatalf("VerifyMarker: %v", err)
	}
	if state != MarkerStateOverwritten {
		t.Errorf("missing marker: got %s want overwritten", state)
	}

	// Marker present but doesn't match sidecar — should also be Overwritten.
	if err := os.WriteFile(mb.path, []byte(`{"claudeAiOauth":{},"__foxy_marker":"someoneelse"}`), 0o600); err != nil {
		t.Fatalf("rewrite credentials: %v", err)
	}
	state, err = c.VerifyMarker()
	if err != nil {
		t.Fatalf("VerifyMarker: %v", err)
	}
	if state != MarkerStateOverwritten {
		t.Errorf("mismatched marker: got %s want overwritten", state)
	}
}

func TestVerifyMarker_Missing(t *testing.T) {
	c, _, dir := newMarkerCoordinator(t)
	if err := writeLastWrite(dir, lastWriteFile{MarkerID: "ours", AccountID: 7}); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}
	// Credentials file never created.
	state, err := c.VerifyMarker()
	if err != nil {
		t.Fatalf("VerifyMarker: %v", err)
	}
	if state != MarkerStateMissing {
		t.Errorf("state: got %s want missing", state)
	}
}

func TestVerifyMarker_SidecarMissing(t *testing.T) {
	c, mb, _ := newMarkerCoordinator(t)
	if err := os.WriteFile(mb.path, []byte(`{"claudeAiOauth":{},"__foxy_marker":"orphan"}`), 0o600); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
	state, err := c.VerifyMarker()
	if err != nil {
		t.Fatalf("VerifyMarker: %v", err)
	}
	if state != MarkerStateSidecarMissing {
		t.Errorf("state: got %s want sidecar-missing", state)
	}
}

func TestStatus_IncludesMarkerState(t *testing.T) {
	c, mb, dir := newMarkerCoordinator(t)
	const markerID = "deadbeef00112233"
	blob := []byte(`{"claudeAiOauth":{"accessToken":"a"},"__foxy_marker":"` + markerID + `"}`)
	if err := os.WriteFile(mb.path, blob, 0o600); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
	if err := writeLastWrite(dir, lastWriteFile{MarkerID: markerID, AccountID: 1}); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}
	st := c.Status()
	if st.MarkerState != "intact" {
		t.Errorf("Status.MarkerState: got %q want intact", st.MarkerState)
	}
}

func TestStatus_NilCoordinator_EmptyMarkerState(t *testing.T) {
	var c *Coordinator
	st := c.Status()
	if st.MarkerState != "" {
		t.Errorf("nil coordinator should produce empty marker_state, got %q", st.MarkerState)
	}
}
