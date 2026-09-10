package deepseek

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// dshconfig.go writes the device half of the contract: the one line in the
// DeepSeek Harness's credential file that makes the `deepseek-official` route
// authenticate as the account foxy leased.
//
// WHERE, and why there is only one candidate. `dsh-credentials-local` resolves
// DEEPSEEK_API_KEY from four layers, first match wins:
//
//	1. the launch environment (DEEPSEEK_API_KEY=… dsh)   not writable
//	2. $DSH_HOME/.credentials.yaml                       ← foxy writes here
//	3. <cwd>/.env                                        not ours to touch
//	4. $DSH_HOME/.env                                    below the stored file
//
// Layer 2 is the highest one a program can write, and it is the only one that
// wins against a key the user saved earlier through dsh's own settings UI.
// Writing layer 4 instead would look correct on a fresh machine and be
// silently ignored on any machine where the user had ever saved a key. Layer 1
// still shadows us, and nothing on disk can change that — hence
// EnvShadowed below, which exists so that case is reported rather than
// mystifying.
//
// The overriding constraint is the same as for codex's config.toml: this is
// the USER'S file. It holds their other API keys, their provider sign-in
// records, and their comments. Everything below edits one line in place
// between sentinel comments and never rewrites the document wholesale.
//
// Their own DEEPSEEK_API_KEY, if they had one, is parked inside the managed
// block as a comment and restored by Remove. Parking it in the file rather
// than in foxy's memory is deliberate: a daemon killed with -9 would otherwise
// lose the user's key permanently, whereas a sentinel block on disk is still
// there — and still reversible — on the next start.

const (
	// credentialRef is the credential name the harness's `deepseek-official`
	// route resolves by default (dsh-llm-deepseek's apiKeyEnv).
	credentialRef = "DEEPSEEK_API_KEY"

	// beginSentinel / endSentinel bracket the block foxy owns. Removal is an
	// exact-interval delete between these lines, so anything the user writes
	// outside them is untouchable by us.
	beginSentinel = "# >>> foxy-switcher deepseek — managed, do not edit >>>"
	endSentinel   = "# <<< foxy-switcher deepseek — managed, do not edit <<<"

	// savedPrefix parks the user's own pre-foxy line so Remove can put it back
	// verbatim, including whatever quoting style they used.
	savedPrefix = "# foxy-switcher:saved "

	// credentialsFileMode is what dsh demands on POSIX: it refuses to load a
	// credential file any other user can read, and tells you to chmod 600.
	credentialsFileMode = 0o600
)

// DshConfig writes into one DeepSeek Harness home directory.
type DshConfig struct {
	// Home is $DSH_HOME (usually ~/.dsh).
	Home string
}

// DefaultHome resolves $DSH_HOME, falling back to ~/.dsh — the same order the
// harness itself uses.
func DefaultHome() (string, error) {
	if h := strings.TrimSpace(os.Getenv("DSH_HOME")); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".dsh"), nil
}

// EnvShadowed reports whether DEEPSEEK_API_KEY is set in this process's
// environment, which the harness treats as the highest-priority layer and
// which therefore beats anything foxy writes.
//
// This is not something foxy can fix — the variable is set in the shell that
// launches dsh, not in ours — so it is reported, not worked around. Note the
// check is necessarily approximate: it sees the daemon's environment, and dsh
// may be launched from a different shell. A false positive costs one log line;
// a false negative costs an operator an afternoon.
func EnvShadowed() bool {
	return strings.TrimSpace(os.Getenv(credentialRef)) != ""
}

func (c DshConfig) path() string {
	return filepath.Join(c.Home, ".credentials.yaml")
}

// Apply makes the credential file authenticate as apiKey. Idempotent: applying
// the same key twice leaves the file byte-identical, so dsh's watcher sees no
// churn.
func (c DshConfig) Apply(apiKey string) error {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return errors.New("deepseek: refusing to write an empty API key")
	}
	if c.Home == "" {
		return errors.New("deepseek: no DSH home configured")
	}
	if err := os.MkdirAll(c.Home, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", c.Home, err)
	}
	doc, err := c.read()
	if err != nil {
		return err
	}
	if err := checkVersioned(doc); err != nil {
		return err
	}
	return c.write(applyBlock(doc, apiKey))
}

