package tui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/hoveychen/foxy-switcher/server/deviceinfo"
)

// Onboarding mirrors the desktop OnboardingOverlay's state machine on
// the TUI. Phases:
//
//	inactive    — normal app rendering.
//	choose      — local / cloud picker. Local closes the overlay
//	              (combined-mode daemon is what's already running);
//	              Cloud → cloud-input.
//	cloud-input — vault URL prompt + deploy-docs hint. Enter triggers
//	              pair-init via the daemon proxy and moves to
//	              cloud-pair.
//	cloud-pair  — show user_code + verification URL, poll until the
//	              vault marks the device approved. Approve writes
//	              agent-config.json and advances to "done"; any
//	              failure (denied / expired / network) drops back to
//	              cloud-input with the URL preserved and an inline
//	              error.
//	done        — "Configured. Quit and re-run `foxy-switcher tui`"
//	              terminal screen — the embedded daemon's mode is
//	              decided at process start (per detectModeFromConfig
//	              in main.go), so we ask the user to bounce. Going
//	              the other way (in-place daemon restart) would mean
//	              tearing down the goroutine in main.runTUI and
//	              re-spawning the daemon, which the simple form here
//	              avoids.
type onboardingPhase int

const (
	onboardingInactive onboardingPhase = iota
	onboardingChoose
	onboardingCloudInput
	onboardingCloudPair
	onboardingDone
)

const onboardingDeployURL = "https://hoveychen.github.io/foxy-switcher/deploy.html"

type onboardingState struct {
	phase        onboardingPhase
	decided      bool // true once decideOnboardingCmd has come back
	chooseCursor int  // 0 = local, 1 = cloud
	urlInput     textinput.Model
	nonce        string
	userCode     string
	verifURL     string
	errMsg       string
}

func newOnboardingState() onboardingState {
	ti := textinput.New()
	ti.Placeholder = "https://vault.example.com"
	ti.CharLimit = 256
	ti.Prompt = "› "
	return onboardingState{
		phase:    onboardingInactive,
		urlInput: ti,
	}
}

// trimmedURL returns the user-supplied vault URL with whitespace and
// trailing slashes stripped. Used as a stable cache key for nonce
// correlation.
func (s *onboardingState) trimmedURL() string {
	return strings.TrimRight(strings.TrimSpace(s.urlInput.Value()), "/")
}

// onboardingDecisionMsg is dispatched once at startup so App.Update can
// flip into onboardingChoose if the user has neither paired with a
// vault nor accumulated any local accounts. Mirrors the auto-dismiss
// rule in App.tsx so an upgrading user is never forced through the
// wizard a second time.
type onboardingDecisionMsg struct {
	needOnboarding bool
}

func decideOnboardingCmd(c *Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		about, err := c.GetAbout(ctx)
		if err != nil {
			// Daemon not ready — leave decision pending; refreshCmd
			// will retry on its own clock and the user sees the
			// regular Loading… while we wait. Returning false here
			// would falsely "unlock" the app.
			return onboardingDecisionMsg{needOnboarding: false}
		}
		accs, _ := c.ListAccounts(ctx)
		configured := (about.Mode == "agent" && about.VaultURL != "") ||
			(about.Mode != "agent" && len(accs) > 0)
		return onboardingDecisionMsg{needOnboarding: !configured}
	}
}

// pairInitMsg / pairPollMsg flow the daemon-proxy responses back into
// App.Update so the state machine doesn't have to share a goroutine
// with the bubbletea event loop.
type pairInitMsg struct {
	out PairInitOut
	err error
}

type pairPollMsg struct {
	out PairPollOut
	err error
}

func pairInitCmd(c *Client, vaultURL, deviceName, nonce string, meta *PairMeta) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := c.PairInit(ctx, vaultURL, deviceName, nonce, meta)
		return pairInitMsg{out: out, err: err}
	}
}

// pairPollCmd waits 2s before issuing the next poll, mirroring the
// desktop's polling cadence and giving the user time to approve in the
// browser without us hammering the vault.
func pairPollCmd(c *Client, vaultURL, nonce string) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(2 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := c.PairPoll(ctx, vaultURL, nonce)
		return pairPollMsg{out: out, err: err}
	}
}

// tuiAgentConfig has the same JSON shape as server/pair.go's
// AgentConfig — duplicated here because main is unimportable. Keep the
// field tags in lock-step with that file; the agent's reader uses the
// canonical names.
type tuiAgentConfig struct {
	VaultURL    string `json:"vault_url"`
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"`
}

// writeOnboardingAgentConfig persists the device token at
// <dataDir>/agent-config.json with mode 0600, atomically. Mirrors the
// Tauri save_agent_config command and server/pair.go's
// writeAgentConfig: tmp + rename, so a crash mid-write can't leave a
// half-written file the agent's reader chokes on.
func writeOnboardingAgentConfig(dataDir string, cfg tuiAgentConfig) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(dataDir, "agent-config.json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, append(buf, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// newOnboardingNonce returns a 16-byte hex string. Like the desktop's
// random nonce, it doesn't carry security weight — the vault uses it
// only to correlate init↔poll on its own side.
func newOnboardingNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Crypto rand can fail on locked-down systems; fall back to
		// timestamp so onboarding is degraded but not broken.
		return fmt.Sprintf("nonce-%d", time.Now().UnixNano())
	}
	return "nonce-" + hex.EncodeToString(b[:])
}

// handleOnboardingKey routes a key press to the appropriate phase
// handler. Returns the cmd to forward up to App.Update.
func (a *App) handleOnboardingKey(msg tea.KeyMsg) tea.Cmd {
	switch a.onboarding.phase {
	case onboardingChoose:
		return a.handleOnboardingChooseKey(msg)
	case onboardingCloudInput:
		return a.handleOnboardingCloudInputKey(msg)
	case onboardingCloudPair:
		return a.handleOnboardingCloudPairKey(msg)
	case onboardingDone:
		return a.handleOnboardingDoneKey(msg)
	}
	return nil
}

