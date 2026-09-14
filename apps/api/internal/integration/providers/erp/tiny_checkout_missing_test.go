package erp

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

func TestTinyMissingPaidOrderPreservesTheCheckpointAndBinding(t *testing.T) {
	for _, stage := range []string{"missing source", "missing replacement", "missing target after cancellation"} {
		t.Run(stage, func(t *testing.T) {
			provider, fake, op := checkoutFinalizationFixture(t)
			missingID := "1"
			if stage == "missing source" {
				delete(fake.orders, "1")
			} else {
				op.Prepared, op.CreateStarted, op.Replace, op.TargetID = true, true, true, "2"
				op.SourceCancelled = stage == "missing target after cancellation"
				missingID = "2"
			}
			journal := &checkoutTestJournal{}
			if err := journal.Save(t.Context(), op); err != nil {
				t.Fatal(err)
			}
			checkpoint := string(journal.raw)
			for range 2 {
				_, err := provider.FinalizePaidCheckout(t.Context(), journal.resume(t), journal)
				var conflict *providers.TinyCheckoutReconciliationError
				if !errors.As(err, &conflict) || !conflict.Missing || conflict.OrderID != missingID {
					t.Fatalf("missing sale lost its business conflict: %v", err)
				}
				if errors.Is(err, providers.ErrOrderNotFound) {
					t.Fatal("paid sale must not enter unpaid reservation recreation")
				}
				if strings.Contains(err.Error(), "(aberto)") {
					t.Fatal("missing order was assigned an unverified status")
				}
				if fake.writes != 0 || journal.bound != "" || string(journal.raw) != checkpoint {
					t.Fatal("missing sale was replaced or its checkpoint discarded")
				}
			}
		})
	}
}

func TestTinyCheckoutDoesNotTreatAuthorizationOrServerFailureAsMissing(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected write: %s", r.Method)
				}
				w.WriteHeader(status)
			}))
			t.Cleanup(server.Close)
			provider := newTinyAgainst(t, server)
			_, err := provider.readCheckoutOrder(t.Context(), "1")
			var conflict *providers.TinyCheckoutReconciliationError
			if err == nil || errors.As(err, &conflict) {
				t.Fatalf("technical failure misclassified as missing sale: %v", err)
			}
		})
	}
}
