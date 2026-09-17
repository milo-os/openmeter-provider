// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"net/http"
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