// ErrUnversionedDocument means the credential file exists but carries no
// top-level `version:` key.
//
// We refuse rather than guess. That shape is dsh's pre-release flat layout — a
// bare mapping of reference names at the root — which dsh migrates in place on
// its next boot. Nesting a `refs:` section into it would produce a document
// that is neither shape, and the store refuses a document it cannot read by
// failing to start. One dsh launch fixes it; a corrupted credential file does
// not.
var ErrUnversionedDocument = errors.New(
	"deepseek: the dsh credential file predates the versioned layout; " +
		"start dsh once so it migrates the file, then retry")

// versionKey matches the top-level `version:` key.
var versionKey = regexp.MustCompile(`^version\s*:`)

func checkVersioned(lines []string) error {
	if len(lines) == 0 {
		return nil // no file — we write a versioned one
	}
	for _, ln := range lines {
		if versionKey.MatchString(ln) {
			return nil
		}
	}
	return ErrUnversionedDocument
}

// Remove takes foxy's block back out and restores the user's own key line if
// there was one. Idempotent, and safe to call on a file foxy never touched.
func (c DshConfig) Remove() error {
	if c.Home == "" {
		return nil
	}
	doc, err := c.read()
	if err != nil {
		return err
	}
	if doc == nil {
		return nil // no file at all
	}
	out, changed := removeBlock(doc)
	if !changed {
		return nil
	}
	return c.write(dropEmptyRefs(out))
}

// read returns the file's lines, or nil when it doesn't exist.
func (c DshConfig) read() ([]string, error) {
	raw, err := os.ReadFile(c.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", c.path(), err)
	}
	return splitLines(string(raw)), nil
}

