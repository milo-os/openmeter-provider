#!/bin/bash
# The time-travel contract suite's helpers: the contract suite's, which this
# shares its fixtures with, plus the clock.
. ../../e2e/contract/lib.sh

TIMETRAVEL_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

clock() {
  bash "${TIMETRAVEL_DIR}/clock.sh" "$@"
}

# billing_date START MONTHS — START moved on by MONTHS, as billing moves a
# contract's billing date (schedule.py's billing_date).
billing_date() {
  python3 - "$1" "$2" <<PY
import sys
sys.path.insert(0, "${CONTRACT_DIR}")
from schedule import billing_date, fmt_time, parse_time
print(fmt_time(billing_date(parse_time(sys.argv[1]), int(sys.argv[2]))))
PY
}

# plus_seconds TIME SECONDS — TIME moved on by SECONDS.
plus_seconds() {
  python3 -c 'import sys; from datetime import datetime, timedelta; t = datetime.strptime(sys.argv[1], "%Y-%m-%dT%H:%M:%SZ") + timedelta(seconds=int(sys.argv[2])); print(t.strftime("%Y-%m-%dT%H:%M:%SZ"))' "$1" "$2"
}
