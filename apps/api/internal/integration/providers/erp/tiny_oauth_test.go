package erp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestTinyOAuthClassifiesFailuresWithoutLeakingCredentials(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		permanent bool
	}{
		{"gateway timeout", 504, "gateway contains secret-token", false},
		{"unavailable", 503, `{"error":"invalid_grant"}`, false},
		{"rate limited", 429, `{"error":"temporarily_unavailable"}`, false},
		{"rate limited with credential code", 429, `{"error":"invalid_grant"}`, false},
		{"revoked", 400, `{"error":"invalid_grant","error_description":"secret-token"}`, true},
		{"invalid client", 401, `{"error":"invalid_client"}`, true},
		{"unauthorized client", 400, `{"error":"unauthorized_client"}`, true},
		{"unknown forbidden", 403, `{"error":"secret-token"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.PostForm.Get("client_secret") != "secret+&=" || r.PostForm.Get("refresh_token") != "refresh+&=" {
					t.Error("OAuth credentials were not form encoded")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			provider, err := NewTiny(TinyConfig{
				Credentials: &Credentials{AccessToken: "old", RefreshToken: "refresh+&="},
				ClientID:    "client", ClientSecret: "secret+&=", Logger: zap.NewNop(),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.refreshTokenAt(t.Context(), server.URL)
			var classified interface {
				Status() int
				Permanent() bool
			}
			if !errors.As(err, &classified) || classified.Status() != tc.status || classified.Permanent() != tc.permanent {
				t.Fatalf("incorrect OAuth classification: %v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("credential leaked in error")
			}
		})
	}
}

func TestTinyRefreshValidatesSuccessAndKeepsUnrotatedRefreshToken(t *testing.T) {
	for _, tc := range []struct {
		name, body, refresh string
		success             bool
	}{
		{"rotated", `{"access_token":"new","refresh_token":"rotated","expires_in":3600}`, "rotated", true},
		{"not rotated", `{"access_token":"new","expires_in":3600}`, "original", true},
		{"empty token", `{"expires_in":3600}`, "", false},
		{"invalid response", `secret-token`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tc.body) }))
			defer server.Close()
			provider, err := NewTiny(TinyConfig{
				Credentials: &Credentials{AccessToken: "old", RefreshToken: "original"},
				ClientID:    "client", ClientSecret: "secret", Logger: zap.NewNop(),
			})
			if err != nil {
				t.Fatal(err)
			}
			creds, err := provider.refreshTokenAt(t.Context(), server.URL)
			if (err == nil) != tc.success {
				t.Fatalf("success=%v err=%v", tc.success, err)
			}
			if tc.success && (creds.AccessToken != "new" || creds.RefreshToken != tc.refresh) {
				t.Fatal("lost valid credentials")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := provider.refreshTokenAt(ctx, server.URL); !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
}
