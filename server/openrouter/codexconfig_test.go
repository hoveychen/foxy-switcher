package openrouter

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func newCodexHome(t *testing.T) CodexConfig {
	t.Helper()
	return CodexConfig{Home: t.TempDir()}
}

func defaultSpec(models ...string) ProviderSpec {
	return ProviderSpec{
		TokenCommand: []string{"/usr/local/bin/foxy-switcher", "cred", "openrouter-token"},
		BaseURL:      "https://openrouter.ai/api/v1",
		Models:       models,
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func listProfiles(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), profileSuffix) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func TestApplyWritesProviderBlockAndOneProfilePerModel(t *testing.T) {
	c := newCodexHome(t)
	res, err := c.Apply(defaultSpec("deepseek/deepseek-v4-flash", "openai/gpt-oss-120b"))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Collisions) != 0 {
		t.Fatalf("unexpected collisions: %v", res.Collisions)
	}

	cfg := read(t, c.configPath())
	for _, want := range []string{
		beginSentinel, endSentinel,
		"[model_providers.openrouter]",
		`base_url = "https://openrouter.ai/api/v1"`,
		`wire_api = "responses"`,
		"[model_providers.openrouter.auth]",
		`command = "/usr/local/bin/foxy-switcher"`,
		`args = ["cred", "openrouter-token"]`,
		"timeout_ms = 5000",
		"refresh_interval_ms = 300000",
	} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("config.toml missing %q:\n%s", want, cfg)
		}
	}
	// The whole point of auth.command is that no secret lands on disk.
	if strings.Contains(cfg, "sk-or") || strings.Contains(cfg, "env_key") {
		t.Fatalf("config.toml carries a key or env_key:\n%s", cfg)
	}

	// One profile file per model — a provider block has no model list, so this
	// is the only thing that makes a model selectable.
	got := listProfiles(t, c.Home)
	want := []string{"or-deepseek-deepseek-v4-flash.config.toml", "or-openai-gpt-oss-120b.config.toml"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("profiles = %v, want %v", got, want)
	}
	p := read(t, filepath.Join(c.Home, "or-deepseek-deepseek-v4-flash.config.toml"))
	if !strings.Contains(p, `model = "deepseek/deepseek-v4-flash"`) ||
		!strings.Contains(p, `model_provider = "openrouter"`) {
		t.Fatalf("profile content:\n%s", p)
	}
	if !strings.Contains(p, profileSentinel) {
		t.Fatal("profile has no ownership sentinel — Remove could never safely delete it")
	}
	// Pinning effort would silently become the default for anyone who doesn't
	// choose one in Fleet.
	if strings.Contains(p, "model_reasoning_effort") {
		t.Fatalf("profile pins reasoning effort:\n%s", p)
	}
}

