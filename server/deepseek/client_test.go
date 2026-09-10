package deepseek

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// balance_infos is a list with one entry per currency, and a real account
// carries several: 160.13 CNY alongside 0.00 USD. Reporting whichever came
// first would show "0.00" for a funded account, which reads as broke. Pinned
// in both orders because the API does not promise one.
func TestBalancePicksTheFundedCurrency(t *testing.T) {
	const cny = `{"currency":"CNY","total_balance":"160.13","granted_balance":"0.00","topped_up_balance":"160.13"}`
	const usd = `{"currency":"USD","total_balance":"0.00","granted_balance":"0.00","topped_up_balance":"0.00"}`
	for name, infos := range map[string]string{
		"funded first": cny + "," + usd,
		"empty first":  usd + "," + cny,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[` + infos + `]}`))
			}))
			defer srv.Close()

			b, err := (&Client{BaseURL: srv.URL, APIKey: "sk-test"}).Balance(context.Background())
			if err != nil {
				t.Fatalf("Balance: %v", err)
			}
			if b.Amount != 160.13 || b.Currency != "CNY" {
				t.Fatalf("Balance = %+v, want the funded 160.13 CNY entry", b)
			}
		})
	}
}

// Every currency empty: still name one, so the UI shows "0.00 CNY" rather
// than a bare number with no unit.
func TestBalanceAllCurrenciesEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"is_available":false,"balance_infos":[
		  {"currency":"CNY","total_balance":"0.00"},{"currency":"USD","total_balance":"0.00"}]}`))
	}))
	defer srv.Close()

	b, err := (&Client{BaseURL: srv.URL, APIKey: "sk-test"}).Balance(context.Background())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if b.Amount != 0 || b.Currency != "CNY" || b.Available {
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
	if got := err.Error(); !strings.Contains(got, "Authentication Fails") {
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
