#!/bin/bash
# Moves the kind cluster (task dev:setup) onto clocks the time-travel suites
# can move:
#
#   1. OpenMeter, rebuilt with openmeter/patch.py, and every one of its
#      workloads (deployments and the subscription-sync, collect and advance
#      jobs) reading OPENMETER_CLOCK_OFFSET_FILE from the node's
#      /var/lib/openmeter-timetravel;
#   2. the provider, built with the timetravel tag, reading the same file
#      as OPENMETER_PROVIDER_CLOCK_OFFSET_FILE.
#
# The file holds how many seconds ahead of the real time they run (0 to
# start). The suites write it through the timetravel-clock pod (clock.sh).
# A cluster set up this way is for these suites only: once its clock has
# moved, the other suites' data is months in OpenMeter's past.
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$dir/../.." && pwd)
cluster=${TEST_INFRA_CLUSTER_NAME:-test-infra}
ns=openmeter-system
openmeter_image=openmeter-timetravel:local
provider_image=ghcr.io/milo-os/openmeter-provider:timetravel

if [ "${SKIP_OPENMETER_BUILD:-}" = "" ] || ! docker image inspect "$openmeter_image" >/dev/null 2>&1; then
  bash "$dir/openmeter/build.sh" "$openmeter_image"
fi
docker build --build-arg GO_BUILD_TAGS=timetravel -t "$provider_image" "$root"
kind load docker-image "$openmeter_image" "$provider_image" --name "$cluster"

echo "Starting the clock at the real time..."
kubectl apply -n "$ns" -f "$dir/clock-pod.yaml"
kubectl wait -n "$ns" --for=condition=ready pod/timetravel-clock --timeout=120s
bash "$dir/clock.sh" set 0

echo "Running OpenMeter on the time-travel build and clock..."
mount_patch() {
  local spec=$1
  cat <<JSON
[
  {"op": "add", "path": "${spec}/containers/0/env", "value": [{"name": "OPENMETER_CLOCK_OFFSET_FILE", "value": "/timetravel/offset"}]},
  {"op": "add", "path": "${spec}/containers/0/volumeMounts/-", "value": {"name": "timetravel", "mountPath": "/timetravel", "readOnly": true}},
  {"op": "add", "path": "${spec}/volumes/-", "value": {"name": "timetravel", "hostPath": {"path": "/var/lib/openmeter-timetravel", "type": "DirectoryOrCreate"}}}
]
JSON
}
patch=$(jq -nc \
  --arg repo "${openmeter_image%%:*}" --arg tag "${openmeter_image##*:}" \
  --arg deployments "$(mount_patch /spec/template/spec | jq -c .)" \
  --arg jobs "$(mount_patch /spec/jobTemplate/spec/template/spec | jq -c .)" '
  [ {op: "add", path: "/spec/values/image", value: {repository: $repo, tag: $tag, pullPolicy: "Never"}},
    {op: "add", path: "/spec/postRenderers", value: [{kustomize: {patches: [
      {target: {kind: "Deployment", name: "openmeter-(api|balance-worker|billing-worker|notification-service|sink-worker)"}, patch: $deployments},
      {target: {kind: "CronJob", name: "openmeter-(subscription-sync|billing-collect-invoices|billing-advance-invoices)"}, patch: $jobs}
    ]}}]} ]')
kubectl patch helmrelease openmeter -n "$ns" --type json -p "$patch"
flux reconcile helmrelease openmeter -n "$ns" --timeout 15m 2>/dev/null ||
  kubectl annotate helmrelease openmeter -n "$ns" --overwrite reconcile.fluxcd.io/requestedAt="$(date +%s)"
kubectl wait -n "$ns" --for=condition=ready helmrelease/openmeter --timeout=15m
for d in api balance-worker billing-worker notification-service sink-worker; do
  kubectl rollout status -n "$ns" "deployment/openmeter-${d}" --timeout=10m
done

echo "Running the provider on the time-travel build and clock..."
kubectl patch deployment openmeter-provider-manager -n "$ns" --type strategic -p "$(jq -nc --arg image "$provider_image" '
  {spec: {template: {spec: {
    containers: [{name: "manager", image: $image, imagePullPolicy: "Never",
      env: [{name: "OPENMETER_PROVIDER_CLOCK_OFFSET_FILE", value: "/timetravel/offset"}],
      volumeMounts: [{name: "timetravel", mountPath: "/timetravel", readOnly: true}]}],
    volumes: [{name: "timetravel", hostPath: {path: "/var/lib/openmeter-timetravel", type: "DirectoryOrCreate"}}]}}}}')"
kubectl rollout status -n "$ns" deployment/openmeter-provider-manager --timeout=5m
echo "Ready: OpenMeter and the provider run on the time-travel clock."
