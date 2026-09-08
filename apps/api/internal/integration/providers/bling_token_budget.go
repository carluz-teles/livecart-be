package providers

import (
	"context"
	"errors"
	"time"
)

// Bling's token endpoint is limited by source IP, across accounts and apps.
// Share a conservative 15/minute budget across LiveCart replicas. Call before
// taking an integration refresh lock so budget acquisition cannot exhaust the
// connection pool while refresh transactions wait for each other.
func (f *Factory) WaitBlingToken(ctx context.Context) error {
	if f.rateLimitManager == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return f.rateLimitManager.GetOrCreateBling("bling:oauth:shared-egress", 0.25).Wait(ctx)
}

func (f *Factory) ObserveBlingTokenError(ctx context.Context, err error) error {
	var status interface{ Status() int }
	if f.rateLimitManager == nil || !errors.As(err, &status) || status.Status() != 429 {
		return nil
	}
	// Bling documents a one-hour IP block for exceeding the token endpoint quota.
	return f.rateLimitManager.GetOrCreateBling("bling:oauth:shared-egress", 0.25).BlockFor(ctx, time.Hour)
}
