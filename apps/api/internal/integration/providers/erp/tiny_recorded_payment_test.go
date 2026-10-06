package erp

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

type recordedPaymentFixture struct {
	Name      string                           `json:"name"`
	Paid      int64                            `json:"paid_cents"`
	Verified  bool                             `json:"verified"`
	Order     tinyCheckoutOrder                `json:"order"`
	Accounts  []tinyReceivable                 `json:"accounts"`
	Receipts  map[string][]tinyRecordedReceipt `json:"receipts"`
	Malformed string
	Failure   string
	Writes    int
}

func recordedPaymentFixtures(t *testing.T) []recordedPaymentFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/tiny_recorded_payments.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []recordedPaymentFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func (f *recordedPaymentFixture) provider(t *testing.T) *Tiny {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			f.Writes++
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == f.Failure {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var body any
		switch {
		case r.URL.Path == "/pedidos/1":
			body = f.Order
		case r.URL.Path == "/contas-receber":
			if r.URL.Query().Get("idVenda") != "1" {
				t.Error("receivables not scoped to the order")
			}
			if f.Malformed != "" {
				_, _ = w.Write([]byte(f.Malformed))
				return
			}
			page := f.Accounts
			offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
			if err != nil || offset < 0 {
				t.Error("invalid account pagination")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if offset < len(page) {
				page = page[offset:min(offset+100, len(page))]
			} else {
				page = []tinyReceivable{}
			}
			body = map[string]any{"itens": page}
		case strings.HasSuffix(r.URL.Path, "/recebimentos"):
			parts := strings.Split(r.URL.Path, "/")
			receipts := f.Receipts[parts[2]]
			if len(receipts) == 0 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			body = receipts
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	return newTinyAgainst(t, srv)
}

func TestTinyRecordedPaymentsAuditedScenarios(t *testing.T) {
	for _, fixture := range recordedPaymentFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			provider := fixture.provider(t)
			for range 2 {
				err := provider.VerifyRecordedOrderPayment(t.Context(), "1", fixture.Paid)
				if (err == nil) != fixture.Verified {
					t.Fatalf("verified=%v, expected %v: %v", err == nil, fixture.Verified, err)
				}
			}
			if fixture.Writes != 0 {
				t.Fatal("external payment reconciliation wrote to Tiny")
			}
		})
	}
}

func TestTinyRecordedPaymentRequiresCompleteSettlementEvidence(t *testing.T) {
	for _, name := range []string{
		"missing titles", "missing list", "null list", "malformed list", "duplicate title", "open title",
		"partial title", "cancelled title", "remaining balance", "missing receipts", "duplicate receipt",
		"wrong receipt total", "negative fee", "missing fee", "missing paid", "missing adjustment",
		"discount", "interest", "addition", "reversal", "unknown movement", "missing receipt id",
		"invalid receipt date", "invalid title date", "invalid title id", "wrong order id", "cancelled order",
		"unapproved order", "unknown order status", "order read failure", "accounts read failure",
		"receipts read failure", "invoiced without receipt", "nonpositive ledger", "overflow",
	} {
		t.Run(name, func(t *testing.T) {
			fixture := recordedPaymentFixtures(t)[1] // One paid title, consolidated card schedule.
			if fixture.Name != "consolidated_card" {
				t.Fatal("unexpected fixture order")
			}
			receipt := &fixture.Receipts["10"][0]
			amount := 1.0
			switch name {
			case "missing titles":
				fixture.Accounts = nil
			case "missing list":
				fixture.Malformed = `{}`
			case "null list":
				fixture.Malformed = `{"itens":null}`
			case "malformed list":
				fixture.Malformed = `{"itens":{}}`
			case "duplicate title":
				fixture.Accounts = append(fixture.Accounts, fixture.Accounts[0])
			case "open title":
				fixture.Accounts[0].Status = "aberto"
			case "partial title":
				fixture.Accounts[0].Status = "parcial"
			case "cancelled title":
				fixture.Accounts[0].Status = "cancelada"
			case "remaining balance":
				fixture.Accounts[0].Balance = 1
			case "missing receipts":
				fixture.Receipts["10"] = nil
			case "duplicate receipt":
				fixture.Receipts["10"] = append(fixture.Receipts["10"], *receipt)
			case "wrong receipt total":
				receipt.Paid = &amount
			case "negative fee":
				amount = -1
				receipt.Fee = &amount
			case "missing fee":
				receipt.Fee = nil
			case "missing paid":
				receipt.Paid = nil
			case "missing adjustment":
				receipt.Interest = nil
			case "discount":
				receipt.Discount = &amount
			case "interest":
				receipt.Interest = &amount
			case "addition":
				receipt.Addition = &amount
			case "reversal":
				receipt.Type = 2
			case "unknown movement":
				receipt.Type = 0
			case "missing receipt id":
				receipt.ID = 0
			case "invalid receipt date":
				receipt.Date = ""
			case "invalid title date":
				fixture.Accounts[0].DueDate = ""
			case "invalid title id":
				fixture.Accounts[0].ID = 0
			case "wrong order id":
				fixture.Order.ID = 2
			case "cancelled order":
				fixture.Order.Status = 2
			case "unapproved order":
				fixture.Order.Status = 0
			case "unknown order status":
				fixture.Order.Status = 999
			case "order read failure":
				fixture.Failure = "/pedidos/1"
			case "accounts read failure":
				fixture.Failure = "/contas-receber"
			case "receipts read failure":
				fixture.Failure = "/contas-receber/10/recebimentos"
			case "invoiced without receipt":
				fixture.Order.Status, fixture.Order.InvoiceID = 1, 99
				fixture.Receipts["10"] = nil
			case "nonpositive ledger":
				fixture.Paid = 0
			case "overflow":
				fixture.Paid, fixture.Order.Total = math.MaxInt64, float64(math.MaxInt64)/100
			}
			provider := fixture.provider(t)
			if err := provider.VerifyRecordedOrderPayment(t.Context(), "1", fixture.Paid); err == nil {
				t.Fatal("incomplete financial evidence was accepted")
			}
			if fixture.Writes != 0 {
				t.Fatal("failed verification wrote to Tiny")
			}
		})
	}
}

func TestTinyRecordedPaymentReadsAllAccountPages(t *testing.T) {
	fixture := recordedPaymentFixtures(t)[1]
	fixture.Paid, fixture.Order.Total = 10100, 101
	fixture.Accounts = []tinyReceivable{}
	one, zero := 1.0, 0.0
	for id := range int64(101) {
		id++
		fixture.Accounts = append(fixture.Accounts, tinyReceivable{
			ID: id, Status: "pago", Value: 1, Balance: 0, DueDate: "2026-10-05",
		})
		fixture.Receipts[strconv.FormatInt(id, 10)] = []tinyRecordedReceipt{{
			ID: id, Date: "2026-10-05", Type: 1, Paid: &one, Fee: &zero,
			Interest: &zero, Discount: &zero, Addition: &zero,
		}}
	}
	provider := fixture.provider(t)
	if err := provider.VerifyRecordedOrderPayment(t.Context(), "1", fixture.Paid); err != nil {
		t.Fatal(err)
	}
	if fixture.Writes != 0 {
		t.Fatal("paginated verification wrote to Tiny")
	}
}
