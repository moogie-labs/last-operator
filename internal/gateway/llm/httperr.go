package llm

import (
	"fmt"
	"net/http"
)

// classifyHTTP maps a provider's HTTP error status to the llm error the gateway
// maps to a gRPC code. detail is shown only for requests the provider rejected
// as invalid. It is never shown for authentication failures, whose text may
// quote part of the key.
func classifyHTTP(code int, detail string) error {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("%w (HTTP %d)", ErrAuth, code)
	case code == http.StatusNotFound:
		return fmt.Errorf("%w (HTTP %d)", ErrModel, code)
	case code == http.StatusTooManyRequests || code >= http.StatusInternalServerError:
		return fmt.Errorf("%w (HTTP %d)", ErrTransient, code)
	default:
		return fmt.Errorf("%w (HTTP %d): %s", ErrBadRequest, code, detail)
	}
}