// write persists the document atomically at 0600, skipping the write when the
// content is unchanged so dsh's file watcher doesn't see churn.
func (c DshConfig) write(lines []string) error {
	content := strings.Join(lines, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	path := c.path()
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		// Still assert the mode: a file the user chmod'd open would stop dsh
		// from loading at all, and we are about to depend on it loading.
		return ensureMode(path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	tmp := path + ".foxy-tmp"
	if err := os.WriteFile(tmp, []byte(content), credentialsFileMode); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return ensureMode(path)
}

// ensureMode re-asserts owner-only permissions. Skipped on Windows, which has
// no mode to set and where dsh skips the check for the same reason.
func ensureMode(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if err := os.Chmod(path, credentialsFileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// refsKey matches the top-level `refs:` mapping key. Anchored at column 0
// because a `refs:` nested under something else is a different thing entirely.
var refsKey = regexp.MustCompile(`^refs:\s*(#.*)?$`)

// ourRefLine matches an existing DEEPSEEK_API_KEY entry anywhere in the refs
// block, whatever indentation the user used.
var ourRefLine = regexp.MustCompile(`^(\s+)` + credentialRef + `\s*:`)

// applyBlock returns the document with foxy's managed block present and
// carrying apiKey.
//
// Three starting states, all reachable in practice:
//
//   - no file / no refs section — write a minimal valid document
//   - a refs section with no DEEPSEEK_API_KEY — insert our block at its end
//   - a refs section with the user's own key — park their line in our block
//
// A managed block that already exists is rewritten in place, and the parked
// line inside it is carried over untouched: re-applying must never lose the
// user's original key, and must never mistake foxy's own previous key for it.
func applyBlock(lines []string, apiKey string) []string {
	indent, start, end := findRefsBlock(lines)
	if start < 0 {
		// No refs section. Either there is no file at all, or the user has one
		// holding only records. Append a section rather than rewriting.
		out := append([]string{}, lines...)
		if len(out) == 0 {
			out = append(out, "version: 1", "")
		} else if strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, "refs:")
		return append(out, managedBlock("  ", apiKey, "")...)
	}

	// The block is emitted where the entry it replaces already sat, so a
	// later Remove restores the document byte for byte rather than shuffling
	// the user's keys into a different order.
	saved, at := "", -1
	var body []string
	inManaged := false
	for _, ln := range lines[start:end] {
		trimmed := strings.TrimSpace(ln)
		switch {
		case trimmed == beginSentinel:
			inManaged = true
			if at < 0 {
				at = len(body)
			}
		case trimmed == endSentinel:
			inManaged = false
		case inManaged && strings.HasPrefix(trimmed, savedPrefix):
			// Carry the user's parked line across a re-apply.
			saved = strings.TrimPrefix(trimmed, savedPrefix)
		case inManaged:
			// Foxy's own previous key line: dropped, not parked.
		case ourRefLine.MatchString(ln):
			// The user's own entry, seen for the first time. Park it, and take
			// its position.
			saved = strings.TrimSpace(ln)
			if at < 0 {
				at = len(body)
			}
		default:
			body = append(body, ln)
		}
	}
	if at < 0 {
		at = len(body)
	}

	out := append([]string{}, lines[:start]...)
	out = append(out, body[:at]...)
	out = append(out, managedBlock(indent, apiKey, saved)...)
	out = append(out, body[at:]...)
	return append(out, lines[end:]...)
}

// removeBlock deletes foxy's managed block and restores any line parked in it.
func removeBlock(lines []string) ([]string, bool) {
	var out []string
	inManaged, changed := false, false
	indent := "  "
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		switch {
		case trimmed == beginSentinel:
			inManaged, changed = true, true
			indent = leadingSpace(ln)
		case trimmed == endSentinel:
			inManaged = false
		case inManaged && strings.HasPrefix(trimmed, savedPrefix):
			out = append(out, indent+strings.TrimPrefix(trimmed, savedPrefix))
		case inManaged:
			// Foxy's key line — dropped.
		default:
			out = append(out, ln)
		}
	}
	return out, changed
}

// managedBlock renders foxy's sentinel-bracketed lines at the given indent.
func managedBlock(indent, apiKey, saved string) []string {
	out := []string{indent + beginSentinel}
	if saved != "" {
		out = append(out, indent+savedPrefix+saved)
	}
	out = append(out, indent+credentialRef+": "+quoteYAML(apiKey))
	return append(out, indent+endSentinel)
}

// findRefsBlock locates the top-level refs mapping and returns the indent its
// entries use plus the [start, end) line range of its body. start is -1 when
// there is no refs section.
func findRefsBlock(lines []string) (indent string, start, end int) {
	head := -1
	for i, ln := range lines {
		if refsKey.MatchString(ln) {
			head = i
			break
		}
	}
	if head < 0 {
		return "", -1, 0
	}
	start = head + 1
	end = len(lines)
	indent = ""
	for i := start; i < len(lines); i++ {
		ln := lines[i]
		if strings.TrimSpace(ln) == "" {
			continue
		}
		lead := leadingSpace(ln)
		if lead == "" {
			// Back at column 0: the refs mapping has ended.
			end = i
			break
		}
		if indent == "" {
			indent = lead
		}
	}
	if indent == "" {
		indent = "  "
	}
	// Don't absorb trailing blank lines into the block — inserting after them
	// would push our entry below a visual separator the user put there.
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return indent, start, end
}

// dropEmptyRefs removes a `refs:` header left with nothing under it. dsh
// refuses a document with an empty value, so a teardown that emptied the
// section we created would otherwise stop the harness from starting at all.
func dropEmptyRefs(lines []string) []string {
	_, start, end := findRefsBlock(lines)
	if start < 0 || end > start {
		return lines
	}
	out := append([]string{}, lines[:start-1]...)
	return append(out, lines[start:]...)
}

func leadingSpace(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// plainScalar matches values YAML reads back verbatim without quoting. DeepSeek
// keys are `sk-` plus base62, so this is the normal path; the quoted fallback
// is there so a gateway key with odd characters can't corrupt the document.
var plainScalar = regexp.MustCompile(`^[A-Za-z0-9_\-./+=]+$`)

func quoteYAML(v string) string {
	if plainScalar.MatchString(v) {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// splitLines splits without leaving a phantom empty final element for a
// trailing newline, so re-joining round-trips.
func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
