#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Asserts what the chart renders for supportedInfraProviders and the per-provider
# RBAC toggles, and that it refuses a provider list the controller would refuse
# at startup.

set -euo pipefail

CHART="${CHART:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/deploy/charts/controller"}"
[[ -d "${CHART}" ]] || { echo "chart not found at ${CHART}" >&2; exit 2; }

failures=0

render() { helm template cwe "${CHART}" "$@"; }

# setting <description> <config.yaml key> <value> <rendered>
# Asserts one key of the rendered nvke-controller-config, where the controller
# reads its settings. A list compares as its items joined with commas.
setting() {
  local got
  got="$(yq 'select(.kind == "ConfigMap" and .metadata.name == "nvke-controller-config") | .data["config.yaml"]' <<<"$4" |
    yq ".$2 | (select(tag == \"!!seq\") | join(\",\")) // .")"
  if [[ "${got}" == "$3" ]]; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected %s = %s, got %s)\n' "$1" "$2" "$3" "${got}" >&2
    failures=$((failures + 1))
  fi
}

wantfail() {
  local description="$1" pattern="$2"
  shift 2
  local output
  if output="$(render "$@" 2>&1)"; then
    printf '  FAIL  %s (render succeeded; expected it to fail)\n' "${description}" >&2
    failures=$((failures + 1))
  elif grep -qF "${pattern}" <<<"${output}"; then
    printf '  ok    %s\n' "${description}"
  else
    printf '  FAIL  %s (failed, but not with: %s)\n' "${description}" "${pattern}" >&2
    failures=$((failures + 1))
  fi
}

# check <description> <expected> <got>
check() {
  if [[ "$3" == "$2" ]]; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected %s, got %s)\n' "$1" "$2" "$3" >&2
    failures=$((failures + 1))
  fi
}

# clusterrole <description> <name suffix> <expected count> <rendered>
# Asserts how many ClusterRoles named <fullname>-<suffix> were rendered.
clusterrole() {
  local got
  got="$(yq "select(.kind == \"ClusterRole\" and .metadata.name == \"cwe-nvke-controller-$2\") | .metadata.name" <<<"$4" | grep -c . || true)"
  if [[ "${got}" == "$3" ]]; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected %s ClusterRole(s) -%s, got %s)\n' "$1" "$3" "$2" "${got}" >&2
    failures=$((failures + 1))
  fi
}

echo "supportedInfraProviders default:"
out="$(render)"
setting "defaults deployed controller to nico only" supportedInfraProviders "nico" "${out}"

echo "supportedInfraProviders opt-in:"
out="$(render --set-string 'supportedInfraProviders=nico\,infra')"
setting "renders both providers when requested" supportedInfraProviders "nico,infra" "${out}"
out="$(render --set-string 'supportedInfraProviders=nico\,infra\,launchLayer')"
setting "renders launchLayer appended to the overlay value" supportedInfraProviders "nico,infra,launchLayer" "${out}"
wantfail "rejects an unknown provider" \
  'supportedInfraProviders: "launchlayer" is not a supported provider' \
  --set-string 'supportedInfraProviders=nico\,launchlayer'
wantfail "rejects a repeated provider" \
  'supportedInfraProviders: "nico" is listed more than once' \
  --set-string 'supportedInfraProviders=nico\, nico'

echo "provider RBAC toggles:"
out="$(render --set capi.rbac.create=true)"
clusterrole "grants NICo RBAC by default" nico 1 "${out}"
clusterrole "grants LaunchLayer RBAC by default" launchlayer 1 "${out}"
out="$(render --set capi.rbac.create=true --set capi.rbac.nico=false --set controllers.nico.enabled=false)"
clusterrole "omits NICo RBAC when capi.rbac.nico is false" nico 0 "${out}"
out="$(render --set capi.rbac.create=true --set capi.rbac.launchLayer=false)"
clusterrole "omits LaunchLayer RBAC when capi.rbac.launchLayer is false" launchlayer 0 "${out}"

