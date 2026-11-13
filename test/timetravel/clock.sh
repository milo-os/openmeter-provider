#!/bin/bash
# clock.sh set SECONDS   run OpenMeter and the provider SECONDS ahead
# clock.sh to TIME       run them at TIME (RFC 3339, UTC) from now on
# clock.sh now           print the time they run at
#
# Both read the offset every 200ms; this waits a second after writing it so
# they have.
set -euo pipefail
ns=openmeter-system

write() {
  kubectl exec -n "$ns" timetravel-clock -- sh -c "echo $1 > /timetravel/offset.tmp && mv /timetravel/offset.tmp /timetravel/offset"
  sleep 1
}

epoch() {
  python3 -c 'import sys; from datetime import datetime, timezone; print(int(datetime.strptime(sys.argv[1], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp()))' "$1"
}

case "$1" in
  set) write "$2" ;;
  to) write "$(($(epoch "$2") - $(date -u +%s)))" ;;
  now)
    offset=$(kubectl exec -n "$ns" timetravel-clock -- cat /timetravel/offset)
    python3 -c 'import sys, time; from datetime import datetime, timezone; print(datetime.fromtimestamp(time.time() + int(sys.argv[1]), timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))' "$offset"
    ;;
  *) echo "usage: clock.sh set SECONDS | to TIME | now" >&2; exit 2 ;;
esac
