package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/database"
	"livecart/apps/api/lib/logger"
)

type SQSEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func main() {
	log, err := logger.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	ctx := context.Background()
	pool, err := database.NewPool(ctx, databaseURL)
	if err != nil {
		log.Sugar().Fatalf("connecting to database: %v", err)
	}

	queries := sqlc.New(pool)

	handler := newHandler(log, queries)
	lambda.Start(handler.Handle)
}

type workerHandler struct {
	log     *zap.Logger
	queries *sqlc.Queries
}

func newHandler(log *zap.Logger, queries *sqlc.Queries) *workerHandler {
	return &workerHandler{log: log, queries: queries}
}

func (h *workerHandler) Handle(ctx context.Context, sqsEvent events.SQSEvent) error {
	for _, record := range sqsEvent.Records {
		var evt SQSEvent
		if err := json.Unmarshal([]byte(record.Body), &evt); err != nil {
			h.log.Sugar().Errorf("unmarshaling event: %v", err)
			continue
		}

		h.log.Sugar().Infow("processing event", "type", evt.Type)

		switch evt.Type {
		case "live_started":
			if err := h.handleLiveStarted(ctx, evt.Data); err != nil {
				h.log.Sugar().Errorf("handling live_started: %v", err)
			}
		case "comment":
			if err := h.handleComment(ctx, evt.Data); err != nil {
				h.log.Sugar().Errorf("handling comment: %v", err)
			}
		case "live_ended":
			if err := h.handleLiveEnded(ctx, evt.Data); err != nil {
				h.log.Sugar().Errorf("handling live_ended: %v", err)
			}
		default:
			h.log.Sugar().Warnw("unknown event type", "type", evt.Type)
		}
	}

	return nil
}

func (h *workerHandler) handleLiveStarted(ctx context.Context, data json.RawMessage) error {
	// TODO: implement live_started handler
	return nil
}

func (h *workerHandler) handleComment(ctx context.Context, data json.RawMessage) error {
	// TODO: implement comment processing with keyword detection
	return nil
}

func (h *workerHandler) handleLiveEnded(ctx context.Context, data json.RawMessage) error {
	// TODO: implement cart consolidation
	return nil
}
