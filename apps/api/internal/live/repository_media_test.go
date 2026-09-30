//go:build integration

package live

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"livecart/apps/api/lib/httpx"
)

func TestSessionMediaConflictPreservesTransaction(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	storeID := seedWindowStore(t, ctx, "media-conflict-"+uuid.NewString())
	svc := newWindowService(nil)
	endsAt := time.Now().Add(24 * time.Hour)
	event, err := svc.Create(ctx, CreateLiveInput{StoreID: storeID, Title: "Media conflict", EndsAt: &endsAt})
	if err != nil {
		t.Fatal(err)
	}
	in := CreateSessionInput{StoreID: storeID, EventID: event.ID, Type: "live", Platform: "instagram", PlatformLiveID: uuid.NewString()}
	errorsOut := make(chan error, 2)
	for range 2 {
		go func() { _, err := svc.CreateSession(ctx, in); errorsOut <- err }()
	}
	var successes, conflicts int
	for range 2 {
		err := <-errorsOut
		if err == nil {
			successes++
			continue
		}
		var domainErr *httpx.ServiceError
		if !errors.As(err, &domainErr) || domainErr.Code != 409 || domainErr.Reason != string(httpx.CodeSessionMediaAlreadyLinked) {
			t.Fatalf("expected media conflict, got %v", err)
		}
		conflicts++
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflict=%d", successes, conflicts)
	}
	var sessions, platforms, facts int
	err = testPool.QueryRow(ctx, `SELECT
        (SELECT COUNT(*) FROM live_sessions WHERE event_id=$1),
        (SELECT COUNT(*) FROM live_session_platforms WHERE platform_live_id=$2),
        (SELECT COUNT(*) FROM event_outbox WHERE name='session.created' AND live_event_id=$1)`,
		event.ID, in.PlatformLiveID).Scan(&sessions, &platforms, &facts)
	if err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || platforms != 1 || facts != 1 {
		t.Fatalf("partial transaction: sessions=%d platforms=%d facts=%d", sessions, platforms, facts)
	}
	app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler})
	app.Use(func(c *fiber.Ctx) error { c.Locals("store_id", storeID); return c.Next() })
	app.Post("/lives/:id/sessions", NewHandler(svc, validator.New()).CreateSession)
	request := httptest.NewRequest("POST", "/lives/"+event.ID+"/sessions", strings.NewReader(
		fmt.Sprintf(`{"type":"live","platform":"instagram","platformLiveId":%q}`, in.PlatformLiveID)))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 409 || payload["reason"] != string(httpx.CodeSessionMediaAlreadyLinked) || strings.Contains(string(body), "uq_lsp") {
		t.Fatalf("invalid conflict contract: status=%d body=%s", response.StatusCode, body)
	}
	placeholder, err := svc.CreateSession(ctx, CreateSessionInput{StoreID: storeID, EventID: event.ID, Type: "live"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.LinkSessionMedia(ctx, LinkSessionMediaInput{
		StoreID: storeID, EventID: event.ID, SessionID: placeholder.ID,
		Type: "reel", Platform: "instagram", PlatformLiveID: in.PlatformLiveID,
	})
	var conflict *httpx.ServiceError
	if !errors.As(err, &conflict) || conflict.Code != 409 {
		t.Fatalf("expected link conflict, got %v", err)
	}
	var sessionType string
	if err := testPool.QueryRow(ctx, `SELECT type FROM live_sessions WHERE id=$1`, placeholder.ID).Scan(&sessionType); err != nil {
		t.Fatal(err)
	}
	if sessionType != "live" {
		t.Fatalf("failed media link changed session type to %q", sessionType)
	}
	// A released media can legitimately be linked again.
	if _, err := testPool.Exec(ctx, `UPDATE live_session_platforms SET released_at=now() WHERE platform_live_id=$1`, in.PlatformLiveID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSession(ctx, in); err != nil {
		t.Fatalf("released media rejected: %v", err)
	}
}

func TestSessionMediaErrorDoesNotHideOtherConstraints(t *testing.T) {
	for _, constraint := range []string{"another_unique_index", "live_sessions_event_id_fkey"} {
		t.Run(constraint, func(t *testing.T) {
			cause := &pgconn.PgError{Code: "23505", ConstraintName: constraint}
			err := sessionMediaError(fmt.Errorf("driver: %w", cause))
			if !errors.Is(err, cause) {
				t.Fatal("original database failure hidden")
			}
			var domainErr *httpx.ServiceError
			if errors.As(err, &domainErr) {
				t.Fatal("unrelated failure classified as media conflict")
			}
		})
	}
}
