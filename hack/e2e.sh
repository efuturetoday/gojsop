#!/usr/bin/env bash
# End-to-end demo for the configmap-sync JSHook on a local kind cluster.
#
# Steps:
#   1. ensure kind cluster `gojsop-e2e`
#   2. build + load the controller image
#   3. install CRDs and deploy the controller
#   4. apply the configmap-sync JSHook
#   5. create the source ConfigMap with the gojsop.io/sync-to annotation
#   6. wait until the synced copies appear in the target namespaces
#
# Re-run anytime; everything is idempotent.
set -euo pipefail

CLUSTER="${CLUSTER:-gojsop-e2e}"
IMG="${IMG:-ko.local/gojsop:dev}"
TARGET_NAMESPACES=(ns-a ns-b ns-c)
SOURCE_NS="default"
SOURCE_CM="demo-config"

cd "$(dirname "$0")/.."

if ! kind get clusters | grep -qx "${CLUSTER}"; then
  echo ">>> creating kind cluster ${CLUSTER}"
  kind create cluster --name "${CLUSTER}"
fi

echo ">>> building controller image ${IMG}"
make docker-build IMG="${IMG}"
kind load docker-image "${IMG}" --name "${CLUSTER}"

echo ">>> installing CRDs"
make install

echo ">>> deploying controller"
make deploy IMG="${IMG}"

echo ">>> ensuring target namespaces"
for ns in "${TARGET_NAMESPACES[@]}"; do
  kubectl get ns "${ns}" >/dev/null 2>&1 || kubectl create ns "${ns}"
done

echo ">>> applying configmap-sync JSHook"
kubectl apply -f config/samples/core_v1alpha1_jshook.yaml

echo ">>> creating source ConfigMap with sync annotation"
kubectl -n "${SOURCE_NS}" create configmap "${SOURCE_CM}" \
  --from-literal=greeting=hello \
  --from-literal=color=blue \
  --dry-run=client -o yaml | \
  kubectl annotate --local -f - "gojsop.io/sync-to=$(IFS=,; echo "${TARGET_NAMESPACES[*]}")" -o yaml | \
  kubectl apply -f -

echo ">>> waiting for synced ConfigMaps to appear"
deadline=$(( $(date +%s) + 60 ))
for ns in "${TARGET_NAMESPACES[@]}"; do
  while ! kubectl -n "${ns}" get configmap "${SOURCE_CM}" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "${deadline}" ] || { echo "timeout waiting for ${ns}/${SOURCE_CM}"; exit 1; }
    sleep 1
  done
  echo "  - ${ns}/${SOURCE_CM} present"
done

echo ">>> verifying data parity"
for ns in "${TARGET_NAMESPACES[@]}"; do
  diff <(kubectl -n "${SOURCE_NS}" get cm "${SOURCE_CM}" -o jsonpath='{.data}') \
       <(kubectl -n "${ns}"        get cm "${SOURCE_CM}" -o jsonpath='{.data}') \
    && echo "  - ${ns} data matches" \
    || { echo "  - ${ns} data MISMATCH"; exit 1; }
done

echo ">>> SUCCESS — configmap-sync hook is replicating ${SOURCE_NS}/${SOURCE_CM} to ${TARGET_NAMESPACES[*]}"
