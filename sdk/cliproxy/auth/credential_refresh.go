package auth

import (
	"context"
	"errors"
	"strings"
	"time"
)

// RefreshCredential returns the current credential, refreshing it when CPA's
// provider policy says it is due. Force bypasses freshness, but not a previous
// refresh failure's backoff or terminal unauthorized state. Callers handling a
// 401 can force one refresh without implementing a second OAuth lifecycle.
// It shares the lock, persistence and scheduler updates used by inference and
// automatic refresh. The boolean also reports reuse of a concurrent refresh.
func (m *Manager) RefreshCredential(ctx context.Context, id string, force bool) (*Auth, bool, error) {
	if m == nil {
		return nil, false, errors.New("auth manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	initial, ok := m.GetByID(id)
	if !ok || initial == nil {
		return nil, false, errors.New("auth not found")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	lock := m.credentialRefreshLock(id)
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	current, ok := m.GetByID(id)
	if !ok || current == nil {
		return nil, false, errors.New("auth not found")
	}
	if current.Disabled || current.Status == StatusDisabled || current.AuthKind() == AuthKindAPIKey || !authHasRefreshCredential(current) {
		return current, false, nil
	}
	if current.RegistrationEpoch != initial.RegistrationEpoch || authAccessToken(current) != authAccessToken(initial) || current.LastRefreshedAt.After(initial.LastRefreshedAt) {
		return current, true, nil
	}
	now := time.Now()
	if hasUnauthorizedAuthFailure(current) || (current.LastError != nil && now.Before(current.NextRefreshAfter)) {
		return current, false, current.LastError
	}
	if !force && !m.shouldRefresh(current, now) {
		return current, false, nil
	}
	updated, err := m.refreshAuthForRequestLocked(ctx, id, authAccessToken(current))
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}