echo "NICo controllers follow the provider list:"
out="$(render --set capi.rbac.create=true)"
setting "on by default, which lists nico" nicoControllersEnabled true "${out}"
out="$(render --set capi.rbac.create=true --set-string 'supportedInfraProviders=infra\, nico')"
setting "on when nico is listed with another provider" nicoControllersEnabled true "${out}"
clusterrole "grants NICo RBAC when nico is listed with another provider" nico 1 "${out}"
out="$(render --set capi.rbac.create=true --set-string supportedInfraProviders=infra)"
setting "off when nico is not listed" nicoControllersEnabled false "${out}"
clusterrole "omits NICo RBAC when nico is not listed" nico 0 "${out}"
out="$(render --set capi.rbac.create=true --set-string supportedInfraProviders=infra --set controllers.nico.enabled=true)"
setting "on when set, even with nico not listed" nicoControllersEnabled true "${out}"
clusterrole "grants NICo RBAC when the controllers are set on" nico 1 "${out}"
out="$(render --set capi.rbac.create=true --set controllers.nico.enabled=false)"
setting "off when set, even with nico listed" nicoControllersEnabled false "${out}"
clusterrole "keeps NICo RBAC for the listed provider with the controllers set off" nico 1 "${out}"
out="$(render --set capi.rbac.create=true --set-string supportedInfraProviders=infra --set controllers.nico.enabled=false)"
clusterrole "omits NICo RBAC with nico not listed and the controllers set off" nico 0 "${out}"
out="$(render --set capi.rbac.create=true --set-string supportedInfraProviders=infra --set capi.rbac.nico=true)"
clusterrole "grants NICo RBAC when capi.rbac.nico is set, whatever the controllers" nico 1 "${out}"

echo "a user's infrastructure provider:"
out="$(render)"
clusterrole "renders no infrastructure ClusterRole without capi.rbac.create" infrastructure 0 "${out}"
out="$(render --set capi.rbac.create=true)"
clusterrole "renders the aggregated infrastructure ClusterRole" infrastructure 1 "${out}"
clusterrole "renders no resources ClusterRole for an empty list" infrastructure-resources 0 "${out}"
selector="$(yq 'select(.kind == "ClusterRole" and .metadata.name == "cwe-nvke-controller-infrastructure") | .aggregationRule.clusterRoleSelectors[0].matchLabels | to_entries | .[0].key + "=" + .[0].value' <<<"${out}")"
check "aggregates ClusterRoles labelled for this release" "nvke.nvidia.com/aggregate-to-cwe-nvke-controller=true" "${selector}"
bound="$(yq 'select(.kind == "ClusterRoleBinding" and .metadata.name == "cwe-nvke-controller-infrastructure") | .roleRef.name + " " + .subjects[0].name' <<<"${out}")"
check "binds the infrastructure ClusterRole to the controller" "cwe-nvke-controller-infrastructure cloud-workflow-engine" "${bound}"
out="$(render --set capi.rbac.create=true --set 'capi.rbac.infrastructureResources={dockerclusters,dockermachines}')"
role='select(.kind == "ClusterRole" and .metadata.name == "cwe-nvke-controller-infrastructure-resources")'
check "labels the resources ClusterRole for aggregation" "true" \
  "$(yq "${role} | .metadata.labels[\"nvke.nvidia.com/aggregate-to-cwe-nvke-controller\"]" <<<"${out}")"
check "grants the listed resources" "dockerclusters,dockermachines" \
  "$(yq "${role} | .rules[0].resources | join(\",\")" <<<"${out}")"
check "grants get on the listed resources' CRDs" \
  "dockerclusters.infrastructure.cluster.x-k8s.io,dockermachines.infrastructure.cluster.x-k8s.io" \
  "$(yq "${role} | .rules[1].resourceNames | join(\",\")" <<<"${out}")"
wantfail "rejects a resource that is not a plural name" \
  'capi.rbac.infrastructureResources: "dockermachines.infrastructure.cluster.x-k8s.io" is not a plural resource name' \
  --set capi.rbac.create=true --set 'capi.rbac.infrastructureResources={dockermachines.infrastructure.cluster.x-k8s.io}'
wantfail "rejects a repeated resource" \
  'capi.rbac.infrastructureResources: "dockermachines" is listed more than once' \
  --set capi.rbac.create=true --set 'capi.rbac.infrastructureResources={dockermachines,dockermachines}'

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} chart provider assertion(s) failed" >&2
  exit 1
fi

echo "all chart provider assertions passed"
