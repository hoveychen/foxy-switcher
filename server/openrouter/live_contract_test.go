package openrouter

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// live_contract_test.go pins the request shapes verified against the real
// OpenRouter management API on 2026-07-31 with a live provisioning key. Each
// assertion below corresponds to an observed response, not to a reading of the
// docs — the three cases here were all WRONG in the first implementation and
// were only caught by making the calls.

// Verified: POST /guardrails/{id}/assignments/keys wants {"key_hashes":[…]},
// an ARRAY. Sending {"key_hash":"…"} returns
//
//	400 ZodError: expected array, received undefined, path ["key_hashes"]
//
// This one was fatal: assignment is treated as a hard failure, so every
// derivation would have unwound and OpenRouter would never have worked at all.
func TestAssignmentSendsKeyHashesArray(t *testing.T) {
	c, f := newFake(t, happyHandler)
	if err := c.AssignGuardrailToKey(context.Background(), "gr-1", "kh-1"); err != nil {
		t.Fatalf("AssignGuardrailToKey: %v", err)
	}
	body := f.seen()[0].Body
	if _, legacy := body["key_hash"]; legacy {
		t.Fatalf("payload still sends the singular key_hash: %+v", body)
	}
	hashes, ok := body["key_hashes"].([]any)
	if !ok {
		t.Fatalf("key_hashes = %#v, want an array", body["key_hashes"])
	}
	if len(hashes) != 1 || hashes[0] != "kh-1" {
		t.Fatalf("key_hashes = %v, want [kh-1]", hashes)
	}
}

// Verified: a guardrail with limit_usd and no reset_interval returns
//
//	400 ZodError: "Reset interval is required when setting a budget limit"
//
// So a spend cap with a lifetime (never-resetting) window is not expressible.
// Fail before the request rather than after, and say which field to fix —
// otherwise the operator sees a raw Zod error at first derivation, long after
// the save that caused it.
func TestGuardrailSpendCapRequiresAResetInterval(t *testing.T) {
	c, f := newFake(t, happyHandler)
	_, err := c.CreateGuardrail(context.Background(), GuardrailSpec{
		Name: "n", AllowedModels: []string{"a/b"}, LimitUSD: 25,
	})
	if err == nil || !strings.Contains(err.Error(), "reset") {
		t.Fatalf("err = %v, want a local refusal naming reset_interval", err)
	}
	if len(f.seen()) != 0 {
		t.Fatalf("sent a request OpenRouter always rejects: %v", f.paths())
	}

	// With a reset interval it goes through.
	if _, err := c.CreateGuardrail(context.Background(), GuardrailSpec{
		Name: "n", AllowedModels: []string{"a/b"}, LimitUSD: 25, ResetInterval: "monthly",
	}); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	// No cap at all is fine without a reset interval.
	if _, err := c.CreateGuardrail(context.Background(), GuardrailSpec{
		Name: "n", AllowedModels: []string{"a/b"},
	}); err != nil {
		t.Fatalf("uncapped spec rejected: %v", err)
	}
}

// DeriveDeviceKey must surface the same refusal rather than unwinding a
// half-built derivation.
func TestDeriveRefusesACapWithNoResetInterval(t *testing.T) {
	c, f := newFake(t, happyHandler)
	_, err := c.DeriveDeviceKey(context.Background(), DeriveSpec{
		KeyName: "foxy-dev-1", GuardrailName: "foxy-dev-1",
		AllowedModels: []string{"a/b"}, LimitUSD: 25,
	})
	if err == nil || !strings.Contains(err.Error(), "reset") {
		t.Fatalf("err = %v, want the reset_interval refusal", err)
	}
	if len(f.seen()) != 0 {
		t.Fatalf("upstream was touched: %v", f.paths())
	}
}

