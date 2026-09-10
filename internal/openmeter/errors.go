// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrMeterNotFound is the sentinel returned by GetMeter when OpenMeter
// responds with a 404 for the requested meter slug. Callers compare with
// errors.Is to decide whether to create or update the meter; DeleteMeter
// treats it as success.
var ErrMeterNotFound = errors.New("openmeter: meter not found")

// ErrCustomerNotFound is the sentinel returned by GetCustomer when OpenMeter
// responds with a 404 for the requested customer key. Callers compare with
// errors.Is to decide whether to create or update the customer; DeleteCustomer
// treats it as success.
var ErrCustomerNotFound = errors.New("openmeter: customer not found")

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

// classify inspects an HTTP response and body and returns the correct
// provider error. resp may be nil, which is treated as a transient
// network-level failure; in that case networkErr must be non-nil and is
// used as the underlying error.
//
// Call sites pass the already-read body bytes (generated *WithResponse
// clients populate Body/HTTPResponse eagerly) so the response body is
// captured for surfacing in PermanentError.ResponseBody and so the body
// text is available as part of the TransientError message.
func classify(resp *http.Response, bodyBytes []byte, networkErr error) error {
	if resp == nil {
		// Network-level failure (DNS, connection refused, TLS, etc.).
		if networkErr == nil {
			networkErr = errors.New("nil response and nil network error")
		}
		return &TransientError{Err: networkErr}
	}

	status := resp.StatusCode
	bodySnippet := strings.TrimSpace(string(bodyBytes))
	// Cap body snippet to avoid pathological sizes leaking into logs.
	const maxBody = 4096
	if len(bodySnippet) > maxBody {
		bodySnippet = bodySnippet[:maxBody] + "...(truncated)"
	}

	switch {
	case status >= 200 && status < 300:
		// Not an error.
		return nil

	case status == http.StatusTooManyRequests:
		return &TransientError{
			Err:        fmt.Errorf("rate limited by openmeter: %s", bodySnippet),
			StatusCode: status,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}

	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return &PermanentError{
			Err:          fmt.Errorf("openmeter rejected authentication (status=%d); verify the bearer token is set and valid: %s", status, bodySnippet),
			StatusCode:   status,
			ResponseBody: bodySnippet,
		}

	case status == http.StatusRequestTimeout, status == http.StatusTooEarly:
		// 4xx by number, retryable by meaning. Without this they fall into
		// the >=400 permanent branch below, which costs a 5-minute backoff
		// and a Warning event for what is really a blip.
		return &TransientError{
			Err:        fmt.Errorf("openmeter transient client-side status %d: %s", status, bodySnippet),
			StatusCode: status,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}

	case status >= 500:
		return &TransientError{
			Err:        fmt.Errorf("openmeter server error: %s", bodySnippet),
			StatusCode: status,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}

	case status >= 400:
		return &PermanentError{
			Err:          fmt.Errorf("openmeter rejected request: %s", bodySnippet),
			StatusCode:   status,
			ResponseBody: bodySnippet,
		}

	default:
		// 1xx / 3xx unexpected at this layer; Go's http client follows
		// redirects so we should not see 3xx here. Treat as permanent so
		// the reconciler surfaces the surprise rather than looping.
		return &PermanentError{
			Err:          fmt.Errorf("unexpected status from openmeter (status=%d): %s", status, bodySnippet),
			StatusCode:   status,
			ResponseBody: bodySnippet,
		}
	}
}

// parseRetryAfter parses a Retry-After header value. Per HTTP spec this may
// be either a delta-seconds integer or an HTTP-date; both are supported.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}
