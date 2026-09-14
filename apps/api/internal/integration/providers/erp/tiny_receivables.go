package erp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"livecart/apps/api/internal/integration/providers"
)

type tinyReceivable struct {
	ID      int64   `json:"id"`
	Status  string  `json:"situacao"`
	DueDate string  `json:"dataVencimento"`
	Value   float64 `json:"valor"`
	Balance float64 `json:"saldo"`
}

func (t *Tiny) checkoutRequest(ctx context.Context, method, path string, payload, target any) error {
	resp, body, err := t.DoRequestRetrying429(ctx, 2, method, tinyAPIBaseURL+path, payload, t.authHeaders())
	if err != nil {
		return fmt.Errorf("tiny checkout %s %s: %w", method, path, err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return fmt.Errorf("tiny checkout %s %s: status %d: %s", method, path, resp.StatusCode, tinyErrorDetail(body))
	}
	if target != nil {
		// Tiny returns 204 when an open title has no receipts.
		if resp.StatusCode == http.StatusNoContent && method == http.MethodGet && strings.HasSuffix(path, "/recebimentos") {
			return nil
		}
		if err := json.Unmarshal(body, target); err != nil {
			return fmt.Errorf("decoding Tiny %s %s response: %w", method, path, err)
		}
	}
	return nil
}

func (t *Tiny) checkoutReceivables(ctx context.Context, orderID string) ([]tinyReceivable, error) {
	if _, err := strconv.ParseInt(orderID, 10, 64); err != nil {
		return nil, fmt.Errorf("invalid Tiny order ID: %w", err)
	}
	var out []tinyReceivable
	for offset := 0; ; offset += 100 {
		var page struct {
			Items json.RawMessage `json:"itens"`
		}
		path := fmt.Sprintf("/contas-receber?idVenda=%s&limit=100&offset=%d", orderID, offset)
		if err := t.checkoutRequest(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		if len(page.Items) == 0 || string(page.Items) == "null" {
			return nil, fmt.Errorf("tiny: resposta de contas a receber sem lista verificável")
		}
		var items []tinyReceivable
		if err := json.Unmarshal(page.Items, &items); err != nil {
			return nil, fmt.Errorf("parsing Tiny receivables: %w", err)
		}
		out = append(out, items...)
		if len(items) < 100 {
			return out, nil
		}
	}
}

func tinyReceivablesMatch(accounts []tinyReceivable, desired []providers.ERPInstallment) bool {
	if len(accounts) != len(desired) {
		return false
	}
	used := make([]bool, len(accounts))
	for _, p := range desired {
		found := false
		for i, a := range accounts {
			if !used[i] && a.ID > 0 && a.Status != "cancelada" && int64(math.Round(a.Value*100)) == p.AmountCents && strings.HasPrefix(a.DueDate, p.DueDate.Format("2006-01-02")) {
				used[i], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (t *Tiny) verifyCheckoutReceivables(ctx context.Context, orderID string, desired []providers.ERPInstallment) error {
	accounts, err := t.checkoutReceivables(ctx, orderID)
	if err != nil {
		return err
	}
	if len(accounts) > 0 && !tinyReceivablesMatch(accounts, desired) {
		return fmt.Errorf("tiny: contas a receber do pedido %s divergem das parcelas; conciliação financeira necessária", orderID)
	}
	return nil
}

func (t *Tiny) requireUnreceivedAccounts(ctx context.Context, accounts []tinyReceivable) error {
	for _, a := range accounts {
		if (a.Status != "aberto" && a.Status != "atrasadas") || a.Value <= 0 || math.Round(a.Value*100) != math.Round(a.Balance*100) {
			return fmt.Errorf("tiny: título %d com recebimento ou situação protegida; conciliação manual necessária", a.ID)
		}
		var receipts []json.RawMessage
		if err := t.checkoutRequest(ctx, http.MethodGet, fmt.Sprintf("/contas-receber/%d/recebimentos", a.ID), nil, &receipts); err != nil {
			return err
		}
		if len(receipts) != 0 {
			return fmt.Errorf("tiny: título %d possui recebimento; conciliação manual necessária", a.ID)
		}
	}
	return nil
}
