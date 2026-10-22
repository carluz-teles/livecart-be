package erp

import (
	"encoding/json"
	"fmt"
)

type tinyOAuthError struct {
	status int
	code   string
}

func (e *tinyOAuthError) Error() string {
	return fmt.Sprintf("tiny: token endpoint HTTP %d (%s)", e.status, e.code)
}

func (e *tinyOAuthError) Status() int { return e.status }

func (e *tinyOAuthError) Permanent() bool {
	rejectedCredential := e.code == "invalid_grant" || e.code == "invalid_client" || e.code == "unauthorized_client"
	return (e.status == 400 || e.status == 401 || e.status == 403) && rejectedCredential
}

// Allowlist the diagnostic code; OAuth responses may echo credentials.
func newTinyOAuthError(status int, body []byte) error {
	var response struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return &tinyOAuthError{status: status, code: "oauth_error"}
	}
	switch response.Error {
	case "invalid_grant", "invalid_client", "unauthorized_client", "invalid_request", "temporarily_unavailable", "server_error":
	default:
		response.Error = "oauth_error"
	}
	return &tinyOAuthError{status: status, code: response.Error}
}
