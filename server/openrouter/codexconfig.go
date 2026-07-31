package openrouter

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// codexconfig.go writes the device half of the contract: the two kinds of file
// codex reads, and that Fleet scans.
//
//   - config.toml gets a [model_providers.openrouter] block — HOW to connect.
//   - <CODEX_HOME>/or-<model>.config.toml, one per allowed model — WHAT can be
//     selected. A provider block carries no model list, and an inline
//     [profiles.x] table is hard-rejected by codex 0.145.0 (the whole session
//     fails to start), so a profile-v2 file per model is the only way to make a
//     model selectable. That is why "one file = one model" is the shape here.
//
// The overriding constraint is that config.toml is the USER'S file: it holds
// their model, features, and project trust list. Everything below edits it in
// place between sentinel comments and never rewrites it wholesale, and nothing
// here ever overwrites or deletes a file that isn't marked as ours.

const (
	// providerID is the codex provider key. Also the or- prefix's namesake.
	providerID = "openrouter"

	// beginSentinel / endSentinel bracket the block foxy owns inside the user's
	// config.toml. Removal is an exact-interval delete between these lines, so
	// anything the user writes outside them is untouchable by us.
	beginSentinel = "# >>> foxy-switcher openrouter — managed block, do not edit >>>"
	endSentinel   = "# <<< foxy-switcher openrouter — managed block, do not edit <<<"

	// profileSentinel marks a profile file as foxy-written. Its presence is the
	// ONLY thing that authorises overwriting or deleting that file, which is what
	// keeps a name collision with a hand-written profile from destroying it.
	profileSentinel = "# foxy-switcher managed profile — do not edit"

	// profilePrefix namespaces our profile files so a collision with a
	// hand-written profile is unlikely in the first place. Unlikely is not
	// impossible, hence the sentinel check.
	profilePrefix = "or-"

	// profileSuffix is what codex (and Fleet's scanner) recognise as a
	// profile-v2 file.
	profileSuffix = ".config.toml"
)

// CodexConfig writes into one codex home directory.
type CodexConfig struct {
	// Home is $CODEX_HOME (usually ~/.codex).
	Home string
}

// ProviderSpec is what to write. It is assembled from the vault's grant.
type ProviderSpec struct {
	// TokenCommand is the argv codex executes to obtain a bearer token, e.g.
	// {"/usr/local/bin/foxy-switcher", "cred", "openrouter-token"}. The key is
	// fetched per use rather than stored, so config.toml holds no secret at all.
	TokenCommand []string
	// BaseURL is the OpenRouter API root.
	BaseURL string
	// Models are the OpenRouter model slugs to expose, one profile file each.
	Models []string
	// TokenTimeoutMS bounds one token-command execution.
	TokenTimeoutMS int
	// TokenRefreshIntervalMS is how often codex re-runs the command.
	TokenRefreshIntervalMS int
}

// Default token-command timings. Five seconds is generous for a loopback call
// to the local daemon; five minutes of reuse keeps the per-request cost near
// zero while still picking up a rotated key within one interval.
const (
	DefaultTokenTimeoutMS         = 5000
	DefaultTokenRefreshIntervalMS = 300000
)

// ApplyResult reports what Apply did, including what it refused to do.
type ApplyResult struct {
	// ProfilesWritten are the absolute paths of profile files created or
	// refreshed.
	ProfilesWritten []string
	// ProfilesRemoved are foxy-owned profile files deleted because their model
	// left the allowlist.
	ProfilesRemoved []string
	// Collisions are models we could NOT expose because a file of that name
	// already exists and isn't ours. Reported rather than fatal: one stray
	// hand-written profile shouldn't disable every other model. The caller is
	// expected to log these — a silently missing model is confusing.
	Collisions []Collision
}

// Collision is one model we declined to write.
type Collision struct {
	Model string
	Path  string
}

func (c Collision) String() string {
	return fmt.Sprintf("model %q: %s exists and was not written by foxy", c.Model, c.Path)
}

// ErrForeignProviderBlock means the user's config.toml already declares
// [model_providers.openrouter] outside our sentinels. We refuse rather than
// fight over the key: two definitions of one provider id is a config the user
// has to resolve, and silently winning would break whatever they set up.
var ErrForeignProviderBlock = errors.New(
	"config.toml already defines [model_providers.openrouter] outside foxy's managed block; " +
		"remove or rename it, or foxy would be overriding your own provider")

