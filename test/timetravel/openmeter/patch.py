#!/usr/bin/env python3
"""Patches an OpenMeter source tree so a test build can run ahead of time.

Usage: patch.py SRC

  - pkg/clock reads OPENMETER_CLOCK_OFFSET_FILE, when set, every 200ms: a
    whole number of seconds this process's clock runs ahead of the real
    time. Every pod mounts the same file, so moving the file moves them all.
  - Billing and the billing apps read the time through pkg/clock instead of
    time.Now(), so all of billing moves with it. The rest of OpenMeter
    (ingest, Kafka, ClickHouse plumbing) keeps the real time.

Only for test/timetravel: nothing built from this is ever deployed.
"""
import pathlib
import re
import sys

src = pathlib.Path(sys.argv[1])
CLOCK_IMPORT = '"github.com/openmeterio/openmeter/pkg/clock"'

# 1. pkg/clock: an offset read from a file.
clock = src / "pkg/clock/clock.go"
s = clock.read_text()
s = s.replace('import (\n\t"sync/atomic"\n\t"time"\n)', '''import (
\t"os"
\t"strconv"
\t"strings"
\t"sync/atomic"
\t"time"
)''', 1)
assert '"os"' in s, "pkg/clock imports changed"
s = s.replace("func Now() time.Time {", '''// fileOffset is how far ahead of the real time the clock runs, in
// nanoseconds, as OPENMETER_CLOCK_OFFSET_FILE last said (test builds only).
var fileOffset int64

func init() {
\tpath := os.Getenv("OPENMETER_CLOCK_OFFSET_FILE")
\tif path == "" {
\t\treturn
\t}
\tread := func() {
\t\tb, err := os.ReadFile(path)
\t\tif err != nil {
\t\t\treturn
\t\t}
\t\tseconds, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
\t\tif err != nil {
\t\t\treturn
\t\t}
\t\tatomic.StoreInt64(&fileOffset, seconds*int64(time.Second))
\t}
\tread()
\tgo func() {
\t\tfor range time.Tick(200 * time.Millisecond) {
\t\t\tread()
\t\t}
\t}()
}

func Now() time.Time {''', 1)
old = "t := time.Now().Add(-driftDuration)"
assert old in s, "pkg/clock.Now changed"
s = s.replace(old, "t := time.Now().Add(-driftDuration).Add(time.Duration(atomic.LoadInt64(&fileOffset)))", 1)
clock.write_text(s)

# 2. Billing reads the time through pkg/clock.
changed = 0
for path in sorted(list((src / "openmeter/billing").rglob("*.go")) + list((src / "openmeter/app").rglob("*.go"))):
    if path.name.endswith("_test.go") or "testutils" in path.parts:
        continue
    s = path.read_text()
    if "time.Now()" not in s:
        continue
    s = s.replace("time.Now()", "clock.Now()")
    if CLOCK_IMPORT not in s:
        s = re.sub(r"^import \(\n", "import (\n\t" + CLOCK_IMPORT + "\n", s, count=1, flags=re.M)
        assert CLOCK_IMPORT in s, f"{path}: no import block"
    # Drop "time" if nothing else in the file uses it.
    body = re.sub(r"^import \((.|\n)*?^\)\n", "", s, count=1, flags=re.M)
    if not re.search(r"\btime\.", body):
        s = re.sub(r'^\t"time"\n', "", s, count=1, flags=re.M)
    path.write_text(s)
    changed += 1
print(f"patched pkg/clock and {changed} billing files")
