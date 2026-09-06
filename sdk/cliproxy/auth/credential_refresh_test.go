package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type credentialRefreshExecutor struct {
	ProviderExecutor
	provider string
	calls    atomic.Int32
	err      error
	started  chan struct{}
	release  chan struct{}
}

func (e *credentialRefreshExecutor) Identifier() string { return e.provider }
func (e *credentialRefreshExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.calls.Add(1)
	if e.started != nil {
		close(e.started)
		select {
		case <-e.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e.err != nil {
		return nil, e.err
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = "new-access"
	updated.Metadata["refresh_token"] = "new-refresh"
	updated.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	return updated, nil
}

func newCredentialRefreshManager(t *testing.T, provider string, expires time.Time) (*Manager, *credentialRefreshExecutor, *requestPrepareStore) {
	t.Helper()
	setRefreshLeadFactory(t, provider, func() *time.Duration { lead := 5 * time.Minute; return &lead })
	store := &requestPrepareStore{}
	m := NewManager(store, nil, nil)
	exec := &credentialRefreshExecutor{provider: provider}
	m.RegisterExecutor(exec)
	_, err := m.Register(t.Context(), &Auth{ID: "credential", Provider: provider, Status: StatusActive, Metadata: map[string]any{
		"access_token": "old-access", "refresh_token": "old-refresh", "expired": expires.Format(time.RFC3339),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m, exec, store
}

func TestRefreshCredentialProviderPolicyAndPersistence(t *testing.T) {
	for _, provider := range []string{"kimi", "codex", "xai"} {
		t.Run(provider, func(t *testing.T) {
			m, exec, store := newCredentialRefreshManager(t, provider, time.Now().Add(time.Hour))
			fresh, refreshed, err := m.RefreshCredential(t.Context(), "credential", false)
			if err != nil || refreshed || authAccessToken(fresh) != "old-access" || exec.calls.Load() != 0 {
				t.Fatalf("fresh: refreshed=%v err=%v", refreshed, err)
			}
			updated, refreshed, err := m.RefreshCredential(t.Context(), "credential", true)
			if err != nil || !refreshed || authAccessToken(updated) != "new-access" || exec.calls.Load() != 1 {
				t.Fatalf("forced: refreshed=%v err=%v", refreshed, err)
			}
			if saved := store.lastAuth(); authAccessToken(saved) != "new-access" || authMetadataString(saved, "refresh_token") != "new-refresh" {
				t.Fatal("rotated tokens were not persisted")
			}
			if updated.LastRefreshedAt.IsZero() {
				t.Fatal("missing refresh timestamp")
			}
			updated.Metadata["access_token"] = "caller-mutation"
			current, _, _ := m.RefreshCredential(t.Context(), "credential", false)
			if authAccessToken(current) != "new-access" {
				t.Fatal("returned metadata aliases manager state")
			}
		})
	}
}

func TestRefreshCredentialRefreshesDueAndMissingExpiry(t *testing.T) {
	for _, missing := range []bool{false, true} {
		m, exec, _ := newCredentialRefreshManager(t, "kimi", time.Now().Add(4*time.Minute))
		if missing {
			auth, _ := m.GetByID("credential")
			delete(auth.Metadata, "expired")
			if _, err := m.Update(t.Context(), auth); err != nil {
				t.Fatal(err)
			}
		}
		updated, refreshed, err := m.RefreshCredential(t.Context(), "credential", false)
		if err != nil || !refreshed || authAccessToken(updated) != "new-access" || exec.calls.Load() != 1 {
			t.Fatalf("missing=%v refreshed=%v err=%v", missing, refreshed, err)
		}
	}
}

func TestRefreshCredentialSkipsStaticAndDisabled(t *testing.T) {
	for _, kind := range []string{"static", "disabled"} {
		m, exec, _ := newCredentialRefreshManager(t, "kimi", time.Now().Add(-time.Hour))
		auth, _ := m.GetByID("credential")
		if kind == "static" {
			delete(auth.Metadata, "refresh_token")
		} else {
			auth.Disabled = true
		}
		if _, err := m.Update(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
		_, refreshed, err := m.RefreshCredential(t.Context(), "credential", true)
		if err != nil || refreshed || exec.calls.Load() != 0 {
			t.Fatalf("%s: refreshed=%v err=%v", kind, refreshed, err)
		}
	}
}

func TestRefreshCredentialHonorsRefreshFailureBackoff(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable} {
		m, exec, _ := newCredentialRefreshManager(t, "kimi", time.Now().Add(-time.Hour))
		exec.err = &Error{HTTPStatus: status, Message: "refresh rejected"}
		for range 2 {
			_, refreshed, err := m.RefreshCredential(t.Context(), "credential", true)
			if err == nil || refreshed {
				t.Fatalf("status=%d refreshed=%v err=%v", status, refreshed, err)
			}
		}
		if exec.calls.Load() != 1 {
			t.Fatalf("status=%d refresh retried during backoff", status)
		}
	}
}

// Err signals that the public API has taken its initial snapshot before it
// waits for the shared credential lock. This avoids timer-based ordering.
type refreshBarrierContext struct {
	context.Context
	ready chan struct{}
	once  sync.Once
}

func (c *refreshBarrierContext) Err() error {
	c.once.Do(func() { close(c.ready) })
	return c.Context.Err()
}

func TestRefreshCredentialReusesInferenceRefresh(t *testing.T) {
	m, exec, _ := newCredentialRefreshManager(t, "kimi", time.Now().Add(-time.Hour))
	exec.started = make(chan struct{})
	exec.release = make(chan struct{})
	inferenceDone := make(chan error, 1)
	go func() {
		_, err := m.refreshAuthForRequest(t.Context(), "credential", "old-access")
		inferenceDone <- err
	}()
	<-exec.started
	ctx := &refreshBarrierContext{Context: t.Context(), ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		auth, refreshed, err := m.RefreshCredential(ctx, "credential", true)
		if err == nil && (!refreshed || authAccessToken(auth) != "new-access") {
			err = errors.New("did not reuse refreshed auth")
		}
		done <- err
	}()
	<-ctx.ready
	close(exec.release)
	if err := <-inferenceDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if exec.calls.Load() != 1 {
		t.Fatalf("refresh calls=%d", exec.calls.Load())
	}
}

func TestRefreshCredentialMissingAndCanceled(t *testing.T) {
	m, exec, _ := newCredentialRefreshManager(t, "kimi", time.Now().Add(-time.Hour))
	if _, _, err := m.RefreshCredential(t.Context(), "missing", true); err == nil {
		t.Fatal("missing credential accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := m.RefreshCredential(ctx, "credential", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if exec.calls.Load() != 0 {
		t.Fatal("canceled request refreshed credential")
	}
}

func TestRefreshCredentialCoalescesConcurrentForcedCalls(t *testing.T) {
	m, exec, _ := newCredentialRefreshManager(t, "kimi", time.Now().Add(time.Hour))
	lock := m.credentialRefreshLock("credential")
	lock.mu.Lock()
	const callers = 8
	done := make(chan error, callers)
	for range callers {
		ctx := &refreshBarrierContext{Context: t.Context(), ready: make(chan struct{})}
		go func() {
			auth, refreshed, err := m.RefreshCredential(ctx, "credential", true)
			if err == nil && (!refreshed || authAccessToken(auth) != "new-access") {
				err = errors.New("did not return refreshed auth")
			}
			done <- err
		}()
		<-ctx.ready
	}
	lock.mu.Unlock()
	for range callers {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if exec.calls.Load() != 1 {
		t.Fatalf("refresh calls=%d", exec.calls.Load())
	}
}
