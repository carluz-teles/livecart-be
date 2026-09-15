//go:build integration

package notification

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestCompleteTestRecipientSetup_SharedInstagram(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	svc := NewService(testQueries, &fakeDMSender{}, zap.NewNop())
	seed := func(code string, expired bool) string {
		t.Helper()
		var id string
		expires := time.Now().Add(time.Hour)
		if expired {
			expires = time.Now().Add(-time.Hour)
		}
		if err := testPool.QueryRow(ctx, `INSERT INTO stores(name,slug,notification_test_setup_code,notification_test_setup_expires_at)
		 VALUES('Shared Instagram', $1, $2, $3) RETURNING id::text`, fmt.Sprintf("shared-setup-%d", time.Now().UnixNano()), code, expires).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	for _, tc := range []struct {
		name      string
		code      string
		connected bool
		consumed  bool
		wantMatch bool
	}{
		{name: "code for second connected store", code: "  livecart-bbbbbb ", connected: true, wantMatch: true},
		{name: "code for unrelated store", code: "LIVECART-CCCCCC", connected: true},
		{name: "no connected stores", code: "LIVECART-CCCCCC"},
		{name: "ordinary message", code: "olá", connected: true},
		{name: "already consumed code", code: "LIVECART-BBBBBB", connected: true, consumed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b, other := seed("LIVECART-AAAAAA", false), seed("LIVECART-BBBBBB", false), seed("LIVECART-CCCCCC", false)
			var stores []string
			if tc.connected {
				stores = []string{a, b}
			}
			if tc.consumed {
				if _, err := svc.CompleteTestRecipientSetup(ctx, stores, "LIVECART-BBBBBB", "original", "original"); err != nil {
					t.Fatal(err)
				}
			}
			got, err := svc.CompleteTestRecipientSetup(ctx, stores, tc.code, "sender", "sender")
			want := ""
			if tc.wantMatch {
				want = b
			}
			if err != nil || got != want {
				t.Fatalf("store=%q want=%q err=%v", got, want, err)
			}
			for _, id := range []string{a, other} {
				r, err := svc.GetTestRecipient(ctx, id)
				if err != nil || r.PSID != "" || r.SetupCode == "" {
					t.Fatalf("unrelated store changed: %+v %v", r, err)
				}
			}
			if tc.consumed {
				r, err := svc.GetTestRecipient(ctx, b)
				if err != nil || r.PSID != "original" {
					t.Fatalf("replay overwrote recipient: %+v %v", r, err)
				}
			}
		})
	}
	duplicateA, duplicateB := seed("LIVECART-DDDDDD", false), seed("LIVECART-DDDDDD", false)
	if got, err := svc.CompleteTestRecipientSetup(ctx, []string{duplicateA, duplicateB}, "LIVECART-DDDDDD", "sender", "sender"); err != nil || got != "" {
		t.Fatalf("ambiguous code selected a store: %q %v", got, err)
	}
	expired := seed("LIVECART-EEEEEE", true)
	if got, err := svc.CompleteTestRecipientSetup(ctx, []string{expired}, "LIVECART-EEEEEE", "sender", "sender"); err != nil || got != "" {
		t.Fatalf("expired code consumed: %q %v", got, err)
	}

	concurrent := seed("LIVECART-FFFFFF", false)
	var wg sync.WaitGroup
	winners := make(chan string, 2)
	for _, sender := range []string{"one", "two"} {
		wg.Add(1)
		go func(sender string) {
			defer wg.Done()
			got, err := svc.CompleteTestRecipientSetup(context.Background(), []string{concurrent}, "LIVECART-FFFFFF", sender, sender)
			if err != nil {
				t.Error(err)
				return
			}
			if got != "" {
				winners <- sender
			}
		}(sender)
	}
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("code consumed %d times", len(winners))
	}
	winner := <-winners
	r, err := svc.GetTestRecipient(ctx, concurrent)
	if err != nil || r.PSID != winner || r.SetupCode != "" {
		t.Fatalf("winning recipient changed: %+v %v", r, err)
	}
}