// Verified: the probe's original payload was rejected twice over —
// "openrouter/auto" is not a valid allowed_models entry
// (400 "Invalid allowed_models: openrouter/auto"), and limit_usd without
// reset_interval trips the rule above. A name-only guardrail creates fine
// (201), so the probe must send nothing else: it is answering "can this
// account create guardrails at all", and any extra field is another way for the
// answer to come back as a spurious error.
func TestCapabilityProbeSendsOnlyAName(t *testing.T) {
	c, f := newFake(t, happyHandler)
	caps, err := c.CheckCapabilities(context.Background())
	if err != nil {
		t.Fatalf("CheckCapabilities: %v", err)
	}
	if !caps.GuardrailsAvailable {
		t.Fatalf("caps = %+v", caps)
	}
	body := f.seen()[0].Body
	for _, field := range []string{"allowed_models", "limit_usd", "reset_interval"} {
		if v, present := body[field]; present {
			t.Fatalf("probe sends %s=%v; any extra field is another way to get a "+
				"validation error instead of a capability answer", field, v)
		}
	}
	if body["name"] == nil {
		t.Fatalf("probe body = %+v, want a name", body)
	}
}

// Verified: creation returns 201, not 200. A client that only accepted 200
// would reject every successful derivation.
func TestCreatedResponsesAreAccepted(t *testing.T) {
	c, _ := newFake(t, func(c call) (int, string) {
		switch {
		case c.Method == http.MethodPost && c.Path == "/guardrails":
			return http.StatusCreated, `{"data":{"id":"gr-1"}}`
		case c.Method == http.MethodPost && c.Path == "/keys":
			return http.StatusCreated, `{"key":"sk-or-v1-runtime","data":{"hash":"kh-1"}}`
		}
		return happyHandler(c)
	})
	got, err := c.DeriveDeviceKey(context.Background(), DeriveSpec{
		KeyName: "foxy-dev-1", GuardrailName: "foxy-dev-1",
		AllowedModels: []string{"a/b"}, LimitUSD: 25, LimitReset: "monthly",
	})
	if err != nil {
		t.Fatalf("201 responses rejected: %v", err)
	}
	if got.Hash != "kh-1" || got.Secret != "sk-or-v1-runtime" {
		t.Fatalf("derived = %+v", got)
	}
}

// Verified: DELETE returns 200 {"deleted":true}; a repeat DELETE returns 404.
// Revocation must therefore treat 404 as success or a device could never be
// fully cleaned up after a partial failure.
func TestRepeatedDeleteIsSuccess(t *testing.T) {
	var seen int
	c, _ := newFake(t, func(c call) (int, string) {
		if c.Method == http.MethodDelete {
			seen++
			if seen == 1 {
				return http.StatusOK, `{"deleted":true}`
			}
			return http.StatusNotFound, `{"error":{"message":"not found"}}`
		}
		return happyHandler(c)
	})
	if err := c.DeleteKey(context.Background(), "kh-1"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := c.DeleteKey(context.Background(), "kh-1"); err != nil {
		t.Fatalf("repeat delete = %v, want nil", err)
	}
}

// Guard against a regression in how a plan-level refusal is classified: a 400
// validation error is NOT "guardrails unavailable" — the account can create
// them, we just sent a bad payload. Conflating the two would report a working
// account as unsupported.
func TestValidationErrorIsNotGuardrailsUnavailable(t *testing.T) {
	c, _ := newFake(t, func(c call) (int, string) {
		if strings.HasPrefix(c.Path, "/guardrails") {
			return http.StatusBadRequest, `{"success":false,"error":{"name":"ZodError","message":"bad"}}`
		}
		return happyHandler(c)
	})
	_, err := c.CreateGuardrail(context.Background(), GuardrailSpec{
		Name: "n", AllowedModels: []string{"a/b"},
	})
	if errors.Is(err, ErrGuardrailsUnavailable) {
		t.Fatalf("a 400 was classified as ErrGuardrailsUnavailable: %v", err)
	}
	if err == nil {
		t.Fatal("a 400 must still be an error")
	}
}
