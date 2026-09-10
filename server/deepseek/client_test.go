package deepseek

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBalanceParsesStringAmounts(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[
		  {"currency":"CNY","total_balance":"110.42","granted_balance":"10.00","topped_up_balance":"100.42"}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "sk-test"}
	b, err := c.Balance(context.Background())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if gotPath != "/user/balance" {
		t.Fatalf("path = %q, want /user/balance", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !b.Available || b.Amount != 110.42 || b.Currency != "CNY" {
		t.Fatalf("Balance = %+v, want available/110.42/CNY", b)
	}
}

// The pool acts on is_available, so a drained account must report false even
// though the call itself succeeded.
func TestBalanceUnavailableIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"is_available":false,"balance_infos":[
		  {"currency":"USD","total_balance":"0.00"}]}`))
	}))
	defer srv.Close()

	b, err := (&Client{BaseURL: srv.URL, APIKey: "sk-test"}).Balance(context.Background())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if b.Available {
		t.Fatal("is_available=false must survive the round trip")
	}
	if b.Currency != "USD" || b.Amount != 0 {
		t.Fatalf("Balance = %+v", b)
	}
}

// An amount we can't parse must not fail the call: availability is what the
// pool acts on, and the amount is only ever displayed.
func TestBalanceTolerateUnparsableAmount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"n/a"}]}`))
	}))
	defer srv.Close()

	b, err := (&Client{BaseURL: srv.URL, APIKey: "sk-test"}).Balance(context.Background())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !b.Available || b.Amount != 0 || b.Currency != "CNY" {
		t.Fatalf("Balance = %+v, want available with a zero amount", b)
	}
}

// An empty balance_infos is a reachable account with an unknown display
// amount, not a failure.
func TestBalanceEmptyInfos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[]}`))
	}))
	defer srv.Close()

	b, err := (&Client{BaseURL: srv.URL, APIKey: "sk-test"}).Balance(context.Background())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !b.Available || b.Currency != "" {
		t.Fatalf("Balance = %+v", b)
	}
}

func TestBalanceUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Authentication Fails"}}`))
	}))
	defer srv.Close()

	_, err := (&Client{BaseURL: srv.URL, APIKey: "sk-bad"}).Balance(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if got := err.Error(); !contains(got, "Authentication Fails") {
		t.Fatalf("error must carry the upstream message, got %q", got)
	}
}

func TestBalanceOtherStatusIsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"upstream down"}`))
	}))
	defer srv.Close()

	_, err := (&Client{BaseURL: srv.URL, APIKey: "sk-test"}).Balance(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusServiceUnavailable || apiErr.Message != "upstream down" {
		t.Fatalf("APIError = %+v", apiErr)
	}
}

func TestBalanceRequiresKey(t *testing.T) {
	_, err := (&Client{BaseURL: "http://127.0.0.1:1", APIKey: "  "}).Balance(context.Background())
	if err == nil {
		t.Fatal("a blank key must fail before any network call")
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
