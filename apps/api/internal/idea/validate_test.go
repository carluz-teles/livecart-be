package idea

import (
	"strings"
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// hasFieldErr reports whether err is a validation.Errors carrying the given json key.
func hasFieldErr(err error, key string) bool {
	verrs, ok := err.(validation.Errors)
	if !ok {
		return false
	}
	_, present := verrs[key]
	return present
}

func strptr(s string) *string { return &s }

func TestCreateIdeaRequestValidate(t *testing.T) {
	valid := CreateIdeaRequest{
		Title:       "A valid idea title",
		Description: "This is a sufficiently long description body.",
		Category:    "feature",
	}

	cases := []struct {
		name    string
		req     CreateIdeaRequest
		wantErr bool
		field   string // json key expected in error (empty = don't assert key)
	}{
		{"valid", valid, false, ""},

		// Title
		{"title missing", CreateIdeaRequest{Description: valid.Description, Category: valid.Category}, true, "title"},
		{"title too short", func() CreateIdeaRequest { r := valid; r.Title = "short"; return r }(), true, "title"},
		{"title too long", func() CreateIdeaRequest { r := valid; r.Title = strings.Repeat("x", 141); return r }(), true, "title"},

		// Description
		{"description missing", CreateIdeaRequest{Title: valid.Title, Category: valid.Category}, true, "description"},
		{"description too short", func() CreateIdeaRequest { r := valid; r.Description = "too short"; return r }(), true, "description"},
		{"description too long", func() CreateIdeaRequest { r := valid; r.Description = strings.Repeat("x", 5001); return r }(), true, "description"},

		// Category
		{"category missing", CreateIdeaRequest{Title: valid.Title, Description: valid.Description}, true, "category"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
			if tc.wantErr && tc.field != "" && !hasFieldErr(err, tc.field) {
				t.Fatalf("expected error on field %q, got %v", tc.field, err)
			}
		})
	}
}

func TestCreateCommentRequestValidate(t *testing.T) {
	valid := CreateCommentRequest{
		Body:            "A comment body",
		ParentCommentID: strptr("f47ac10b-58cc-4372-a567-0e02b2c3d479"),
	}

	cases := []struct {
		name    string
		req     CreateCommentRequest
		wantErr bool
		field   string
	}{
		{"valid with parent", valid, false, ""},
		{"valid nil parent", CreateCommentRequest{Body: "A comment body"}, false, ""},

		// Body
		{"body missing", CreateCommentRequest{}, true, "body"},
		{"body too long", CreateCommentRequest{Body: strings.Repeat("x", 5001)}, true, "body"},

		// ParentCommentID (is.UUIDv4)
		{"parent not uuid", CreateCommentRequest{Body: "ok", ParentCommentID: strptr("not-a-uuid")}, true, "parentCommentId"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
			if tc.wantErr && tc.field != "" && !hasFieldErr(err, tc.field) {
				t.Fatalf("expected error on field %q, got %v", tc.field, err)
			}
		})
	}
}
