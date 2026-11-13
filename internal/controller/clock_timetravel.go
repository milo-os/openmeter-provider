//go:build timetravel

// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// A build for test/timetravel reads how far ahead of the real time to run,
// in whole seconds, from OPENMETER_PROVIDER_CLOCK_OFFSET_FILE every 200ms:
// the same file OpenMeter's test build reads, so the two move together.
// Never built into a release.
func init() {
	path := os.Getenv("OPENMETER_PROVIDER_CLOCK_OFFSET_FILE")
	if path == "" {
		return
	}
	var offset atomic.Int64
	read := func() {
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		seconds, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			return
		}
		offset.Store(seconds * int64(time.Second))
	}
	read()
	go func() {
		for range time.Tick(200 * time.Millisecond) {
			read()
		}
	}()
	systemNow = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
}
