// SPDX-License-Identifier: AGPL-3.0-only

package controllermanager

import (
	"testing"

	"go.miloapis.com/instrumentation/instrumenttest"
)

func TestInstrumentationAdopted(t *testing.T) {
	instrumenttest.AssertAdopted(t)
}