func (a *App) handleOnboardingChooseKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "left", "h":
		a.onboarding.chooseCursor = 0
	case "right", "l", "tab":
		a.onboarding.chooseCursor = 1
	case "1", "L":
		a.onboarding.phase = onboardingInactive
	case "2", "C":
		a.onboarding.errMsg = ""
		a.onboarding.urlInput.SetValue("")
		a.onboarding.urlInput.Focus()
		a.onboarding.phase = onboardingCloudInput
		return textinput.Blink
	case "enter":
		if a.onboarding.chooseCursor == 0 {
			a.onboarding.phase = onboardingInactive
			return nil
		}
		a.onboarding.errMsg = ""
		a.onboarding.urlInput.SetValue("")
		a.onboarding.urlInput.Focus()
		a.onboarding.phase = onboardingCloudInput
		return textinput.Blink
	case "ctrl+c":
		return tea.Quit
	}
	return nil
}

func (a *App) handleOnboardingCloudInputKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		a.onboarding.urlInput.Blur()
		a.onboarding.errMsg = ""
		a.onboarding.phase = onboardingChoose
		return nil
	case "ctrl+c":
		return tea.Quit
	case "enter":
		url := a.onboarding.trimmedURL()
		if url == "" {
			a.onboarding.errMsg = "Enter a vault URL first."
			return nil
		}
		a.onboarding.nonce = newOnboardingNonce()
		deviceName, _ := os.Hostname()
		if deviceName == "" {
			deviceName = "Foxy device"
		}
		a.onboarding.urlInput.Blur()
		a.onboarding.phase = onboardingCloudPair
		a.onboarding.userCode = ""
		a.onboarding.verifURL = ""
		a.onboarding.errMsg = "Reaching vault…"
		info := deviceinfo.Collect()
		meta := &PairMeta{
			Hostname:   info.Hostname,
			OS:         info.OS,
			OSVersion:  info.OSVersion,
			Arch:       info.Arch,
			Model:      info.Model,
			AppVersion: info.AppVersion,
			ClientType: "tui",
		}
		return pairInitCmd(a.accounts.client, url, deviceName, a.onboarding.nonce, meta)
	}
	var cmd tea.Cmd
	a.onboarding.urlInput, cmd = a.onboarding.urlInput.Update(msg)
	return cmd
}

func (a *App) handleOnboardingCloudPairKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		// User backs out — drop back to URL input. The pairPollCmd
		// already dispatched will deliver one stragglepairPollMsg
		// later, but the phase check in App.Update ignores it.
		a.onboarding.phase = onboardingCloudInput
		a.onboarding.urlInput.Focus()
		a.onboarding.errMsg = ""
		return textinput.Blink
	case "ctrl+c":
		return tea.Quit
	}
	return nil
}

func (a *App) handleOnboardingDoneKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "q", "ctrl+c", "enter", "esc":
		return tea.Quit
	}
	return nil
}

// onPairInitMsg / onPairPollMsg encapsulate the App.Update handling so
// app.go stays focused on the bubbletea router.
func (a *App) onPairInitMsg(msg pairInitMsg) tea.Cmd {
	if a.onboarding.phase != onboardingCloudPair {
		return nil
	}
	if msg.err != nil {
		a.onboarding.errMsg = "Couldn't reach the vault: " + msg.err.Error()
		a.onboarding.phase = onboardingCloudInput
		a.onboarding.urlInput.Focus()
		return textinput.Blink
	}
	a.onboarding.userCode = msg.out.UserCode
	a.onboarding.verifURL = msg.out.VerificationURL
	a.onboarding.errMsg = ""
	return pairPollCmd(a.accounts.client, a.onboarding.trimmedURL(), a.onboarding.nonce)
}

func (a *App) onPairPollMsg(msg pairPollMsg) tea.Cmd {
	if a.onboarding.phase != onboardingCloudPair {
		// User backed out while a poll was in flight — drop the result.
		return nil
	}
	if msg.err != nil {
		// Transient — keep polling.
		return pairPollCmd(a.accounts.client, a.onboarding.trimmedURL(), a.onboarding.nonce)
	}
	switch msg.out.Status {
	case "approved":
		url := a.onboarding.trimmedURL()
		err := writeOnboardingAgentConfig(a.dataDir, tuiAgentConfig{
			VaultURL:    url,
			DeviceID:    msg.out.DeviceID,
			DeviceToken: msg.out.DeviceToken,
		})
		if err != nil {
			a.onboarding.errMsg = "Save failed: " + err.Error()
			a.onboarding.phase = onboardingCloudInput
			a.onboarding.urlInput.Focus()
			return textinput.Blink
		}
		a.onboarding.phase = onboardingDone
		return nil
	case "denied":
		a.onboarding.errMsg = "The vault denied this pairing."
		a.onboarding.phase = onboardingCloudInput
		a.onboarding.urlInput.Focus()
		return textinput.Blink
	case "expired":
		a.onboarding.errMsg = "Pairing code expired. Try again."
		a.onboarding.phase = onboardingCloudInput
		a.onboarding.urlInput.Focus()
		return textinput.Blink
	case "pending":
		return pairPollCmd(a.accounts.client, a.onboarding.trimmedURL(), a.onboarding.nonce)
	default:
		a.onboarding.errMsg = "Unexpected vault status: " + msg.out.Status
		a.onboarding.phase = onboardingCloudInput
		a.onboarding.urlInput.Focus()
		return textinput.Blink
	}
}
