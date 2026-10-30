// SPDX-License-Identifier: AGPL-3.0-only

package apiserver

import (
	"bytes"
	"strings"
	"testing"

	logsapi "k8s.io/component-base/logs/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"go.miloapis.com/instrumentation/logging"
)

// TestZapLevelFollowsVerbosity: -v sets which V(n) lines controller-runtime's
// logger writes, as it does for klog.
func TestZapLevelFollowsVerbosity(t *testing.T) {
	tests := []struct {
		verbosity logsapi.VerbosityLevel
		written   []string
		dropped   []string
	}{
		{verbosity: 0, written: []string{"v0"}, dropped: []string{"v1", "v2", "v3"}},
		{verbosity: 2, written: []string{"v0", "v1", "v2"}, dropped: []string{"v3"}},
		{verbosity: 300, written: []string{"v0", "v1", "v2", "v3"}},
	}
	for _, tt := range tests {
		var out bytes.Buffer
		logger := logging.New(&zap.Options{Level: zapLevel(tt.verbosity), DestWriter: &out})
		for v, msg := range []string{"v0", "v1", "v2", "v3"} {
			logger.V(v).Info(msg)
		}
		for _, msg := range tt.written {
			if !strings.Contains(out.String(), `"`+msg+`"`) {
				t.Errorf("-v=%d: %q not written; output:\n%s", tt.verbosity, msg, out.String())
			}
		}
		for _, msg := range tt.dropped {
			if strings.Contains(out.String(), `"`+msg+`"`) {
				t.Errorf("-v=%d: %q written; output:\n%s", tt.verbosity, msg, out.String())
			}
		}
	}
}
