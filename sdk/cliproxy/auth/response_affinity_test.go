package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSessionAffinityPinsPreviousResponseToCreatingCredential(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
	t.Cleanup(selector.Stop)
	auths := []*Auth{
		{ID: "auth-a", Provider: "openai-compatibility", Status: StatusActive},
		{ID: "auth-b", Provider: "openai-compatibility", Status: StatusActive},
	}

	metadata := map[string]any{
		cliproxyexecutor.SessionAffinityProviderMetadataKey: "openai-compatibility",
		cliproxyexecutor.SessionAffinityModelMetadataKey:    "qwen-plus",
		cliproxyexecutor.ResponseAffinityIDMetadataKey:      "resp_1",
	}
	selector.OnResult(Result{
		AuthID: "auth-a", Provider: "openai-compatibility", Model: "qwen-plus", Success: true,
		Options: cliproxyexecutor.Options{Metadata: metadata},
	})

	opts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"model":"qwen-plus","previous_response_id":"resp_1","input":"next"}`),
		Metadata:        make(map[string]any),
	}
	picked, errPick := selector.Pick(context.Background(), "openai-compatibility", "qwen-plus", opts, auths)
	if errPick != nil {
		t.Fatalf("Pick error: %v", errPick)
	}
	if picked.ID != "auth-a" {
		t.Fatalf("picked auth = %q, want auth-a", picked.ID)
	}
	if got := opts.Metadata[cliproxyexecutor.SessionAffinityStatusMetadataKey]; got != "response_hit" {
		t.Fatalf("affinity status = %#v, want response_hit", got)
	}
}

func TestSessionAffinityNeverFailsOverUnknownPreviousResponse(t *testing.T) {
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	t.Cleanup(selector.Stop)
	auths := []*Auth{
		{ID: "auth-a", Provider: "openai-compatibility", Status: StatusActive},
		{ID: "auth-b", Provider: "openai-compatibility", Status: StatusActive},
	}
	opts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"model":"qwen-plus","previous_response_id":"resp_unknown","input":"next"}`),
		Metadata:        make(map[string]any),
	}

	_, errPick := selector.Pick(context.Background(), "openai-compatibility", "qwen-plus", opts, auths)
	assertAuthErrorCode(t, errPick, "previous_response_affinity_missing")
}

func TestSessionAffinityAllowsUnknownPreviousResponseWithOnlyOneCredential(t *testing.T) {
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	t.Cleanup(selector.Stop)
	auths := []*Auth{{ID: "only-auth", Provider: "openai-compatibility", Status: StatusActive}}
	opts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"model":"qwen-plus","previous_response_id":"resp_before_restart","input":"next"}`),
		Metadata:        make(map[string]any),
	}

	picked, errPick := selector.Pick(context.Background(), "openai-compatibility", "qwen-plus", opts, auths)
	if errPick != nil || picked.ID != "only-auth" {
		t.Fatalf("singleton pick = %#v, err=%v", picked, errPick)
	}
	if got := opts.Metadata[cliproxyexecutor.SessionAffinityStatusMetadataKey]; got != "response_singleton" {
		t.Fatalf("affinity status = %#v, want response_singleton", got)
	}
}

func TestSessionAffinityNeverFailsOverUnavailablePreviousResponseOwner(t *testing.T) {
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	t.Cleanup(selector.Stop)
	auths := []*Auth{
		{ID: "auth-a", Provider: "openai-compatibility", Status: StatusActive, Unavailable: true},
		{ID: "auth-b", Provider: "openai-compatibility", Status: StatusActive},
	}
	selector.cache.Set(responseAffinityCacheKey("openai-compatibility", "resp_1", "qwen-plus"), "auth-a")
	opts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"model":"qwen-plus","previous_response_id":"resp_1","input":"next"}`),
		Metadata:        make(map[string]any),
	}

	_, errPick := selector.Pick(context.Background(), "openai-compatibility", "qwen-plus", opts, auths)
	assertAuthErrorCode(t, errPick, "previous_response_affinity_unavailable")
}

func TestSessionAffinityCapabilityScopeLeavesOtherProvidersUnchanged(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback:      &RoundRobinSelector{},
		TTL:           time.Hour,
		AuthAttribute: "session_affinity",
	})
	t.Cleanup(selector.Stop)
	auths := []*Auth{{ID: "ordinary", Provider: "openai-compatibility", Status: StatusActive}}
	opts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"model":"gpt-test","previous_response_id":"external_response","input":"next"}`),
		Metadata:        make(map[string]any),
	}

	picked, errPick := selector.Pick(context.Background(), "openai-compatibility", "gpt-test", opts, auths)
	if errPick != nil || picked.ID != "ordinary" {
		t.Fatalf("ordinary provider pick = %#v, err=%v", picked, errPick)
	}
	if got := opts.Metadata[cliproxyexecutor.SessionAffinityStatusMetadataKey]; got != "disabled" {
		t.Fatalf("affinity status = %#v, want disabled", got)
	}
}

func assertAuthErrorCode(t *testing.T, err error, want string) {
	t.Helper()
	authErr, ok := err.(*Error)
	if !ok || authErr.Code != want {
		t.Fatalf("error = %#v, want auth error code %q", err, want)
	}
}
