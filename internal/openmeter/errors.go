// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrMeterNotFound is the sentinel returned by GetMeter when OpenMeter
// responds with a 404 for the requested meter slug. Callers compare with
// errors.Is to decide whether to create or update the meter; DeleteMeter
// treats it as success.
var ErrMeterNotFound = errors.New("openmeter: meter not found")

// TransientError wraps a failure that the caller should retry with backoff.
// Network errors, 429s, and 5xx responses all surface as TransientError.
type TransientError struct {
	// Err is the underlying error (network error, or a synthetic error
	// constructed from an HTTP response body).
	Err error
	// StatusCode is the HTTP status code when the failure came from a
	// response; zero for pre-response failures (network, TLS, DNS).
	StatusCode int
	// RetryAfter is parsed from the Retry-After header when present on a
	// 429 or 503. Zero means the header was not present or not parseable;
	// callers should fall back to their own backoff schedule.
	RetryAfter time.Duration
}

// Error implements error.
func (e *TransientError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("openmeter: transient error (status=%d): %v", e.StatusCode, e.Err)
	}
	return fmt.Sprintf("openmeter: transient error: %v", e.Err)
}

// Unwrap exposes the underlying error so errors.Is / errors.As traverse it.
func (e *TransientError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// PermanentError wraps a failure the caller MUST NOT retry. 4xx responses
// (other than 429) surface as PermanentError. The response body is captured
// verbatim for surfacing to the user via status conditions.
type PermanentError struct {
	Err          error
	StatusCode   int
	ResponseBody string
}

// Error implements error.
func (e *PermanentError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("openmeter: permanent error (status=%d): %v", e.StatusCode, e.Err)
	}
	return fmt.Sprintf("openmeter: permanent error: %v", e.Err)
}

// Unwrap exposes the underlying error.
func (e *PermanentError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// IsTransient reports whether err (or any error wrapped in it) is a
// TransientError.
func IsTransient(err error) bool {
	var t *TransientError
	return errors.As(err, &t)
}

// IsPermanent reports whether err (or any error wrapped in it) is a
// PermanentError.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// classify inspects an HTTP status and the SDK's parsed error body and
// returns the correct provider error. It is the single place that turns an
// OpenMeter HTTP response status into a retriable-vs-permanent decision.
//
// A status of 0 is treated as an unknown non-HTTP failure (the SDK returned
// no usable response); in that case err must be non-nil and is used as the
// underlying error.
func classify(status int, body string, err error) error {
	switch {
	case status >= 200 && status < 300:
		// Not an error.
		return nil

	case status == http.StatusTooManyRequests:
		return &TransientError{
			Err:        fmt.Errorf("rate limited by openmeter: %s", body),
			StatusCode: status,
		}

	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return &PermanentError{
			Err:          fmt.Errorf("openmeter rejected authentication (status=%d); verify bearer token is set and valid: %s", status, body),
			StatusCode:   status,
			ResponseBody: body,
		}

	case status >= 500:
		return &TransientError{
			Err:        fmt.Errorf("openmeter server error: %s", body),
			StatusCode: status,
		}

	case status >= 400:
		return &PermanentError{
			Err:          fmt.Errorf("openmeter rejected request: %s", body),
			StatusCode:   status,
			ResponseBody: body,
		}

	case status == 0 && err != nil:
		// Network-level failure (DNS, connection refused, TLS, etc.).
		return &TransientError{Err: err}

	default:
		// 1xx / 3xx unexpected at this layer; Go's http client follows
		// redirects so we should not see 3xx here. Treat as permanent so
		// the reconciler surfaces the surprise rather than looping.
		return &PermanentError{
			Err:        fmt.Errorf("unexpected status from openmeter (status=%d): %s", status, body),
			StatusCode: status,
		}
	}
}