// TestApplyPreservesTheUsersConfig is the central promise of in-place editing:
// config.toml holds the user's model, features and project trust list, and a
// wholesale rewrite would destroy them.
func TestApplyPreservesTheUsersConfig(t *testing.T) {
	c := newCodexHome(t)
	original := `model = "gpt-5.6-sol"
model_reasoning_effort = "high"

[features]
web_search_request = true

[projects."/Users/me/work"]
trust_level = "trusted"
`
	if err := os.WriteFile(c.configPath(), []byte(original), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := c.Apply(defaultSpec("a/b")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cfg := read(t, c.configPath())
	for _, want := range []string{
		`model = "gpt-5.6-sol"`, `model_reasoning_effort = "high"`,
		"[features]", "web_search_request = true",
		`[projects."/Users/me/work"]`, `trust_level = "trusted"`,
	} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("Apply destroyed user setting %q:\n%s", want, cfg)
		}
	}

	// And Remove must put the file back exactly as it was.
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := read(t, c.configPath()); got != original {
		t.Fatalf("round-trip changed the user's config:\n--- got ---\n%s\n--- want ---\n%s", got, original)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	c := newCodexHome(t)
	spec := defaultSpec("a/b", "c/d")
	if _, err := c.Apply(spec); err != nil {
		t.Fatalf("first: %v", err)
	}
	first := read(t, c.configPath())
	firstProfile := read(t, filepath.Join(c.Home, "or-a-b.config.toml"))

	for i := 0; i < 3; i++ {
		if _, err := c.Apply(spec); err != nil {
			t.Fatalf("re-apply %d: %v", i, err)
		}
	}
	if got := read(t, c.configPath()); got != first {
		t.Fatalf("re-apply changed config.toml:\n--- got ---\n%s\n--- first ---\n%s", got, first)
	}
	if got := read(t, filepath.Join(c.Home, "or-a-b.config.toml")); got != firstProfile {
		t.Fatalf("re-apply changed a profile file")
	}
	if n := strings.Count(read(t, c.configPath()), beginSentinel); n != 1 {
		t.Fatalf("managed block appears %d times, want 1", n)
	}
}

// TestApplyNeverOverwritesAHandWrittenProfile is the data-loss test. The design
// calls this out as the one place where a bug could silently destroy a user's
// file, so ownership is proven by sentinel, never by name.
func TestApplyNeverOverwritesAHandWrittenProfile(t *testing.T) {
	c := newCodexHome(t)
	// The user happens to have a profile whose name collides with ours.
	mine := filepath.Join(c.Home, "or-a-b.config.toml")
	handWritten := "model = \"my-own-model\"\nmodel_provider = \"my-provider\"\n"
	if err := os.WriteFile(mine, []byte(handWritten), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := c.Apply(defaultSpec("a/b", "c/d"))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := read(t, mine); got != handWritten {
		t.Fatalf("the user's profile was overwritten:\n%s", got)
	}
	if len(res.Collisions) != 1 || res.Collisions[0].Model != "a/b" {
		t.Fatalf("collisions = %v, want the a/b clash reported", res.Collisions)
	}
	// One clash must not disable the other models.
	if _, err := os.Stat(filepath.Join(c.Home, "or-c-d.config.toml")); err != nil {
		t.Fatalf("the unaffected model was not written: %v", err)
	}

	// Remove must leave it alone too.
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := read(t, mine); got != handWritten {
		t.Fatalf("Remove deleted the user's profile:\n%s", got)
	}
}

func TestApplyRefusesAHandWrittenProviderBlock(t *testing.T) {
	c := newCodexHome(t)
	original := `[model_providers.openrouter]
name = "My OpenRouter"
base_url = "https://my-proxy.example.com/v1"
env_key = "OPENROUTER_API_KEY"
`
	if err := os.WriteFile(c.configPath(), []byte(original), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := c.Apply(defaultSpec("a/b"))
	if !errors.Is(err, ErrForeignProviderBlock) {
		t.Fatalf("err = %v, want ErrForeignProviderBlock", err)
	}
	if got := read(t, c.configPath()); got != original {
		t.Fatalf("the user's provider block was modified:\n%s", got)
	}
	// Nothing else should have been written either — a half-applied config is
	// worse than none.
	if profiles := listProfiles(t, c.Home); len(profiles) != 0 {
		t.Fatalf("profiles written despite the refusal: %v", profiles)
	}
}

func TestApplyRefusesAHandWrittenProviderSubTable(t *testing.T) {
	// Only the .auth sub-table is present — still the user claiming the id.
	c := newCodexHome(t)
	if err := os.WriteFile(c.configPath(),
		[]byte("[model_providers.openrouter.auth]\ncommand = \"my-helper\"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := c.Apply(defaultSpec("a/b")); !errors.Is(err, ErrForeignProviderBlock) {
		t.Fatalf("err = %v, want ErrForeignProviderBlock", err)
	}
}

func TestApplyIgnoresACommentedOutProviderBlock(t *testing.T) {
	// A commented header is not a declaration; refusing on it would strand the
	// user with no way to enable OpenRouter short of deleting their comments.
	c := newCodexHome(t)
	if err := os.WriteFile(c.configPath(),
		[]byte("# [model_providers.openrouter]\n# base_url = \"x\"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := c.Apply(defaultSpec("a/b")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// A shrunk allowlist must stop offering the dropped model, or the device keeps
// advertising a model the guardrail now rejects.
func TestApplyRemovesProfilesForModelsNoLongerAllowed(t *testing.T) {
	c := newCodexHome(t)
	if _, err := c.Apply(defaultSpec("a/b", "c/d", "e/f")); err != nil {
		t.Fatalf("first: %v", err)
	}
	res, err := c.Apply(defaultSpec("a/b"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(res.ProfilesRemoved) != 2 {
		t.Fatalf("removed = %v, want the two dropped models", res.ProfilesRemoved)
	}
	if got := listProfiles(t, c.Home); len(got) != 1 || got[0] != "or-a-b.config.toml" {
		t.Fatalf("profiles = %v, want only or-a-b", got)
	}
}

func TestRemoveDeletesOnlyOurProfiles(t *testing.T) {
	c := newCodexHome(t)
	if _, err := c.Apply(defaultSpec("a/b", "c/d")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// A user profile that doesn't match our prefix at all.
	other := filepath.Join(c.Home, "work.config.toml")
	if err := os.WriteFile(other, []byte("model = \"x\"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := c.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := listProfiles(t, c.Home); len(got) != 1 || got[0] != "work.config.toml" {
		t.Fatalf("profiles after Remove = %v, want only the user's", got)
	}
	cfg := read(t, c.configPath())
	if strings.Contains(cfg, beginSentinel) || strings.Contains(cfg, "model_providers.openrouter") {
		t.Fatalf("managed block survived Remove:\n%s", cfg)
	}
}

func TestRemoveIsSafeWhenNothingWasApplied(t *testing.T) {
	c := newCodexHome(t)
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove on an empty home: %v", err)
	}
	if err := os.WriteFile(c.configPath(), []byte("model = \"x\"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove with an unrelated config: %v", err)
	}
	if got := read(t, c.configPath()); got != "model = \"x\"\n" {
		t.Fatalf("Remove touched an unrelated config: %q", got)
	}
}

// Two distinct models can sanitise to the same filename. Silently letting one
// overwrite the other would drop a model from the dropdown with no error.
func TestProfileNamesAreInjective(t *testing.T) {
	names := profileNames([]string{"a/b", "a-b", "A/B"})
	if len(names) != 3 {
		t.Fatalf("names = %v, want one per model", names)
	}
	seen := map[string]string{}
	for model, name := range names {
		if prev, dup := seen[name]; dup {
			t.Fatalf("models %q and %q share the filename %q", prev, model, name)
		}
		seen[name] = model
	}
	// Deterministic: the same input must always produce the same mapping, or
	// each re-apply would delete and recreate files.
	again := profileNames([]string{"A/B", "a-b", "a/b"})
	for model, name := range names {
		if again[model] != name {
			t.Fatalf("mapping for %q changed: %q -> %q", model, name, again[model])
		}
	}
}

func TestColliidingModelsBothGetProfiles(t *testing.T) {
	c := newCodexHome(t)
	if _, err := c.Apply(defaultSpec("a/b", "a-b")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	profiles := listProfiles(t, c.Home)
	if len(profiles) != 2 {
		t.Fatalf("profiles = %v, want 2 (both models selectable)", profiles)
	}
	models := map[string]bool{}
	for _, p := range profiles {
		body := read(t, filepath.Join(c.Home, p))
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "model = ") {
				models[line] = true
			}
		}
	}
	if len(models) != 2 {
		t.Fatalf("distinct model lines = %d, want 2 — one model was lost", len(models))
	}
}

// A Windows executable path is full of backslashes; unescaped they would be
// read as TOML escape sequences and codex would fail to parse the config.
func TestTOMLStringEscapesWindowsPaths(t *testing.T) {
	got := tomlString(`C:\Program Files\foxy\bin\foxy.exe`)
	want := `"C:\\Program Files\\foxy\\bin\\foxy.exe"`
	if got != want {
		t.Fatalf("tomlString = %s, want %s", got, want)
	}
	if q := tomlString(`say "hi"`); q != `"say \"hi\""` {
		t.Fatalf("tomlString = %s", q)
	}
}

func TestApplyWritesEscapedWindowsCommand(t *testing.T) {
	c := newCodexHome(t)
	spec := defaultSpec("a/b")
	spec.TokenCommand = []string{`C:\foxy\foxy.exe`, "cred", "openrouter-token"}
	if _, err := c.Apply(spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if cfg := read(t, c.configPath()); !strings.Contains(cfg, `command = "C:\\foxy\\foxy.exe"`) {
		t.Fatalf("windows command not escaped:\n%s", cfg)
	}
}

func TestApplyRequiresATokenCommand(t *testing.T) {
	c := newCodexHome(t)
	spec := defaultSpec("a/b")
	spec.TokenCommand = nil
	if _, err := c.Apply(spec); err == nil {
		t.Fatal("Apply without a token command must fail — the alternative is writing the key to disk")
	}
}

// Repeated apply/remove cycles must not accumulate blank lines or leave debris.
func TestApplyRemoveCyclesAreStable(t *testing.T) {
	c := newCodexHome(t)
	original := "model = \"gpt-5.6-sol\"\n"
	if err := os.WriteFile(c.configPath(), []byte(original), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Apply(defaultSpec("a/b")); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		if err := c.Remove(); err != nil {
			t.Fatalf("remove %d: %v", i, err)
		}
		if got := read(t, c.configPath()); got != original {
			t.Fatalf("cycle %d left residue: %q", i, got)
		}
	}
}
