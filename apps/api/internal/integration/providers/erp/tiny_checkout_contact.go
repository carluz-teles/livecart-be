package erp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"livecart/apps/api/internal/integration/providers"
)

type tinyCheckoutContact struct {
	ID       int64  `json:"id"`
	Document string `json:"cpfCnpj"`
	Status   string `json:"situacao"`
}

// The reservation may belong to an Instagram placeholder, while the checkout
// document already belongs to another contact. Resolve that identity before
// changing any financial data; a rejected/read-incomplete search is not absence.
func (t *Tiny) resolveCheckoutContact(ctx context.Context, currentID string, customer providers.ERPContactInput) (string, error) {
	if strings.TrimSpace(customer.CpfCnpj) == "" {
		return currentID, nil
	}
	document := digitsOnlyTiny(customer.CpfCnpj)
	if len(document) != 11 && len(document) != 14 {
		return "", fmt.Errorf("tiny: documento do comprador inválido para localizar contato")
	}
	contacts, err := t.findCheckoutContacts(ctx, document)
	if err != nil {
		return "", err
	}
	selected := ""
	for _, contact := range contacts {
		if contact.ID <= 0 || digitsOnlyTiny(contact.Document) != document {
			return "", fmt.Errorf("tiny: busca de contatos retornou documento ou identificação divergente")
		}
		if contact.Status != "A" && contact.Status != "B" {
			return "", fmt.Errorf("tiny: contato do comprador está inativo ou excluído; confira o cadastro no ERP")
		}
		candidate := strconv.FormatInt(contact.ID, 10)
		if selected != "" && selected != candidate {
			return "", fmt.Errorf("tiny: mais de um contato com o documento do comprador; confira os cadastros no ERP")
		}
		selected = candidate
	}
	if selected == "" {
		selected = currentID
	}
	// Re-read even a search match: never replace a different person's document
	// based only on a stale order snapshot or a cached Instagram/contact mapping.
	id, err := strconv.ParseInt(selected, 10, 64)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("tiny: contato de checkout sem identificação verificável")
	}
	var contact tinyCheckoutContact
	if err := t.checkoutRequest(ctx, http.MethodGet, "/contatos/"+selected, nil, &contact); err != nil {
		return "", fmt.Errorf("verifying Tiny checkout contact: %w", err)
	}
	if contact.ID != id || (contact.Status != "A" && contact.Status != "B") {
		return "", fmt.Errorf("tiny: contato de checkout inexistente, inativo ou divergente")
	}
	actualDocument := digitsOnlyTiny(contact.Document)
	if (actualDocument != "" && actualDocument != document) || (len(contacts) > 0 && actualDocument != document) {
		return "", fmt.Errorf("tiny: documento do contato diverge do comprador; cadastro original preservado")
	}
	return selected, nil
}

func (t *Tiny) findCheckoutContacts(ctx context.Context, document string) ([]tinyCheckoutContact, error) {
	const limit = 10
	query := url.Values{"cpfCnpj": {formatBrazilianDocument(document)}, "limit": {strconv.Itoa(limit)}}
	resp, body, err := t.DoRequestRetrying429(ctx, 2, http.MethodGet, tinyAPIBaseURL+"/contatos?"+query.Encode(), nil, t.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("finding Tiny checkout contact: %w", err)
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		// Do not copy the document from the query or provider validation body.
		return nil, fmt.Errorf("finding Tiny checkout contact: status %d", resp.StatusCode)
	}
	var page struct {
		Items      json.RawMessage `json:"itens"`
		Pagination struct {
			Total int `json:"total"`
		} `json:"paginacao"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("decoding Tiny checkout contacts: %w", err)
	}
	if len(page.Items) == 0 || string(page.Items) == "null" {
		return nil, fmt.Errorf("tiny: busca de contatos sem lista verificável")
	}
	var contacts []tinyCheckoutContact
	if err := json.Unmarshal(page.Items, &contacts); err != nil {
		return nil, fmt.Errorf("decoding Tiny checkout contact list: %w", err)
	}
	if page.Pagination.Total > len(contacts) || len(contacts) >= limit {
		return nil, fmt.Errorf("tiny: busca de contatos incompleta; confira os cadastros do comprador no ERP")
	}
	return contacts, nil
}
