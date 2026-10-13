// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestClassify_RetryableClientStatusesAreTransient covers 408/425, which are
// 4xx by number but retryable by meaning. They used to fall into the
// catch-all >=400 permanent branch, costing a 5-minute backoff and a
// Warning event for what is really a blip.
func TestClassify_RetryableClientStatusesAreTransient(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooEarly} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			err := classify(&http.Response{StatusCode: status, Header: http.Header{}}, []byte("slow down"), nil)
			if !IsTransient(err) {
				t.Errorf("status %d classified %T, want TransientError", status, err)
			}
			if IsPermanent(err) {
				t.Errorf("status %d must not classify as permanent: %v", status, err)
			}
		})
	}
}

// TestClassify_OtherClientStatusesStayPermanent guards the other side: a
// genuine 400/404 must not become retryable just because 408/425 were
// carved out.
func TestClassify_OtherClientStatusesStayPermanent(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusPreconditionFailed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			err := classify(&http.Response{StatusCode: status, Header: http.Header{}}, []byte("nope"), nil)
			if !IsPermanent(err) {
				t.Errorf("status %d classified %T, want PermanentError", status, err)
			}
		})
	}
}

// TestClassify_ServerErrorsAndRateLimitsAreTransient pins the pre-existing
// behavior these carve-outs sit next to.
func TestClassify_ServerErrorsAndRateLimitsAreTransient(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			if err := classify(&http.Response{StatusCode: status, Header: http.Header{}}, nil, nil); !IsTransient(err) {
				t.Errorf("status %d classified %T, want TransientError", status, err)
			}
		})
	}
}

// TestClassify_ProblemInstanceIsDropped: OpenMeter gives every problem
// response a per-request instance id. With it in the error text, the same
// failure read differently on every call, and the BillingEntitlement
// controller's condition message changed on every reconcile: each status
// patch re-triggered the reconcile at once (~50/s against OpenMeter while
// an account had no payment method).
func TestClassify_ProblemInstanceIsDropped(t *testing.T) {
	body := func(seq string) []byte {
		return []byte(`{"type":"about:blank","title":"Conflict","status":409,"detail":"conflict error: invalid billing setup: customer has no data for stripe app <x> & co","instance":"urn:request:openmeter-api-58668987d8-shcgz/wndS4FINFt-` + seq + `"}`)
	}
	resp := &http.Response{StatusCode: http.StatusConflict, Header: http.Header{}}

	first := classifySubscription(resp, body("018939"))
	second := classifySubscription(resp, body("018942"))
	if first.Error() != second.Error() {
		t.Fatalf("same failure, different errors:\n%v\n%v", first, second)
	}
	if strings.Contains(first.Error(), "urn:request") {
		t.Errorf("error still carries the request instance: %v", first)
	}
	if !errors.Is(first, ErrCustomerBillingNotReady) {
		t.Errorf("err = %v, want ErrCustomerBillingNotReady", first)
	}
	// The detail text is kept verbatim, unescaped, for substring matching.
	if !strings.Contains(first.Error(), "customer has no data for stripe app <x> & co") {
		t.Errorf("detail altered: %v", first)
	}
}

func TestClassify_NonProblemBodiesAreUnchanged(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}}
	for _, body := range []string{"plain text failure", `["a","b"]`, `{"detail":"no instance here"}`} {
		var perm *PermanentError
		if !errors.As(classify(resp, []byte(body), nil), &perm) {
			t.Fatalf("%q: want a PermanentError", body)
		}
		if perm.ResponseBody != body {
			t.Errorf("ResponseBody = %q, want %q", perm.ResponseBody, body)
		}
	}
}
