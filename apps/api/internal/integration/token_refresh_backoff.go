package integration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"livecart/apps/api/internal/events"
)

var errTokenRefreshDeferred = events.NewDeferredError("token refresh deferred until the provider cooldown ends")

// A generic Bling 403 may be an IP block, not revoked credentials. Keep the
// connection and its credentials, but share the cooldown across workers/deploys.
func tokenRefreshDeferred(row *IntegrationRow, now time.Time) bool {
	if row.Provider != "bling" {
		return false
	}
	failure, ok := row.Metadata["tokenRefreshFailure"].(map[string]any)
	if !ok {
		return false
	}
	raw, _ := failure["nextAttemptAt"].(string)
	next, err := time.Parse(time.RFC3339, raw)
	return err == nil && now.Before(next)
}

func recordBlingTokenCooldown(
	ctx context.Context, repo *Repository, row *IntegrationRow, refreshErr error,
) error {
	var status interface{ Status() int }
	if row.Provider != "bling" || !errors.As(refreshErr, &status) || status.Status() != 403 {
		return nil
	}
	var permanent interface{ Permanent() bool }
	if errors.As(refreshErr, &permanent) && permanent.Permanent() {
		return nil // The existing status=error flow requires reconnection.
	}
	attempts := 1
	if previous, ok := row.Metadata["tokenRefreshFailure"].(map[string]any); ok {
		if n, ok := previous["attempts"].(float64); ok {
			attempts += int(n)
		}
	}
	metadata := make(map[string]any, len(row.Metadata)+1)
	for key, value := range row.Metadata {
		metadata[key] = value
	}
	metadata["tokenRefreshFailure"] = map[string]any{
		"attempts": attempts, "statusCode": 403,
		"lastAttemptAt": time.Now().UTC().Format(time.RFC3339),
		"nextAttemptAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	if err := repo.UpdateMetadata(ctx, row.ID, metadata); err != nil {
		return fmt.Errorf("recording Bling token cooldown: %w", err)
	}
	return nil
}
