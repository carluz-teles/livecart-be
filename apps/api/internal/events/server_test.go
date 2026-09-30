package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/hibiken/asynq"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestErrorHandlerClassifiesFailuresWithoutLosingCorrelation(t *testing.T) {
	busy := NewDeferredError("stock read already in progress")
	for _, tc := range []struct {
		name    string
		err     error
		level   zap.AtomicLevel
		message string
	}{
		{name: "expected contention", err: fmt.Errorf("stock: %w", busy), level: zap.NewAtomicLevelAt(zap.InfoLevel), message: "event handler deferred; retry scheduled"},
		{name: "unexpected failure", err: errors.New("database unavailable"), level: zap.NewAtomicLevelAt(zap.ErrorLevel), message: "event handler failed"},
		{name: "joined real failure stays visible", err: errors.Join(busy, errors.New("database unavailable")), level: zap.NewAtomicLevelAt(zap.ErrorLevel), message: "event handler failed"},
		{name: "skip retry is terminal", err: fmt.Errorf("invalid payload: %w", asynq.SkipRetry), level: zap.NewAtomicLevelAt(zap.ErrorLevel), message: "event handler failed — DEAD-LETTERED (archived)"},
		{name: "revoked is not archived", err: asynq.RevokeTask, level: zap.NewAtomicLevelAt(zap.InfoLevel), message: "event task revoked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			payload, err := json.Marshal(Envelope{EventID: "event-1", Metadata: map[string]string{
				"store_id": "store-1", "integration_id": "integration-1", "secret": "must-not-be-logged",
			}})
			if err != nil {
				t.Fatal(err)
			}
			errorHandler(zap.New(core))(t.Context(), asynq.NewTask("test.event", payload), tc.err)
			entries := logs.All()
			if len(entries) != 1 || entries[0].Message != tc.message || entries[0].Level != tc.level.Level() {
				t.Fatalf("unexpected log: %+v", entries)
			}
			fields := entries[0].ContextMap()
			if fields["event_id"] != "event-1" || fields["store_id"] != "store-1" || fields["integration_id"] != "integration-1" {
				t.Fatalf("missing correlation: %v", fields)
			}
			if _, ok := fields["secret"]; ok {
				t.Fatal("arbitrary metadata leaked")
			}
		})
	}
}
