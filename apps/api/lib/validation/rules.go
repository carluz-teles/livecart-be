// Package validation holds reusable ozzo-validation rules shared across domains,
// so a rule (slug, document formats, enums...) is defined once and reused in any
// Request's Validate().
package validation

import (
	"regexp"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// slugRegex matches valid URL slugs: lowercase letters, numbers and hyphens,
// not starting or ending with a hyphen.
var slugRegex = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Slug is a reusable ozzo rule enforcing the URL-slug format. Use as:
//
//	validation.Field(&r.Slug, validation.Required, appvalidation.Slug)
var Slug = validation.Match(slugRegex).Error("must be a valid slug (lowercase letters, numbers and hyphens)")