// Apply makes the codex home match spec: the provider block is inserted or
// refreshed, a profile file exists for each allowed model, and foxy-owned
// profiles for models no longer allowed are removed.
//
// Idempotent — running it twice with the same spec leaves byte-identical files,
// which matters because the agent re-applies on every authorisation change.
func (c CodexConfig) Apply(spec ProviderSpec) (ApplyResult, error) {
	var res ApplyResult
	if c.Home == "" {
		return res, errors.New("codex home not set")
	}
	if len(spec.TokenCommand) == 0 {
		return res, errors.New("token command required (the key is never written to disk)")
	}
	if err := os.MkdirAll(c.Home, 0o700); err != nil {
		return res, fmt.Errorf("mkdir %s: %w", c.Home, err)
	}

	if err := c.applyProviderBlock(spec); err != nil {
		return res, err
	}

	// Write the wanted profiles, tracking which paths are legitimately ours so
	// the sweep below doesn't delete what we just wrote.
	wanted := make(map[string]bool)
	for model, name := range profileNames(spec.Models) {
		path := filepath.Join(c.Home, name+profileSuffix)
		ours, err := isOurProfile(path)
		if err != nil {
			return res, err
		}
		if !ours {
			// The file exists and we didn't write it. This is the single place in
			// the whole feature where a bug could destroy user data, so the rule is
			// absolute: never overwrite what we don't own.
			res.Collisions = append(res.Collisions, Collision{Model: model, Path: path})
			continue
		}
		wanted[path] = true
		if err := writeFileIfChanged(path, profileContent(model)); err != nil {
			return res, err
		}
		res.ProfilesWritten = append(res.ProfilesWritten, path)
	}

	removed, err := c.sweepStaleProfiles(wanted)
	if err != nil {
		return res, err
	}
	res.ProfilesRemoved = removed
	sort.Strings(res.ProfilesWritten)
	sort.Strings(res.ProfilesRemoved)
	return res, nil
}

// Remove restores the codex home to its pre-foxy state: the managed block is
// cut out of config.toml (leaving every other line exactly as the user has it)
// and every foxy-owned profile file is deleted. A hand-written provider block
// or profile of the same name is left completely alone.
//
// Safe to call when nothing was ever applied.
func (c CodexConfig) Remove() error {
	if c.Home == "" {
		return errors.New("codex home not set")
	}
	path := c.configPath()
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// No config at all — nothing of ours can be in it.
	case err != nil:
		return fmt.Errorf("read %s: %w", path, err)
	default:
		stripped, found := stripManagedBlock(string(raw))
		if found {
			if err := writeFileIfChanged(path, stripped); err != nil {
				return err
			}
		}
	}
	_, err = c.sweepStaleProfiles(nil) // nothing is wanted → remove all ours
	return err
}

func (c CodexConfig) configPath() string { return filepath.Join(c.Home, "config.toml") }

// applyProviderBlock inserts or refreshes the managed block, preserving every
// other byte of the user's config.toml.
func (c CodexConfig) applyProviderBlock(spec ProviderSpec) error {
	path := c.configPath()
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	existing := string(raw)

	// Cut our old block out first, so the foreign-block check below looks only at
	// what the user actually wrote.
	body, _ := stripManagedBlock(existing)
	if declaresProvider(body) {
		return ErrForeignProviderBlock
	}

	block := renderProviderBlock(spec)
	out := body
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if out != "" {
		out += "\n"
	}
	out += block
	return writeFileIfChanged(path, out)
}

// renderProviderBlock builds the sentinel-bracketed TOML. Kept deterministic
// (no maps, no timestamps) so re-applying an unchanged spec rewrites nothing.
func renderProviderBlock(spec ProviderSpec) string {
	baseURL := spec.BaseURL
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/v1"
	}
	timeout := spec.TokenTimeoutMS
	if timeout <= 0 {
		timeout = DefaultTokenTimeoutMS
	}
	refresh := spec.TokenRefreshIntervalMS
	if refresh <= 0 {
		refresh = DefaultTokenRefreshIntervalMS
	}

	args := make([]string, 0, len(spec.TokenCommand)-1)
	for _, a := range spec.TokenCommand[1:] {
		args = append(args, tomlString(a))
	}

	var b strings.Builder
	b.WriteString(beginSentinel + "\n")
	b.WriteString("[model_providers." + providerID + "]\n")
	b.WriteString("name = \"OpenRouter\"\n")
	b.WriteString("base_url = " + tomlString(baseURL) + "\n")
	b.WriteString("wire_api = \"responses\"\n")
	b.WriteString("\n")
	// auth.command keeps the key off disk entirely: codex runs this on demand
	// and reads the token from stdout. The alternative (env_key) would need the
	// key in codex's process environment, and codex is spawned by Fleet — an
	// environment foxy has no way to reach.
	b.WriteString("[model_providers." + providerID + ".auth]\n")
	b.WriteString("command = " + tomlString(spec.TokenCommand[0]) + "\n")
	b.WriteString("args = [" + strings.Join(args, ", ") + "]\n")
	b.WriteString(fmt.Sprintf("timeout_ms = %d\n", timeout))
	b.WriteString(fmt.Sprintf("refresh_interval_ms = %d\n", refresh))
	b.WriteString(endSentinel + "\n")
	return b.String()
}

