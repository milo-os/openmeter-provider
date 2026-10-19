// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// v3API calls OpenMeter's v3 API. Plans whose rate cards convert usage into
// their pricing unit (unit_config) can only be written and read there: the
// v1 API leaves them out of plan listings and refuses to read, publish,
// archive, change or migrate to them. There is no Go client for v3, so this
// is a thin JSON-over-HTTP helper using the generated v3 types.
type v3API struct {
	baseURL    string
	apiSecret  string
	httpClient *http.Client
}

func newV3API(server, apiSecret string, httpClient *http.Client) *v3API {
	return &v3API{
		baseURL:    strings.TrimRight(server, "/") + "/api/v3/openmeter",
		apiSecret:  apiSecret,
		httpClient: httpClient,
	}
}

// do sends in (when non-nil) as JSON and decodes a 2xx response into out
// (when non-nil). It returns the response status with the classified error,
// so callers can treat a 404 as they need.
func (a *v3API) do(ctx context.Context, method, path string, query url.Values, in, out any) (int, error) {
	u := a.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, &PermanentError{Err: fmt.Errorf("encode %s %s: %w", method, path, err)}
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, &PermanentError{Err: err}
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if a.apiSecret != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiSecret)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return 0, classify(nil, nil, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, &TransientError{Err: fmt.Errorf("read %s %s: %w", method, path, err)}
	}
	if err := classify(resp, raw, nil); err != nil {
		return resp.StatusCode, err
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, &TransientError{Err: fmt.Errorf("decode %s %s: %w", method, path, err)}
		}
	}
	return resp.StatusCode, nil
}
