#!/bin/sh
# Polls OpenMeter's meter query endpoint, scoped to the customer's OWN
# internal id (filterCustomerId) as well as the CloudEvent subject, and
# asserts both the summed value and the row's own customerId match what's
# expected — the end-to-end proof that events published straight onto NATS
# (publish-usage.sh) were picked up by SubmissionConsumer, forwarded via
# SubmitUsageBatch, and actually landed in OpenMeter attributed to that
# specific customer (not just present somewhere in the raw ingest stream).
#
# Uses filterCustomerId, NOT a manual groupBy=customer_id: confirmed by
# reading OpenMeter's own source (openmeter/streaming/query_params.go's
# QueryParams.Validate()) that groupBy=customer_id requires a non-empty
# customer filter ("customer filter is required with customer_id group
# by") — passing it manually without filterCustomerId is what produced a
# 400 here. filterCustomerId alone already auto-adds groupBy=customer_id
# server-side (see openmeter/meter/httphandler/mapping.go's
# toQueryParamsFromRequest), so nothing else is needed.
#
# Runs from the chainsaw host machine, reaching into the cluster via
# `kubectl exec` on the openmeter-query pod (the app pods are distroless and
# offer no shell/curl of their own).
#
# Usage: verify-usage.sh <meter-slug> <customer-id> <subject> <want-total> <from-rfc3339>
#
# <customer-id> is OpenMeter's internal ULID id (see resolve-customer-id.sh),
# not the external key — filterCustomerId only accepts the internal id.
#
# <from-rfc3339> scopes the query to events at/after that time, so a stale
# accumulated value from a previous run of this same test doesn't mask a
# regression (OpenMeter's usage query has no other notion of "this test
# run's events" to filter on).
set -eu

# Strip any stray CR/LF from every argument before it reaches the URL:
# these arrive from chainsaw script `outputs`, which keep whatever trailing
# newline the producing script emitted — and a newline inside a query
# string makes the HTTP request line malformed (OpenMeter answers 400).
# The producing scripts use printf for this reason; this is belt-and-braces.
strip() { printf '%s' "$1" | tr -d '\r\n'; }

slug=$(strip "$1")
customer_id=$(strip "$2")
subject=$(strip "$3")
want_total=$(strip "$4")
from=$(strip "$5")

# Percent-encode "/" (subject is "projects/<name>") and ":" (RFC 3339
# timestamps) so they're unambiguously query-param values, not path/reserved
# characters.
encoded_subject=$(echo "$subject" | sed 's#/#%2F#g')
encoded_from=$(echo "$from" | sed 's/:/%3A/g')
url="http://openmeter-api.openmeter-system.svc.cluster.local/api/v1/meters/${slug}/query?subject=${encoded_subject}&filterCustomerId=${customer_id}&from=${encoded_from}"

echo "querying: ${url}"

body=""
last_err=""

# OpenMeter's dev/CI install runs the full production pipeline (raw ingest
# -> Kafka -> sink-worker -> ClickHouse materialized view), not a synchronous
# write — a meter query only sees usage once that pipeline has flushed, which
# can lag noticeably under CI's shared, resource-constrained runners (the
# HelmRelease caps the whole OpenMeter stack at 500m CPU / 512Mi memory;
# see config/dependencies/openmeter/helmrelease.yaml). Confirmed live: a CI
# run's submission-consumer logged "successfully submitted batch of usage
# events to OpenMeter" immediately, but the meter query still returned
# {"data":[]} 60s later — the ingest succeeded, materialization just hadn't
# caught up yet. 90 attempts * 5s = 7.5 minutes gives that pipeline generous
# room without masking a genuine regression (a real bug fails fast: either a
# malformed query 400s immediately, or the value is simply wrong once data
# appears — both exit long before the retry budget is exhausted).
attempts=90
interval=5

for i in $(seq 1 "$attempts"); do
  err_file=$(mktemp)
  # `if` is one of the shell constructs POSIX exempts from `set -e`'s
  # exit-on-failure — needed since a non-2xx response makes wget exit
  # nonzero, and we want to inspect *why* rather than abort the script.
  if body=$(kubectl exec -n openmeter-system openmeter-query -- wget -q -O - "$url" 2>"$err_file"); then
    last_err=""
  else
    last_err=$(cat "$err_file")
  fi
  rm -f "$err_file"

  if [ -n "$last_err" ]; then
    echo "meter ${slug}: request failed (${last_err}) — waiting..."
    sleep "$interval"
    continue
  fi

  got_value=$(echo "$body" | grep -o '"value":[0-9.]*' | head -1 | cut -d: -f2)
  got_customer=$(echo "$body" | grep -o "\"customerId\":\"${customer_id}\"" || true)

  if [ -n "$got_value" ] && [ -n "$got_customer" ]; then
    if awk -v got="$got_value" -v want="$want_total" 'BEGIN { exit !(got == want) }'; then
      echo "meter ${slug} customer ${customer_id} converged: value=${got_value} (want ${want_total})"
      exit 0
    fi
    echo "meter ${slug} customer ${customer_id}: value=${got_value} so far, want ${want_total} — waiting..."
  else
    echo "meter ${slug} customer ${customer_id}: request OK, no matching row yet (body: ${body}) — waiting... (attempt ${i}/${attempts})"
  fi
  sleep "$interval"
done

echo "meter ${slug} customer ${customer_id} did not converge to value=${want_total}"
echo "last body: ${body}"
echo "last error (if any): ${last_err}"
exit 1
