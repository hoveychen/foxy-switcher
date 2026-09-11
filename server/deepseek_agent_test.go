package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/vault"
)

type stubDeepSeekSource struct {
	grant *vault.DeepSeekGrant
	err   error
	calls int
}

func (s *stubDeepSeekSource) DeepSeekConfig(context.Context) (*vault.DeepSeekGrant, error) {
	s.calls++
	return s.grant, s.err
}

func newDSWriter(t *testing.T, src deepSeekGrantSource) (*deepSeekWriter, string) {
	t.Helper()
	home := t.TempDir()
	return newDeepSeekWriter(src, home, log.New(new(bytes.Buffer), "", 0)), home
}

func dshCredentials(t *testing.T, home string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".credentials.yaml"))
	if err != nil {
		return ""
	}
	return string(raw)
}

func TestDeepSeekWriterAppliesGrant(t *testing.T) {
	src := &stubDeepSeekSource{grant: &vault.DeepSeekGrant{
		AccountID: 1, AccountName: "pool", APIKey: "sk-ds-one",
		BaseURL: vault.DefaultDeepSeekBaseURL,
	}}
	w, home := newDSWriter(t, src)

	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := dshCredentials(t, home); !strings.Contains(got, "DEEPSEEK_API_KEY: sk-ds-one") {
		t.Fatalf("key was not written:\n%s", got)
	}
}

// Losing the grant — revoked, suspended, provider withdrawn, or every account
// drained — must remove the credential, so dsh stops authenticating as an
// account this device is no longer entitled to.
func TestDeepSeekWriterTearsDownWhenGrantIsLost(t *testing.T) {
	for name, lost := range map[string]error{
		"not granted": selector.ErrNoAvailable,
		"no account":  vault.ErrNoDeepSeekAccount,
		"wrapped":     errors.Join(vault.ErrNoDeepSeekAccount, errors.New("2 skipped")),
	} {
		t.Run(name, func(t *testing.T) {
			src := &stubDeepSeekSource{grant: &vault.DeepSeekGrant{
				AccountID: 1, AccountName: "pool", APIKey: "sk-ds-one",
			}}
			w, home := newDSWriter(t, src)
			if err := w.Sync(context.Background()); err != nil {
				t.Fatalf("Sync: %v", err)
			}

			src.grant, src.err = nil, lost
			if err := w.Sync(context.Background()); err != nil {
				t.Fatalf("Sync after losing the grant: %v", err)
			}
			if got := dshCredentials(t, home); strings.Contains(got, "sk-ds-one") {
				t.Fatalf("the key survived a lost grant:\n%s", got)
			}
		})
	}
}

// A transient vault outage must NOT tear the credential out: dsh would lose
// its key mid-session over a blip.
func TestDeepSeekWriterKeepsCredentialOnTransientError(t *testing.T) {
	src := &stubDeepSeekSource{grant: &vault.DeepSeekGrant{
		AccountID: 1, AccountName: "pool", APIKey: "sk-ds-one",
	}}
	w, home := newDSWriter(t, src)
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	src.grant, src.err = nil, errors.New("connection refused")
	if err := w.Sync(context.Background()); err == nil {
		t.Fatal("a transient failure must be reported, not swallowed")
	}
	if got := dshCredentials(t, home); !strings.Contains(got, "sk-ds-one") {
		t.Fatalf("a vault blip removed a working credential:\n%s", got)
	}
}

// Rotation onto another account (the previous one ran out of money) has to be
// announced: it explains a bill landing somewhere else.
func TestDeepSeekWriterLogsRollover(t *testing.T) {
	var logs bytes.Buffer
	src := &stubDeepSeekSource{grant: &vault.DeepSeekGrant{
		AccountID: 1, AccountName: "primary", APIKey: "sk-ds-one",
	}}
	w := newDeepSeekWriter(src, t.TempDir(), log.New(&logs, "", 0))

	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !strings.Contains(logs.String(), "configured the DeepSeek Harness") {
		t.Fatalf("first apply must be announced, got %q", logs.String())
	}
	logs.Reset()

	// Same account again: silent.
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("Sync again: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("a steady-state sync must be silent, got %q", logs.String())
	}

	src.grant = &vault.DeepSeekGrant{AccountID: 2, AccountName: "backup", APIKey: "sk-ds-two"}
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("Sync rollover: %v", err)
	}
	if !strings.Contains(logs.String(), `rolled onto account "backup"`) {
		t.Fatalf("rollover must be announced, got %q", logs.String())
	}
}

func TestDeepSeekWriterTeardownIsIdempotent(t *testing.T) {
	w, _ := newDSWriter(t, &stubDeepSeekSource{err: selector.ErrNoAvailable})
	for i := 0; i < 3; i++ {
		if err := w.Teardown(); err != nil {
			t.Fatalf("Teardown #%d: %v", i, err)
		}
	}
}