// profileContent is one model's profile-v2 file.
//
// model_reasoning_effort is deliberately absent: writing it would make it the
// default for every session on this profile. Fleet's effort selector overrides
// it via a CLI flag, but a user who picks no effort would silently get whatever
// we pinned instead of codex's own default.
func profileContent(model string) string {
	return profileSentinel + "\n" +
		"# model: " + model + "\n" +
		"model = " + tomlString(model) + "\n" +
		"model_provider = \"" + providerID + "\"\n"
}

// profileNames maps each model to its profile file's base name (no suffix).
//
// The mapping must be deterministic and injective. Sanitising can collapse two
// distinct models onto one name ("a/b" and "a-b" both sanitise to "a-b"), so
// any repeat gets a short digest of the full model id appended. Since the model
// list arrives sorted (store normalises it), which one gets the digest is
// stable across runs — otherwise a rerun would churn files.
func profileNames(models []string) map[string]string {
	sorted := append([]string(nil), models...)
	sort.Strings(sorted)
	used := make(map[string]bool, len(sorted))
	out := make(map[string]string, len(sorted))
	for _, m := range sorted {
		if strings.TrimSpace(m) == "" {
			continue
		}
		name := profilePrefix + sanitiseModel(m)
		if used[name] {
			sum := sha256.Sum256([]byte(m))
			name += "-" + hex.EncodeToString(sum[:])[:8]
		}
		used[name] = true
		out[m] = name
	}
	return out
}

// sanitiseModel maps an OpenRouter slug to a filename-safe token.
func sanitiseModel(model string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(model)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			// Collapse runs of separators so "a//b" and "a-b" don't both produce
			// double dashes; the collision digest handles the resulting overlap.
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// sweepStaleProfiles deletes every foxy-owned profile file not in `wanted`.
// Files without our sentinel are never touched, no matter how well their name
// matches — that is what makes an accidental name clash survivable.
func (c CodexConfig) sweepStaleProfiles(wanted map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(c.Home)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", c.Home, err)
	}
	var removed []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, profilePrefix) || !strings.HasSuffix(name, profileSuffix) {
			continue
		}
		path := filepath.Join(c.Home, name)
		if wanted[path] {
			continue
		}
		owned, err := hasProfileSentinel(path)
		if err != nil {
			return nil, err
		}
		if !owned {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove %s: %w", path, err)
		}
		removed = append(removed, path)
	}
	return removed, nil
}

// isOurProfile reports whether we may write `path`: true when the file is
// absent (nothing to lose) or carries our sentinel.
func isOurProfile(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return strings.Contains(string(raw), profileSentinel), nil
}

// hasProfileSentinel is isOurProfile's inverse default: a missing file is NOT
// ours to delete (it doesn't exist), which keeps the sweep from reporting
// phantom removals.
func hasProfileSentinel(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return strings.Contains(string(raw), profileSentinel), nil
}

// stripManagedBlock removes every sentinel-bracketed region from s, returning
// the remainder and whether anything was cut. An unterminated begin sentinel
// (a file truncated mid-write) drops everything from that line on, since the
// rest of it was ours anyway.
func stripManagedBlock(s string) (string, bool) {
	if !strings.Contains(s, beginSentinel) {
		return s, false
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	inBlock := false
	found := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inBlock && trimmed == beginSentinel {
			inBlock, found = true, true
			// Drop a single blank separator line we may have added before the block
			// so repeated apply/remove cycles don't accumulate blank lines.
			if n := len(out); n > 0 && strings.TrimSpace(out[n-1]) == "" {
				out = out[:n-1]
			}
			continue
		}
		if inBlock {
			if trimmed == endSentinel {
				inBlock = false
			}
			continue
		}
		out = append(out, line)
	}
	res := strings.Join(out, "\n")
	// Collapse a trailing run of blank lines to exactly one newline, so
	// apply→remove round-trips back to the original file.
	res = strings.TrimRight(res, "\n")
	if res != "" {
		res += "\n"
	}
	return res, found
}

// declaresProvider reports whether the (already de-foxied) config declares the
// openrouter provider itself. Matches the table headers codex accepts:
// [model_providers.openrouter] and its dotted sub-tables.
func declaresProvider(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") {
			continue
		}
		// Strip an inline trailing comment, then the brackets.
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
			continue
		}
		header := strings.TrimSpace(line[1 : len(line)-1])
		header = strings.TrimSpace(strings.Trim(header, "[]"))
		if header == "model_providers."+providerID ||
			strings.HasPrefix(header, "model_providers."+providerID+".") {
			return true
		}
	}
	return false
}

// tomlString renders a Go string as a TOML basic string. Escaping matters most
// on Windows, where an executable path is full of backslashes that TOML would
// otherwise read as escape sequences (C:\foxy\bin -> an invalid \f\b escape).
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(fmt.Sprintf(`\u%04X`, r))
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// writeFileIfChanged writes atomically (tmp + rename) and skips the write
// entirely when the content already matches. Skipping keeps mtimes stable so a
// file watcher doesn't see churn on every re-apply.
func writeFileIfChanged(path, content string) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	tmp := path + ".foxy-tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
