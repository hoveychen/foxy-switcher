// dump-oauth prints the raw /api/oauth/profile and /api/oauth/usage payloads
// for every account in the store. Used to diagnose schema differences
// between subscription tiers (e.g. is there a field that distinguishes
// personal Max 5x from Max 20x?). Run with the daemon STOPPED so it doesn't
// fight over the SQLite file.
//
//	cd server
//	go run ./cmd/dump-oauth                    # all accounts
//	go run ./cmd/dump-oauth -account-id=3      # one account
//	go run ./cmd/dump-oauth -data-dir=/path    # custom data dir
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/hoveychen/foxy-switcher/server/authz"
	"github.com/hoveychen/foxy-switcher/server/store"
)

const baseURL = "https://api.anthropic.com"

func main() {
	dataDir := flag.String("data-dir", "", "data dir (default ~/.foxy-switcher)")
	accountID := flag.Int64("account-id", 0, "if non-zero, only dump this account id")
	flag.Parse()

	dir := *dataDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fail("home dir:", err)
		}
		dir = filepath.Join(home, ".foxy-switcher")
	}

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		fail("open store:", err)
	}
	defer st.Close()

	ctx := context.Background()
	accs, err := st.List(ctx)
	if err != nil {
		fail("list accounts:", err)
	}

	for _, a := range accs {
		if *accountID != 0 && a.ID != *accountID {
			continue
		}
		fmt.Printf("\n========================================\n")
		fmt.Printf("Account #%d: %s\n", a.ID, a.Name)
		fmt.Printf("  email=%q org=%q\n", a.Email, a.OrganizationName)
		fmt.Printf("  plan=%q subscription_type=%q\n", a.Plan, a.SubscriptionType)
		fmt.Printf("========================================\n")

		token := a.AccessToken
		if a.ExpiresAt > 0 && a.ExpiresAt < time.Now().Add(60*time.Second).UnixMilli() {
			fmt.Println("(token expired or near expiry, refreshing…)")
			tr, err := authz.RefreshToken(ctx, a.RefreshToken)
			if err != nil {
				fmt.Fprintf(os.Stderr, "refresh failed: %v\n", err)
				continue
			}
			token = tr.AccessToken
		}

		fmt.Println("--- /api/oauth/profile ---")
		fmt.Println(fetchPretty(ctx, token, "/api/oauth/profile"))
		fmt.Println("--- /api/oauth/usage ---")
		fmt.Println(fetchPretty(ctx, token, "/api/oauth/usage"))
	}
}

func fetchPretty(ctx context.Context, token, path string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return fmt.Sprintf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Sprintf("request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return string(body)
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(body)
	}
	return string(pretty)
}

func fail(msg string, err error) {
	fmt.Fprintln(os.Stderr, msg, err)
	os.Exit(1)
}
