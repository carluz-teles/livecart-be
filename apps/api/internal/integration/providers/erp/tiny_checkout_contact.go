package erp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
)

type tinyCheckoutContact struct {
	ID       int64  `json:"id"`
	Document string `json:"cpfCnpj"`
	Status   string `json:"situacao"`
	Name     string `json:"nome"`
	Email    string `json:"email"`
	Phone    string `json:"telefone"`
	Mobile   string `json:"celular"`
}

type tinyCheckoutContactConflict struct{ reason string }

func (e *tinyCheckoutContactConflict) Error() string { return "tiny: " + e.reason }

func (c tinyCheckoutContact) active() bool { return c.Status == "A" || c.Status == "B" }

// A duplicated document alone cannot choose between distinct registrations.
// Corroborate it with the checkout's name, email AND phone before breaking a tie.
func (c tinyCheckoutContact) matchesBuyer(customer providers.ERPContactInput) bool {
	phone := digitsOnlyTiny(customer.Phone)
	return strings.TrimSpace(customer.Name) != "" && strings.TrimSpace(customer.Email) != "" && phone != "" &&
		sameTinyCheckoutText(stripAccents(c.Name), stripAccents(customer.Name)) &&
		strings.EqualFold(strings.TrimSpace(c.Email), strings.TrimSpace(customer.Email)) &&
		(phone == digitsOnlyTiny(c.Phone) || phone == digitsOnlyTiny(c.Mobile))
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
	active := map[string]tinyCheckoutContact{}
	for _, contact := range contacts {
		// Deleted/inactive registrations can still be returned by the CPF
		// search. They are not candidates, and do not invalidate active ones.
		if contact.Status == "E" || contact.Status == "I" {
			continue
		}
		if contact.ID <= 0 || digitsOnlyTiny(contact.Document) != document {
			return "", fmt.Errorf("tiny: busca de contatos retornou documento ou identificação divergente")
		}
		if !contact.active() {
			return "", &tinyCheckoutContactConflict{reason: "situação do contato não reconhecida; confira o cadastro no ERP"}
		}
		candidate := strconv.FormatInt(contact.ID, 10)
		active[candidate] = contact
	}
	selected := currentID
	corroborate := false
	if _, found := active[currentID]; !found && len(active) > 0 {
		var selectedID int64
		for _, contact := range active {
			if len(active) > 1 && !contact.matchesBuyer(customer) {
				continue
			}
			// Use the lowest matching ID as a stable tie-breaker. Once selected,
			// the saved binding takes precedence on subsequent retries.
			if selectedID == 0 || contact.ID < selectedID {
				selectedID = contact.ID
			}
		}
		if selectedID == 0 {
			return "", &tinyCheckoutContactConflict{reason: "mais de um contato ativo com o documento; nome, email e telefone não permitem confirmar o comprador"}
		}
		selected = strconv.FormatInt(selectedID, 10)
		corroborate = len(active) > 1
	} else if len(active) == 0 && len(contacts) > 0 {
		return "", &tinyCheckoutContactConflict{reason: "contato do comprador está inativo ou excluído; confira o cadastro no ERP"}
	}
	// Re-read even a search match: never replace a different person's document
	// based only on a stale order snapshot or a cached Instagram/contact mapping.
	id, err := strconv.ParseInt(selected, 10, 64)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("tiny: contato de checkout sem identificação verificável")
	}
	contact, err := t.readCheckoutContact(ctx, selected)
	if err != nil {
		return "", fmt.Errorf("verifying Tiny checkout contact: %w", err)
	}
	if contact.ID != id || !contact.active() {
		return "", &tinyCheckoutContactConflict{reason: "contato de checkout inexistente, inativo ou divergente"}
	}
	actualDocument := digitsOnlyTiny(contact.Document)
	if (actualDocument != "" && actualDocument != document) || (len(contacts) > 0 && actualDocument != document) {
		return "", &tinyCheckoutContactConflict{reason: "documento do contato diverge do comprador; cadastro original preservado"}
	}
	if corroborate && !contact.matchesBuyer(customer) {
		return "", &tinyCheckoutContactConflict{reason: "cadastro do comprador alterado durante a conferência"}
	}
	if len(contacts) > 1 {
		t.Logger.Info("tiny checkout contact candidates resolved", zap.String("contact_id", selected),
			zap.Int("candidates", len(contacts)), zap.Int("active_candidates", len(active)), zap.Bool("buyer_details_verified", corroborate))
	}
	return selected, nil
}

func (t *Tiny) readCheckoutContact(ctx context.Context, id string) (*tinyCheckoutContact, error) {
	var contact tinyCheckoutContact
	if err := t.checkoutRequest(ctx, http.MethodGet, "/contatos/"+id, nil, &contact); err != nil {
		return nil, err
	}
	return &contact, nil
}

func (t *Tiny) updateCheckoutContact(ctx context.Context, id string, customer providers.ERPContactInput) error {
	if document := digitsOnlyTiny(customer.CpfCnpj); document != "" {
		contact, err := t.readCheckoutContact(ctx, id)
		if err != nil {
			return err
		}
		if strconv.FormatInt(contact.ID, 10) != id || !contact.active() {
			return &tinyCheckoutContactConflict{reason: "contato de checkout inexistente, inativo ou divergente"}
		}
		actual := digitsOnlyTiny(contact.Document)
		if actual != "" && actual != document {
			return &tinyCheckoutContactConflict{reason: "documento do contato diverge do comprador; cadastro original preservado"}
		}
		if actual == document {
			// Do not resubmit an unchanged document: legacy Tiny registrations
			// may share it and trigger duplicate validation on a redundant PUT.
			customer.CpfCnpj = ""
		}
	}
	return t.UpdateContact(ctx, id, customer)
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
