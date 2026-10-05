#!/bin/sh
# Publishes one or more usage CloudEvents directly onto
# billing.usage.<project>.valid — the exact subject SubmissionConsumer's
# durable JetStream pull consumer filters on (filterSubject =
# "billing.usage.*.valid" in internal/submission/submission.go). This
# bypasses billing's own Central Validation/Attribution consumer (not
# present in this e2e cluster) and exercises openmeter-provider's consumer
# in isolation, the same way it would see events forwarded by that pipeline.
#
# Runs from the chainsaw host machine, using `kubectl exec` into the
# nats-box pod (config/dependencies/nats-ci/nats-box.yaml) since neither the
# chainsaw host nor the submission-consumer pod itself carries a nats CLI.
#
# Usage: publish-usage.sh <project> <meter-type> <value> [value ...]
# Prints the sum of all published values to stdout (for use as a chainsaw
# script `outputs` value in the calling step).
set -eu

project="$1"
meter_type="$2"
shift 2

subject="billing.usage.${project}.valid"

nats_pod=$(kubectl get pod -n nats-system -l app.kubernetes.io/component=nats-box --no-headers -o name | head -1)
if [ -z "$nats_pod" ]; then
  echo "ERROR: no nats-box pod found in nats-system" >&2
  exit 1
fi

total=0
now="$(date -u '+%Y-%m-%dT%H:%M:%S.000Z')"
seq_no=0

for value in "$@"; do
  seq_no=$((seq_no + 1))
  # $$-based suffix (not $RANDOM, which dash/POSIX sh lacks) keeps ids
  # unique across re-runs without needing a real ULID generator here.
  id="e2e-submission-$$-${seq_no}-${value}"

  event=$(cat <<EOF
{"specversion":"1.0","id":"${id}","type":"${meter_type}","source":"/e2e/submission-test","subject":"projects/${project}","datacontenttype":"application/json","time":"${now}","data":{"value":"${value}"}}
EOF
)

  kubectl exec -n nats-system "$nats_pod" -- \
    nats pub --server nats://nats:4222 "$subject" "$event"

  echo "published event id=${id} value=${value} to ${subject}" >&2
  total=$((total + value))
done

# printf, NOT echo: chainsaw's ($stdout) capture keeps a trailing newline
# verbatim, and this value is compared numerically downstream.
printf '%s' "$total"
