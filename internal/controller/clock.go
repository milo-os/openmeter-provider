// SPDX-License-Identifier: AGPL-3.0-only

package controller

import "time"

// systemNow is the controllers' clock when a reconciler has no Now of its
// own: the real time, except in a build for test/timetravel, which runs it
// ahead with OpenMeter's (clock_timetravel.go).
var systemNow = time.Now
