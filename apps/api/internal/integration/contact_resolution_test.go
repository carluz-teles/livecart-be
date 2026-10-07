//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

type contactResolutionProvider struct {
	providers.ERPProvider
	matches           []providers.ERPContactResult
	searchErr         error
	updateErr         error
	searches, creates int
	updated           []string
}

func (p *contactResolutionProvider) SearchContacts(_ context.Context, params providers.SearchContactsParams) ([]providers.ERPContactResult, error) {
	p.searches++
	if params.CpfCnpj != "12345678909" {
		return nil, errors.New("unexpected document query")
	}
	return p.matches, p.searchErr
}

func (p *contactResolutionProvider) UpdateContact(_ context.Context, id string, _ providers.ERPContactInput) error {
	p.updated = append(p.updated, id)
	return p.updateErr
}

func (p *contactResolutionProvider) CreateContact(_ context.Context, input providers.ERPContactInput) (*providers.ERPContactResult, error) {
	p.creates++
	return &providers.ERPContactResult{ContactID: "new", Name: input.Name}, nil
}

func TestResolveERPContactReconcilesDocumentBeforeCache(t *testing.T) {
	for _, tc := range []struct {
		name, cached, document, want string
		matches                      []providers.ERPContactResult
		searchErr, updateErr         error
		wantErr                      bool
		searches, creates            int
	}{
		{name: "stale cache", cached: "old", document: "12345678909", matches: []providers.ERPContactResult{{ContactID: "verified"}}, want: "verified", searches: 1},
		{name: "verified cache", cached: "verified", document: "12345678909", matches: []providers.ERPContactResult{{ContactID: "verified"}}, want: "verified", searches: 1},
		{name: "document without cache", document: "12345678909", matches: []providers.ERPContactResult{{ContactID: "verified"}}, want: "verified", searches: 1},
		{name: "no document", cached: "old", want: "old"},
		{name: "new document for cached contact", cached: "old", document: "12345678909", want: "old", searches: 1},
		{name: "new contact", document: "12345678909", want: "new", searches: 1, creates: 1},
		{name: "new contact without document", want: "new", creates: 1},
		{name: "unavailable lookup", cached: "old", document: "12345678909", searchErr: errors.New("provider unavailable"), wantErr: true, searches: 1},
		{name: "unavailable lookup without cache", document: "12345678909", searchErr: errors.New("provider unavailable"), wantErr: true, searches: 1},
		{name: "ambiguous lookup", cached: "old", document: "12345678909", matches: []providers.ERPContactResult{{ContactID: "one"}, {ContactID: "two"}}, wantErr: true, searches: 1},
		{name: "empty contact", cached: "old", document: "12345678909", matches: []providers.ERPContactResult{{}}, wantErr: true, searches: 1},
		{name: "zero contact", cached: "old", document: "12345678909", matches: []providers.ERPContactResult{{ContactID: "0"}}, wantErr: true, searches: 1},
		{name: "optional enrichment failure", cached: "old", document: "12345678909", matches: []providers.ERPContactResult{{ContactID: "verified"}}, updateErr: errors.New("unavailable update"), want: "verified", searches: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, row, _, _ := stockConsistencyFixture(t, syncedProduct)
			if tc.cached != "" {
				if err := testRepo.UpsertERPContact(t.Context(), row.StoreID, row.ID, "buyer", "handle", tc.cached); err != nil {
					t.Fatal(err)
				}
			}
			p := &contactResolutionProvider{matches: tc.matches, searchErr: tc.searchErr, updateErr: tc.updateErr}
			id, err := svc.resolveERPContact(t.Context(), p, row, row.StoreID, "buyer", "handle", "Buyer", tc.document, "", "")
			if (err != nil) != tc.wantErr || id != tc.want || p.searches != tc.searches || p.creates != tc.creates {
				t.Fatalf("resolved=%q err=%v searches=%d creates=%d", id, err, p.searches, p.creates)
			}
			wantCached := tc.want
			if tc.wantErr {
				wantCached = tc.cached
			}
			cached, err := testRepo.GetERPContact(t.Context(), row.StoreID, row.ID, "buyer")
			if err != nil || cached != wantCached {
				t.Fatalf("cached=%q want=%q err=%v", cached, wantCached, err)
			}
			if tc.wantErr || tc.creates > 0 {
				if len(p.updated) != 0 {
					t.Fatalf("unexpected contact mutation: %v", p.updated)
				}
			} else if len(p.updated) != 1 || p.updated[0] != tc.want {
				t.Fatalf("wrong contact enriched: %v", p.updated)
			}
		})
	}
}
