#!/bin/bash
# Moves the clock to just after midnight on the next 31st, and prints that
# midnight: when the suite's contract starts. Starting on a 31st means its
# billing dates fall on the last day of every shorter month, the case most
# likely to go wrong.
set -euo pipefail
. ./lib.sh

start=$(python3 -c '
from datetime import datetime, timezone
now = datetime.now(timezone.utc)
year, month = now.year, now.month
while True:
    month += 1
    if month > 12:
        year, month = year + 1, 1
    if month in (1, 3, 5, 7, 8, 10, 12):
        print(datetime(year, month, 31, tzinfo=timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))
        break
')
clock to "$(plus_seconds "$start" 30)" >&2
echo "$start"
