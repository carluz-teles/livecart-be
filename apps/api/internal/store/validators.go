package store

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	digitsOnly = regexp.MustCompile(`\D`)
	ufRegex    = regexp.MustCompile(`^[A-Z]{2}$`)
)

// normalizeAndValidateAddress normalizes CEP/UF and returns an error when the
// non-empty fields have invalid shape. Fields left empty pass through — the
// strict "required for shipping label" check happens when a shipment is
// actually being created, not here.
func normalizeAndValidateAddress(in AddressDTO) (AddressDTO, error) {
	out := in

	if out.State != "" {
		out.State = strings.ToUpper(strings.TrimSpace(out.State))
		if !ufRegex.MatchString(out.State) {
			return out, fmt.Errorf("state must be a 2-letter UF code (e.g. SP)")
		}
	}

	if out.Zip != "" {
		digits := digitsOnly.ReplaceAllString(out.Zip, "")
		if len(digits) != 8 {
			return out, fmt.Errorf("zip must have 8 digits (e.g. 01310100 or 01310-100)")
		}
		out.Zip = digits
	}

	return out, nil
}

// normalizeAndValidateCNPJ accepts CNPJ in any of the common Brazilian
// formats (formatted or digits-only). Empty input is valid and returned as
// empty — CNPJ is optional. Returns the canonical XX.XXX.XXX/XXXX-XX shape.
func normalizeAndValidateCNPJ(in string) (string, error) {
	raw := strings.TrimSpace(in)
	if raw == "" {
		return "", nil
	}
	digits := digitsOnly.ReplaceAllString(raw, "")
	if len(digits) != 14 {
		return "", fmt.Errorf("cnpj must have 14 digits")
	}
	return fmt.Sprintf("%s.%s.%s/%s-%s",
		digits[0:2], digits[2:5], digits[5:8], digits[8:12], digits[12:14],
	), nil
}

// mergeAddress copies non-empty fields from req over current. This preserves
// existing values when the frontend sends a subset on PUT /stores/me.
// Empty strings in the request mean "not sent" — to explicitly clear a
// field the caller would need a dedicated endpoint.
func mergeAddress(current, req AddressDTO) AddressDTO {
	out := current
	if req.Street != "" {
		out.Street = req.Street
	}
	if req.Number != "" {
		out.Number = req.Number
	}
	if req.Complement != "" {
		out.Complement = req.Complement
	}
	if req.District != "" {
		out.District = req.District
	}
	if req.City != "" {
		out.City = req.City
	}
	if req.State != "" {
		out.State = req.State
	}
	if req.Zip != "" {
		out.Zip = req.Zip
	}
	if req.Country != "" {
		out.Country = req.Country
	}
	if req.StateRegister != "" {
		out.StateRegister = req.StateRegister
	}
	return out
}

// mergeUpdateStoreFields applies the same "non-empty wins" merge to the top
// level store fields (whatsapp/email/sms/description/website/cnpj). Name
// is not merged because it is required by the validator and always arrives
// populated.
func mergeUpdateStoreFields(current StoreOutput, req UpdateStoreRequest) UpdateStoreRequest {
	out := req
	if out.WhatsappNumber == "" {
		out.WhatsappNumber = current.WhatsappNumber
	}
	if out.EmailAddress == "" {
		out.EmailAddress = current.EmailAddress
	}
	if out.SMSNumber == "" {
		out.SMSNumber = current.SMSNumber
	}
	if out.Description == "" {
		out.Description = current.Description
	}
	if out.Website == "" {
		out.Website = current.Website
	}
	if out.LogoURL == "" && current.LogoURL != nil {
		out.LogoURL = *current.LogoURL
	}
	if out.CNPJ == "" {
		out.CNPJ = current.CNPJ
	}
	out.Address = mergeAddress(current.Address, out.Address)
	return out
}

// normalizeUpdateStoreRequest merges the incoming request with the store's
// current persisted values (subset semantics), normalizes CEP/UF/CNPJ and
// returns the merged request ready to be applied. Errors carry a
// human-readable message safe to expose via BadRequest.
func normalizeUpdateStoreRequest(current StoreOutput, req UpdateStoreRequest) (UpdateStoreRequest, error) {
	merged := mergeUpdateStoreFields(current, req)

	addr, err := normalizeAndValidateAddress(merged.Address)
	if err != nil {
		return merged, err
	}
	merged.Address = addr

	cnpj, err := normalizeAndValidateCNPJ(merged.CNPJ)
	if err != nil {
		return merged, err
	}
	merged.CNPJ = cnpj

	return merged, nil
}
