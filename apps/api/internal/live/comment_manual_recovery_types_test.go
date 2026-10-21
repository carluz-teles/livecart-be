package live

import (
	"errors"
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/google/uuid"

	"livecart/apps/api/lib/httpx"
)

func TestRecoverCommentRequestValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		confirmed bool
	}{
		{name: "explicit confirmation", confirmed: true},
		{name: "missing or false confirmation", confirmed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (RecoverCommentRequest{Confirmed: tc.confirmed}).Validate()
			if tc.confirmed {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var fields validation.Errors
			if !errors.As(err, &fields) || fields["confirmed"] == nil {
				t.Fatalf("fields: %v", err)
			}
		})
	}
	id := uuid.NewString()
	for _, tc := range []struct {
		name  string
		ids   [4]string
		valid bool
	}{
		{name: "valid", ids: [4]string{id, id, id, id}, valid: true},
		{name: "store", ids: [4]string{"bad", id, id, id}},
		{name: "event", ids: [4]string{id, "bad", id, id}},
		{name: "comment", ids: [4]string{id, id, "bad", id}},
		{name: "actor", ids: [4]string{id, id, id, ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (RecoverCommentRequest{Confirmed: true}).ToInput(tc.ids[0], tc.ids[1], tc.ids[2], tc.ids[3])
			if (err == nil) != tc.valid {
				t.Fatalf("ToInput: %v", err)
			}
		})
	}
}

func TestManualRecoveryOnlyExplicitRefusalsAreTerminal(t *testing.T) {
	transient := httpx.DomainError(409, httpx.CodeCartERPSyncPending, "sync pending")
	if recoveryProcessingError(transient) != transient {
		t.Fatal("transient edit sync became terminal")
	}
	var rejected *CommentRecoveryRejected
	if !errors.As(recoveryProcessingError(httpx.DomainError(409, httpx.CodeCommentRecoveryConflict, "closed")), &rejected) {
		t.Fatal("closed purchase did not terminate recovery")
	}
}
