#!/bin/sh
# Publishes exactly one usage CloudEvent onto billing.usage.<project>.valid
# with a CALLER-SUPPLIED id, unlike publish-usage.sh (which always generates
# a fresh, unique id per call). Exists so a caller can publish the same id
# twice — simulating a NATS redelivery of the same message (e.g. after a
# nack, a pod restart before ack, or an at-least-once delivery retry) — to
# verify OpenMeter's own documented (source, id) dedup actually protects
# against double-billing, not just that a single publish works once.
#
# Runs from the chainsaw host machine, using `kubectl exec` into the
# nats-box pod (config/dependencies/nats-ci/nats-box.yaml) since neither the
# chainsaw host nor the submission-consumer pod itself carries a nats CLI.
#
# Usage: publish-usage-with-id.sh <project> <meter-type> <id> <value>
set -eu

project="$1"
meter_type="$2"
id="$3"
value="$4"

subject="billing.usage.${project}.valid"

nats_pod=$(kubectl get pod -n nats-system -l app.kubernetes.io/component=nats-box --no-headers -o name | head -1)
if [ -z "$nats_pod" ]; then
  echo "ERROR: no nats-box pod found in nats-system" >&2
  exit 1
fi

now="$(date -u '+%Y-%m-%dT%H:%M:%S.000Z')"

event=$(cat <<EOF
{"specversion":"1.0","id":"${id}","type":"${meter_type}","source":"/e2e/submission-test","subject":"projects/${project}","datacontenttype":"application/json","time":"${now}","data":{"value":"${value}"}}
EOF
)

kubectl exec -n nats-system "$nats_pod" -- \
  nats pub --server nats://nats:4222 "$subject" "$event"

echo "published event id=${id} value=${value} to ${subject}" >&2
