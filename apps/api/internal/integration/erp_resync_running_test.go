package integration

import (
	"livecart/apps/api/internal/integration/providers"
	"testing"
	"time"
)

func TestERPResyncResponseKeepsFinalCounters(t *testing.T) {
	for _, status := range []string{"queued", "running", "retrying", "completed", "completed_with_errors", "failed"} {
		t.Run(status, func(t *testing.T) {
			checkpoint := &ERPResyncProgress{Status: status, Total: 100, Done: 90, Succeeded: 89, Failed: 1}
			response := toIntegrationResponse(&CreateIntegrationOutput{ERPResync: checkpoint})
			active := status == "queued" || status == "running" || status == "retrying"
			if response.ERPResyncRunning != active || response.ERPResyncDone != 90 || response.ERPResyncTotal != 100 || response.ERPResync != checkpoint {
				t.Fatalf("lost checkpoint in HTTP response: %+v", response)
			}
		})
	}
}

func TestERPResyncLegacyMarkerDoesNotClaimAnActiveWorker(t *testing.T) {
	response := toIntegrationResponse(&CreateIntegrationOutput{Metadata: map[string]any{
		providers.MetadataResyncRunningSince: time.Now().Format(time.RFC3339),
	}})
	if response.ERPResyncRunning || response.ERPResync == nil || response.ERPResync.Status != "interrupted" {
		t.Fatalf("legacy marker blocked a tracked run: %+v", response)
	}
	empty := toIntegrationResponse(&CreateIntegrationOutput{})
	if empty.ERPResyncRunning || empty.ERPResync != nil {
		t.Fatalf("invented a run for an empty integration: %+v", empty)
	}
}
