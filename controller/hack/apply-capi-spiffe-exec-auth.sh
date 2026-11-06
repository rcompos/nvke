#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Inject the SPIRE exec-auth plugin into CAPI/CAPD controller Deployments.
# Applied after clusterctl init/upgrade. Missing Deployments are skipped so
# capi-core-up (no CAPD) and capd-up (may omit kubeadm CP until topology is on)
# can share this script.
#
# The overlay mounts csi.spiffe.io. On a fresh Kind cluster that CSI driver
# does not exist until Tilt installs SPIRE, so rollout wait is skipped until
# the driver is registered. Re-run after `tilt up` (or on capd-upgrade) to
# wait for Ready.
#
# Usage: ./hack/apply-capi-spiffe-exec-auth.sh [--kubeconfig PATH] [--providers]
#          [--deployment NAMESPACE/NAME[:CONTAINER]]...
#
# Without --kubeconfig, kubectl picks the cluster the way it always does:
# KUBECONFIG if set, otherwise ~/.kube/config. With it, every kubectl call
# is pinned to PATH. No positional arguments.
#
# By default only Cluster API core, the kubeadm providers and CAPD are
# patched. Every controller that reads a tenant's CAPI kubeconfig needs the
# plugin, so a site on another infrastructure provider adds it:
#   --providers   also patch every Deployment clusterctl installed, found by
#                 its cluster.x-k8s.io/provider label
#   --deployment  also patch this Deployment; CONTAINER is its manager
#                 container, by default the one named manager or its only one

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OVERLAY="${ROOT}/deploy/k8s/capi-spiffe-exec-auth"

usage() {
  echo "usage: $(basename "$0") [--kubeconfig PATH] [--providers] [--deployment NAMESPACE/NAME[:CONTAINER]]..." >&2
  exit 2
}

kubeconfig=""
providers=false
extra=()
while [[ $# -gt 0 ]]; do
  case "$1" in
  --providers)
    providers=true
    shift
    ;;
  --deployment)
    [[ $# -ge 2 && "$2" == */* ]] || usage
    extra+=("$2")
    shift 2
    ;;
  --deployment=*)
    [[ "${1#--deployment=}" == */* ]] || usage
    extra+=("${1#--deployment=}")
    shift
    ;;
  --kubeconfig)
    [[ $# -ge 2 ]] || usage
    kubeconfig="$2"
    [[ -n "${kubeconfig}" ]] || usage
    shift 2
    ;;
  --kubeconfig=*)
    kubeconfig="${1#--kubeconfig=}"
    [[ -n "${kubeconfig}" ]] || usage
    shift
    ;;
  -h | --help) usage ;;
  *) usage ;;
  esac
done

# Every kubectl call goes through here so one flag pins them all.
kubectl_cmd() {
  if [[ -n "${kubeconfig}" ]]; then
    kubectl --kubeconfig "${kubeconfig}" "$@"
  else
    kubectl "$@"
  fi
}

wait_for_csi=false
if kubectl_cmd get csidriver csi.spiffe.io >/dev/null 2>&1; then
  wait_for_csi=true
else
  echo "SPIRE CSI driver not registered yet; patched pods become Ready after tilt up"
fi

patch_if_present() {
  local ns="$1"
  local name="$2"
  local patch="$3"
  if ! kubectl_cmd get "deployment/${name}" -n "${ns}" >/dev/null 2>&1; then
    echo "skip ${ns}/${name} (not installed)"
    return 0
  fi
  echo "patching ${ns}/${name}"
  kubectl_cmd patch "deployment/${name}" -n "${ns}" --type strategic --patch-file "${patch}"
  if [[ "${wait_for_csi}" == "true" ]]; then
    kubectl_cmd rollout status "deployment/${name}" -n "${ns}" --timeout=180s
  fi
}

patch_if_present capi-system capi-controller-manager \
  "${OVERLAY}/capi-controller-manager.yaml"
patch_if_present capi-kubeadm-control-plane-system capi-kubeadm-control-plane-controller-manager \
  "${OVERLAY}/capi-kubeadm-control-plane-controller-manager.yaml"
patch_if_present capi-kubeadm-bootstrap-system capi-kubeadm-bootstrap-controller-manager \
  "${OVERLAY}/capi-kubeadm-bootstrap-controller-manager.yaml"
patch_if_present capd-system capd-controller-manager \
  "${OVERLAY}/capd-controller-manager.yaml"

known=" capi-system/capi-controller-manager capi-kubeadm-control-plane-system/capi-kubeadm-control-plane-controller-manager capi-kubeadm-bootstrap-system/capi-kubeadm-bootstrap-controller-manager capd-system/capd-controller-manager "

# The manager container: the one given, else the one named manager, else the
# Deployment's only container.
manager_container() {
  local ns="$1" name="$2" containers
  containers="$(kubectl_cmd get "deployment/${name}" -n "${ns}" \
    -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"\n"}{end}')"
  if grep -Fxq manager <<<"${containers}"; then
    echo manager
  elif [[ "$(grep -c . <<<"${containers}")" -eq 1 ]]; then
    echo "${containers}"
  else
    echo "${ns}/${name} has containers $(tr '\n' ' ' <<<"${containers}")and none named manager; name it with --deployment ${ns}/${name}:CONTAINER" >&2
    return 1
  fi
}

patch_provider() {
  local target="$1" ref ns name container patch
  ref="${target%%:*}"
  ns="${ref%%/*}"
  name="${ref#*/}"
  if [[ "${known}" == *" ${ref} "* ]]; then
    return 0
  fi
  if ! kubectl_cmd get "deployment/${name}" -n "${ns}" >/dev/null 2>&1; then
    echo "${ref}: no such Deployment" >&2
    return 1
  fi
  if [[ "${target}" == *:* ]]; then
    container="${target#*:}"
  else
    container="$(manager_container "${ns}" "${name}")"
  fi
  patch="$(mktemp)"
  sed "s/^        - name: CONTAINER$/        - name: ${container}/" \
    "${OVERLAY}/provider-controller-manager.yaml" >"${patch}"
  patch_if_present "${ns}" "${name}" "${patch}"
  rm -f "${patch}"
  known+="${ref} "
}

if [[ "${providers}" == "true" ]]; then
  found="$(kubectl_cmd get deployments -A -l cluster.x-k8s.io/provider \
    -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}')"
  for target in ${found}; do
    patch_provider "${target}"
  done
fi
for target in ${extra[@]+"${extra[@]}"}; do
  patch_provider "${target}"
done
