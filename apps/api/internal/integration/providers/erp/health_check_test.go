package erp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"livecart/apps/api/internal/integration/providers"
)

type healthCheckTransport func(*http.Request) (*http.Response, error)

func (f healthCheckTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestTinyHealthCheckMeasuresAllBatches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core, logs := observer.New(zap.InfoLevel)
		provider, err := NewTiny(TinyConfig{Credentials: &Credentials{AccessToken: "test"}, Logger: zap.New(core)})
		if err != nil {
			t.Fatal(err)
		}
		var active, peak, calls atomic.Int32
		provider.HTTPClient.Transport = healthCheckTransport(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(7 * time.Second)
			return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		})
		start := time.Now()
		result, err := provider.HealthCheck(t.Context())
		if err != nil || len(result.Items) != 7 || calls.Load() != 7 || peak.Load() != 3 {
			t.Fatalf("result=%+v err=%v calls=%d peak=%d", result, err, calls.Load(), peak.Load())
		}
		entries := logs.FilterMessage("tiny health check completed").All()
		if len(entries) != 1 || entries[0].ContextMap()["duration"] != time.Since(start) || time.Since(start) != 21*time.Second {
			t.Fatalf("duration must include queued batches: elapsed=%s entries=%+v", time.Since(start), entries)
		}
	})
}

func TestTinyHealthCheckDeadlinePreservesPartialResults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		provider, err := NewTiny(TinyConfig{Credentials: &Credentials{AccessToken: "test"}, Logger: zap.NewNop()})
		if err != nil {
			t.Fatal(err)
		}
		provider.HTTPClient.Transport = healthCheckTransport(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/formas-pagamento") {
				return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			}
			<-req.Context().Done()
			return nil, req.Context().Err()
		})
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		start := time.Now()
		result, err := provider.HealthCheck(ctx)
		if err != nil || len(result.Items) != 7 || time.Since(start) != 30*time.Second {
			t.Fatalf("partial audit: result=%+v err=%v elapsed=%s", result, err, time.Since(start))
		}
		for i, item := range result.Items {
			want := providers.ERPHealthStatusUnknown
			if i < 2 {
				want = providers.ERPHealthStatusMissing // A successful empty response is conclusive.
			}
			if item.Status != want {
				t.Fatalf("item %d: status=%s, want %s", i, item.Status, want)
			}
		}
	})
}

// Erro transitório de lookup NÃO pode virar pendência (falso "missing") —
// bug de campo jul/2026: 429/timeout do Tiny marcava cadastro como faltando.
func TestHealthCheckItemStatuses(t *testing.T) {
	cases := []struct {
		name      string
		matchedID int64
		err       error
		want      providers.ERPHealthCheckStatus
	}{
		{"encontrado", 42, nil, providers.ERPHealthStatusOK},
		{"ausente de verdade", 0, nil, providers.ERPHealthStatusMissing},
		{"lookup falhou (transitório)", 0, errors.New("429 too many requests"), providers.ERPHealthStatusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := healthCheckItem(providers.ERPHealthFormaEnvio, "Correios", tc.matchedID, tc.err, "d", "p")
			if item.Status != tc.want {
				t.Fatalf("status = %q, esperado %q", item.Status, tc.want)
			}
		})
	}
}
