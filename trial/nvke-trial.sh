#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Trial NVKE on one machine: a kind management cluster, Cluster API with the
# Docker infrastructure provider (CAPD), the NVKE site stack built from this
# checkout, and one tenant cluster created through the generic infra provider.
# Every NVKE image and binary is built here; nothing is pulled from an NVKE
# registry.
#
# usage: trial/nvke-trial.sh [up|validate|status|kubeconfig|nvkectl ARGS|down]
#
#   up          build, deploy, create the tenant, write its kubeconfig, then
#               validate the site
#   validate    run the nvke-validate site and tenant checks; results in
#               .state/validate
#   status      show the management plane, the tenant and its containers;
#               restarts the NVKE API port-forward if it stopped
#   kubeconfig  write the tenant kubeconfig again
#   nvkectl     run nvkectl against the trial's site, e.g. nvkectl get clusters
#   down        delete the tenant, then the management cluster
#
# Every step of `up` skips what already exists, so a failed run can be re-run.
# Settings (environment):
#   KIND_CLUSTER_NAME  management cluster name (default nvke-trial)
#   TRIAL_API_PORT     local port for the NVKE API (default 18080)
#   TRIAL_WAIT_MINUTES how long to wait for the tenant (default 30)
#   TRIAL_REBUILD      1 rebuilds the images and nvkectl even if they exist
#   TRIAL_SKIP_DISK_CHECK 1 skips the free-disk check
#   TRIAL_SKIP_VALIDATE 1 ends `up` without validating
#   TRIAL_SKIP_TENANT_VALIDATE 1 validates the site alone
#
# The CNI is Calico: Docker Desktop's kernel has no openvswitch module, so the
# default OVN-Kubernetes cannot run on its nodes.

set -Eeuo pipefail

TRIAL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${TRIAL_DIR}/.." && pwd)"
STATE_DIR="${TRIAL_DIR}/.state"

export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-nvke-trial}"
API_PORT="${TRIAL_API_PORT:-18080}"
WAIT_MINUTES="${TRIAL_WAIT_MINUTES:-30}"
REBUILD="${TRIAL_REBUILD:-0}"

CONTROLLER_IMAGE="nvke-controller:trial"
NODE_IMAGE="cwe-capd-kindest-node:local"
VALIDATE_IMAGE="nvke-validate:trial"
VALIDATE_DIR="${STATE_DIR}/validate"
# Every namespace the trial installs into that must run a ready pod. The
# suite's default list is the NICo deploy path's.
SITE_NAMESPACES="kube-system,cert-manager,capi-system,capi-kubeadm-bootstrap-system,capi-kubeadm-control-plane-system,capd-system,nvke-controller,postgres,spire-server,temporal"
# The tenant's, with Calico.
TENANT_NAMESPACES="kube-system,calico-system,calico-apiserver,tigera-operator"
TENANT_VALIDATE_RBAC="${TRIAL_DIR}/validate-tenant-rbac.yaml"
NVKECTL="${REPO_ROOT}/cli/bin/nvkectl"
CLUSTERCTL="${REPO_ROOT}/controller/.local/bin/clusterctl"
TENANT_SPEC="${TRIAL_DIR}/tenant.yaml"
TENANT_KUBECONFIG="${STATE_DIR}/tenant.kubeconfig"
PORT_FORWARD_PID="${STATE_DIR}/port-forward.pid"
PORT_FORWARD_LOG="${STATE_DIR}/port-forward.log"
CAPI_CLUSTER_FILE="${STATE_DIR}/capi-cluster-name"
SITE_NAME="trial"

export KUBECONFIG="${STATE_DIR}/mgmt.kubeconfig"
# nvkectl keeps its site list under XDG_CONFIG_HOME; keep the trial's out of
# the user's own ~/.config/nvkectl.
NVKECTL_CONFIG_HOME="${STATE_DIR}/config"

log() { printf '\n[nvke-trial] %s\n' "$*" >&2; }
die() { printf '[nvke-trial] error: %s\n' "$*" >&2; exit 1; }

nvkectl() { XDG_CONFIG_HOME="${NVKECTL_CONFIG_HOME}" "${NVKECTL}" --site "${SITE_NAME}" "$@"; }
gmk() { "${MAKE_CMD}" -C "${REPO_ROOT}/$1" "${@:2}"; }

tenant_name() {
  sed -n 's/^name:[[:space:]]*//p' "${TENANT_SPEC}" | head -n1 | tr -d '"'"'"
}

# The Secret nvke-validate reads the tenant credential from.
tenant_secret() { printf 'nvke-validate-%s\n' "$(tenant_name)"; }

# ---------------------------------------------------------------- preflight

# The images, the management cluster and the tenant nodes share the Docker
# daemon's disk, and a kubelet evicts every pod once less than 10% of it is
# free. Ask for that margin plus room for the builds.
check_disk() {
  [[ "${TRIAL_SKIP_DISK_CHECK:-0}" == 1 ]] && return
  local size_kb free_kb need_kb
  read -r size_kb free_kb < <(docker run --rm busybox:1.37 df -kP / | awk 'NR == 2 {print $2, $4}')
  [[ -n "${size_kb}" && -n "${free_kb}" ]] || die "could not read the Docker disk's free space"
  need_kb=$((size_kb / 10 + 15 * 1024 * 1024))
  if ((free_kb < need_kb)); then
    die "the Docker disk has $((free_kb / 1024 / 1024)) GiB free of $((size_kb / 1024 / 1024)) GiB; the trial needs $((need_kb / 1024 / 1024)) GiB. Free space (docker builder prune) or raise the disk limit (Docker Desktop: Settings > Resources). TRIAL_SKIP_DISK_CHECK=1 skips this check"
  fi
}

preflight() {
  local cmd
  for cmd in docker kind kubectl helm go jq python3 curl; do
    command -v "${cmd}" >/dev/null 2>&1 || die "${cmd} is required"
  done
  # The Makefiles need GNU Make 4; macOS ships 3.81 as make.
  if command -v gmake >/dev/null 2>&1; then
    MAKE_CMD=gmake
  elif make --version 2>/dev/null | head -n1 | grep -Eq 'GNU Make [4-9]'; then
    MAKE_CMD="make"
  else
    die "GNU Make 4 or later is required (on macOS: brew install make, which installs gmake)"
  fi

  docker info >/dev/null 2>&1 || die "Docker is not reachable; start Docker Desktop or your Docker daemon"

  # On macOS the Makefiles send Docker commands to COLIMA_DOCKER_HOST. Point it
  # at whatever daemon the current Docker context uses.
  if [[ -z "${COLIMA_DOCKER_HOST:-}" ]]; then
    COLIMA_DOCKER_HOST="$(docker context inspect -f '{{.Endpoints.docker.Host}}')"
  fi
  export COLIMA_DOCKER_HOST
  export DOCKER_HOST="${DOCKER_HOST:-${COLIMA_DOCKER_HOST}}"

  local arch
  arch="$(docker version --format '{{.Server.Arch}}')"
  case "${arch}" in
    arm64 | aarch64) DOCKER_ARCH=arm64 ;;
    amd64 | x86_64) DOCKER_ARCH=amd64 ;;
    *) die "the trial runs on arm64 or amd64; this Docker daemon is ${arch}" ;;
  esac

  check_disk

  [[ -f "${TENANT_SPEC}" ]] || die "missing ${TENANT_SPEC}"
  mkdir -p "${STATE_DIR}" "${NVKECTL_CONFIG_HOME}"
}

# ------------------------------------------------------------ management plane

management_cluster() {
  log "management cluster: kind ${KIND_CLUSTER_NAME}"
  if kind get clusters 2>/dev/null | grep -Fx "${KIND_CLUSTER_NAME}" >/dev/null && ! management_running; then
    die "the management cluster ${KIND_CLUSTER_NAME} exists but is not running, as an interrupted run leaves it; run trial/nvke-trial.sh down, then up"
  fi
  gmk controller kind-up
  kind get kubeconfig --name "${KIND_CLUSTER_NAME}" >"${KUBECONFIG}"
  chmod 0600 "${KUBECONFIG}"
}

cluster_api() {
  if kubectl get deployment -n capd-system capd-controller-manager >/dev/null 2>&1; then
    log "Cluster API and CAPD: already installed"
    return
  fi
  local version
  version="$(gmk controller -s --no-print-directory print-CAPI_CORE_VERSION)"
  log "Cluster API and CAPD: installing ${version}"
  gmk controller clusterctl-install
  CLUSTER_TOPOLOGY=true "${CLUSTERCTL}" init \
    --core "cluster-api:${version}" \
    --bootstrap "kubeadm:${version}" \
    --control-plane "kubeadm:${version}" \
    --infrastructure "docker:${version}" \
    --wait-providers
}

image_exists() { docker image inspect "$1" >/dev/null 2>&1; }

# The node image bakes in a Calico manifests package. Building it needs
# dpkg-deb and sha256sum, which macOS lacks, so build it in a Debian container
# when they are missing. capd-image then finds the package and keeps it.
calico_package() {
  local deb="${REPO_ROOT}/controller/.local/cni/nvke-cni-calico-manifests-3.30.1_3.30.1_all.deb"
  if [[ -f "${deb}" && "${REBUILD}" != 1 ]]; then
    return
  fi
  if command -v dpkg-deb >/dev/null 2>&1 && command -v sha256sum >/dev/null 2>&1; then
    return
  fi
  log "building the Calico manifests package in a Debian container (no dpkg-deb on this host)"
  mkdir -p "${REPO_ROOT}/controller/.local/cni"
  docker run --rm --user "$(id -u):$(id -g)" \
    -v "${REPO_ROOT}/controller:/src" -w /src \
    buildpack-deps:bookworm-curl \
    bash hack/ci/prepare-calico-assets.sh /src/.local/cni
  [[ -f "${deb}" ]] || die "the Calico manifests package was not built: ${deb}"
}

build() {
  calico_package
  if [[ "${REBUILD}" == 1 ]] || ! image_exists "${NODE_IMAGE}"; then
    log "building the tenant node image ${NODE_IMAGE} (nvke-agent and Calico)"
    CNI_PLUGIN=calico INSTALL_CNI_AFTER_INIT=true CAPD_SPIRE_ATTESTATION_METHOD=joinToken \
      gmk controller capd-image
  else
    log "tenant node image ${NODE_IMAGE}: already built"
  fi

  if [[ "${REBUILD}" == 1 ]] || ! image_exists "${CONTROLLER_IMAGE}"; then
    log "building the controller image ${CONTROLLER_IMAGE}"
    # The controller Makefile takes GIT_VERSION from GitLab's CI_COMMIT_SHA
    # whenever CI is set, which leaves it empty under GitHub Actions.
    gmk controller docker-build-local LOCAL_PLATFORM="linux/${DOCKER_ARCH}" IMG="${CONTROLLER_IMAGE}" \
      GIT_VERSION="$(git -C "${REPO_ROOT}" rev-parse HEAD)"
  else
    log "controller image ${CONTROLLER_IMAGE}: already built"
  fi
  log "loading ${CONTROLLER_IMAGE} into kind"
  kind load docker-image "${CONTROLLER_IMAGE}" --name "${KIND_CLUSTER_NAME}"

  if [[ "${REBUILD}" == 1 ]] || [[ ! -x "${NVKECTL}" ]]; then
    log "building nvkectl"
    gmk cli build
  fi
}

deploy_stack() {
  local vip
  vip="$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' \
    "${KIND_CLUSTER_NAME}-control-plane")"
  [[ -n "${vip}" ]] || die "could not read the kind control-plane address"
  log "deploying the site stack from controller/deploy/capi-trial (management address ${vip})"
  MANAGEMENT_VIP="${vip}" \
  STACK_DIR="${REPO_ROOT}" \
  DEPLOY_TARGET=capi-trial \
  NODE_AGENT_SPIRE_ATTESTATION_METHOD=joinToken \
  NVKE_CONTROLLER_RELEASES_DIR="${TRIAL_DIR}/releases/${DOCKER_ARCH}" \
    bash "${REPO_ROOT}/controller/hack/ci/deploy-stack.sh" "${CONTROLLER_IMAGE}"

  # Tenant kubeconfigs authenticate with the SPIRE exec plugin, so every
  # controller that reads them has to carry it.
  log "patching the Cluster API and CAPD controllers with the SPIRE exec-auth plugin"
  gmk controller capi-spiffe-exec-auth
  kubectl -n nvke-controller rollout status deployment/nvke-controller --timeout=10m
}

# ------------------------------------------------------------------- NVKE API

api_up() { curl -fsS -o /dev/null "http://127.0.0.1:${API_PORT}/api/version" 2>/dev/null; }

port_forward() {
  if api_up; then
    return
  fi
  stop_port_forward
  log "forwarding the NVKE API to http://127.0.0.1:${API_PORT}"
  nohup kubectl -n nvke-controller port-forward svc/nvke-controller "${API_PORT}:8080" \
    >"${PORT_FORWARD_LOG}" 2>&1 &
  echo $! >"${PORT_FORWARD_PID}"
  local _
  for _ in $(seq 1 30); do
    api_up && return
    sleep 1
  done
  die "the NVKE API did not answer on port ${API_PORT}; see ${PORT_FORWARD_LOG}"
}

stop_port_forward() {
  if [[ -f "${PORT_FORWARD_PID}" ]]; then
    kill "$(cat "${PORT_FORWARD_PID}")" 2>/dev/null || true
    rm -f "${PORT_FORWARD_PID}"
  fi
}

register_site() {
  nvkectl config set-site --name "${SITE_NAME}" --url "http://127.0.0.1:${API_PORT}" \
    --kubeconfig "${KUBECONFIG}" >/dev/null
}

# --------------------------------------------------------------------- tenant

tenant_json() { nvkectl get clusters "$(tenant_name)" -o json 2>/dev/null; }

tenant_condition() {
  tenant_json | jq -r --arg c "$1" \
    '(if type == "array" then .[0] else . end) | .conditions[$c].status // "Unknown"' 2>/dev/null \
    || echo Unknown
}

create_tenant() {
  local name
  name="$(tenant_name)"
  if tenant_json >/dev/null; then
    log "tenant ${name}: already exists"
  else
    log "creating tenant ${name} from ${TENANT_SPEC}"
    nvkectl create cluster -f "${TENANT_SPEC}"
  fi

  log "waiting up to ${WAIT_MINUTES} minutes for ${name} to be provisioned"
  local deadline=$(($(date +%s) + WAIT_MINUTES * 60)) last=""
  while :; do
    local state
    state="$(tenant_condition provisioned)"
    [[ "${state}" == True ]] && break
    if [[ "$(date +%s)" -ge "${deadline}" ]]; then
      tenant_json | jq '(if type == "array" then .[0] else . end) | .conditions' >&2 || true
      die "${name} was not provisioned in ${WAIT_MINUTES} minutes; run '$0 status'"
    fi
    local machines
    machines="$(kubectl get machines.cluster.x-k8s.io -A --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    if [[ "${state}/${machines}" != "${last}" ]]; then
      log "provisioned=${state}, CAPI machines=${machines}"
      last="${state}/${machines}"
    fi
    sleep 15
  done
  log "tenant ${name} is provisioned"
}

# The CAPI Cluster the controller created for the tenant: the only one, or
# the one whose name carries the tenant's.
capi_cluster_name() {
  local name all
  name="$(tenant_name)"
  all="$(kubectl get clusters.cluster.x-k8s.io -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')"
  if [[ "$(printf '%s\n' "${all}" | grep -c .)" -eq 1 ]]; then
    printf '%s\n' "${all}"
  else
    printf '%s\n' "${all}" | grep -F "${name}" | head -n1
  fi
}

tenant_reachable() {
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" --request-timeout=10s get nodes >/dev/null 2>&1
}

tenant_kubeconfig() {
  local name capi
  name="$(tenant_name)"
  capi="$(capi_cluster_name)"
  [[ -n "${capi}" ]] && printf '%s\n' "${capi}" >"${CAPI_CLUSTER_FILE}"

  nvkectl get kubeconfig "${name}" --output-file "${TENANT_KUBECONFIG}"
  chmod 0600 "${TENANT_KUBECONFIG}"
  if ! tenant_reachable; then
    # Docker Desktop does not route container addresses to the host, so point
    # the kubeconfig at the load balancer's published 127.0.0.1 port instead.
    [[ -n "${capi}" ]] || die "could not find the tenant's CAPI Cluster to rewrite its kubeconfig"
    log "the kubeconfig's server is not reachable from this host; using the published load balancer port"
    (cd "${REPO_ROOT}/controller" && ./hack/fetch-capd-kubeconfig.sh "${capi}" "${TENANT_KUBECONFIG}")
    chmod 0600 "${TENANT_KUBECONFIG}"
    tenant_reachable || die "the tenant API is not reachable with ${TENANT_KUBECONFIG}"
  fi

  # `kubectl wait --all` only covers nodes that have registered, so first wait
  # for one node per CAPI machine.
  local want have deadline=$(($(date +%s) + 600))
  want="$(kubectl get machines.cluster.x-k8s.io -A --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  log "waiting for the tenant's ${want} nodes to register and be Ready"
  while :; do
    have="$(kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    ((have >= want)) && break
    [[ "$(date +%s)" -ge "${deadline}" ]] && die "only ${have} of ${want} tenant nodes registered in 10 minutes"
    sleep 10
  done
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" wait --for=condition=Ready nodes --all --timeout=10m
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes -o wide
}

# ------------------------------------------------------------------- validate

# The suite reaches the tenant as a ServiceAccount, never as its admin, and
# reads that credential from a Secret in its own namespace on the site, which
# outside a pod is default. A CAPD tenant has no control-plane VIP, so the
# Secret names the server: a control-plane node on kind's network, where the
# validation container runs. Not CAPD's load balancer, whose address the API
# server's certificate does not carry.
tenant_credential() {
  local capi node server dir
  capi="$(capi_cluster_name)"
  [[ -n "${capi}" ]] || die "could not find the tenant's CAPI Cluster"
  node="$(docker ps --filter "label=io.x-k8s.kind.cluster=${capi}" \
    --filter label=io.x-k8s.kind.role=control-plane --format '{{.Names}}' | head -n1)"
  [[ -n "${node}" ]] || die "found no control-plane container for ${capi}"
  server="$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' "${node}")"
  [[ -n "${server}" ]] || die "could not read the address of ${node}"

  kubectl --kubeconfig "${TENANT_KUBECONFIG}" apply -f "${TENANT_VALIDATE_RBAC}" >/dev/null
  dir="${VALIDATE_DIR}/credential"
  mkdir -m 0700 "${dir}"
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" -n nvke-validate create token nvke-validate \
    --duration=2h >"${dir}/token"
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" config view --raw --minify \
    -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 --decode >"${dir}/ca.crt"
  printf 'https://%s:6443' "${server}" >"${dir}/server"
  kubectl -n default create secret generic "$(tenant_secret)" \
    --from-file="${dir}/token" --from-file="${dir}/ca.crt" --from-file="${dir}/server" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  rm -rf "${dir}"
}

# Runs the validation image from a container on kind's Docker network, as the
# management cluster's admin: the site checks, and the tenant's unless
# TRIAL_SKIP_TENANT_VALIDATE=1. The expected versions come from what the site
# was built from, not from the site: the controller's from the image the trial
# deployed, Kubernetes' from the kind node image. The tenant's come from the
# controller's record of it.
validate() {
  if [[ "${REBUILD}" == 1 ]] || ! image_exists "${VALIDATE_IMAGE}"; then
    log "building the validation image ${VALIDATE_IMAGE}"
    gmk controller/test/e2e validate-image VALIDATE_IMAGE="${VALIDATE_IMAGE}" \
      VALIDATE_PLATFORM="linux/${DOCKER_ARCH}"
  fi

  local controller_version kubernetes_version node_image
  controller_version="$(docker run --rm "${CONTROLLER_IMAGE}" controller --version | awk '{print $2}')"
  [[ -n "${controller_version}" ]] || die "could not read the version of ${CONTROLLER_IMAGE}"
  node_image="$(docker inspect -f '{{.Config.Image}}' "${KIND_CLUSTER_NAME}-control-plane")"
  kubernetes_version="${node_image%%@*}"
  kubernetes_version="${kubernetes_version##*:}"
  [[ "${kubernetes_version}" == v* ]] || die "could not read the Kubernetes version from the kind node image ${node_image}"

  rm -rf "${VALIDATE_DIR}"
  mkdir -p "${VALIDATE_DIR}/artifacts"

  local what="the site" tenant_env=(-e NVKE_TENANT_VALIDATE=false)
  if [[ "${TRIAL_SKIP_TENANT_VALIDATE:-0}" != 1 ]]; then
    tenant_credential
    what="the site and tenant $(tenant_name)"
    tenant_env=(
      -e NVKE_TENANT_VALIDATE=true
      -e NVKE_TENANT_CLUSTER="$(tenant_name)"
      -e NVKE_TENANT_NAMESPACES="${TENANT_NAMESPACES}"
    )
  fi

  kind get kubeconfig --internal --name "${KIND_CLUSTER_NAME}" >"${VALIDATE_DIR}/kubeconfig"
  chmod 0600 "${VALIDATE_DIR}/kubeconfig"

  log "validating ${what}: controller ${controller_version}, Kubernetes ${kubernetes_version}"
  local name="nvke-validate-${KIND_CLUSTER_NAME}" rc=0
  docker rm -f "${name}" >/dev/null 2>&1 || true
  docker run --rm --name "${name}" --network kind --user "$(id -u):$(id -g)" \
    -v "${VALIDATE_DIR}/artifacts:/artifacts" \
    -v "${VALIDATE_DIR}/kubeconfig:/kube/config:ro" \
    -e KUBECONFIG=/kube/config \
    -e NVKE_CONTROLLER_EXPECTED_VERSION="${controller_version}" \
    -e NVKE_SITE_KUBERNETES_VERSION="${kubernetes_version}" \
    -e NVKE_SITE_NAMESPACES="${SITE_NAMESPACES}" \
    "${tenant_env[@]}" \
    "${VALIDATE_IMAGE}" >"${VALIDATE_DIR}/validate.log" 2>&1 || rc=$?
  rm -f "${VALIDATE_DIR}/kubeconfig"
  kubectl -n default delete secret "$(tenant_secret)" --ignore-not-found >/dev/null || true

  grep -E '^(Ran [0-9]+ of|SUCCESS!|FAIL!)' "${VALIDATE_DIR}/validate.log" >&2 || true
  if ((rc != 0)); then
    grep -E '^\s*\[FAIL\]' "${VALIDATE_DIR}/validate.log" >&2 || true
    die "validation failed; see ${VALIDATE_DIR}/validate.log and ${VALIDATE_DIR}/artifacts"
  fi
  log "validation passed; results in ${VALIDATE_DIR}"
}

# ------------------------------------------------------------------- commands

cmd_up() {
  preflight
  management_cluster
  cluster_api
  build
  deploy_stack
  port_forward
  register_site
  create_tenant
  tenant_kubeconfig
  if [[ "${TRIAL_SKIP_VALIDATE:-0}" != 1 ]]; then
    validate
  fi
  cat >&2 <<EOF

[nvke-trial] done. The tenant is running:

  export KUBECONFIG=${TENANT_KUBECONFIG}
  kubectl get nodes

  The NVKE API is at http://127.0.0.1:${API_PORT} while the port-forward runs;
  $0 status restarts it. Run nvkectl against it with:
  $0 nvkectl get clusters

  Remove everything with: $0 down
EOF
}

cmd_status() {
  preflight
  if ! kind get clusters 2>/dev/null | grep -Fx "${KIND_CLUSTER_NAME}" >/dev/null; then
    log "no management cluster ${KIND_CLUSTER_NAME}"
    return
  fi
  if ! management_running; then
    log "the management cluster ${KIND_CLUSTER_NAME} exists but is not running; run $0 down, then up"
    return
  fi
  log "management plane pods that are not Running or Completed"
  kubectl get pods -A --no-headers | awk '$4 != "Running" && $4 != "Completed"' || true
  log "Cluster API"
  kubectl get clusters.cluster.x-k8s.io,machines.cluster.x-k8s.io -A 2>/dev/null || true
  if [[ -x "${NVKECTL}" ]]; then
    port_forward
    register_site
    log "NVKE API at http://127.0.0.1:${API_PORT}; tenants"
    nvkectl get clusters || true
  fi
  local capi
  capi="$(capi_cluster_name 2>/dev/null || true)"
  if [[ -n "${capi}" ]]; then
    log "tenant containers"
    docker ps -a --filter "label=io.x-k8s.kind.cluster=${capi}" --format '{{.Names}}\t{{.Status}}'
    tenant_health "${capi}"
  fi
}

# Says what is wrong with the tenant when its API does not answer. CAPD
# tenants do not survive a Docker restart: the node containers come back on
# new addresses, while the API server, its certificate and etcd keep the old.
tenant_health() {
  local capi="$1" node advertised current stopped
  if [[ -f "${TENANT_KUBECONFIG}" ]] && tenant_reachable; then
    log "tenant API: reachable with ${TENANT_KUBECONFIG}"
    return 0
  fi
  stopped="$(docker ps -a --filter "label=io.x-k8s.kind.cluster=${capi}" --filter status=exited --format '{{.Names}}')"
  if [[ -n "${stopped}" ]]; then
    log "tenant containers stopped: $(printf '%s' "${stopped}" | tr '\n' ' ')"
  fi
  node="$(docker ps --filter "label=io.x-k8s.kind.cluster=${capi}" \
    --filter label=io.x-k8s.kind.role=control-plane --format '{{.Names}}' | head -n1)"
  if [[ -n "${node}" ]]; then
    current="$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' "${node}")"
    advertised="$(docker exec "${node}" sed -n 's/^ *- --advertise-address=//p' \
      /etc/kubernetes/manifests/kube-apiserver.yaml 2>/dev/null || true)"
    if [[ -n "${advertised}" && "${advertised}" != "${current}" ]]; then
      log "tenant API: broken. ${node} now has address ${current}, but its API server was set up on ${advertised}, as after a Docker restart. A CAPD tenant does not recover from that; run $0 down, then up"
      return 0
    fi
  fi
  if [[ -f "${TENANT_KUBECONFIG}" ]]; then
    log "tenant API: not reachable with ${TENANT_KUBECONFIG}; $0 kubeconfig writes it again"
  else
    log "tenant API: no kubeconfig yet; $0 kubeconfig writes it"
  fi
}

cmd_nvkectl() {
  [[ -x "${NVKECTL}" ]] || die "nvkectl is not built; run $0 up"
  if ! kind get clusters 2>/dev/null | grep -Fx "${KIND_CLUSTER_NAME}" >/dev/null || ! management_running; then
    die "the management cluster ${KIND_CLUSTER_NAME} is not running; run $0 up"
  fi
  mkdir -p "${NVKECTL_CONFIG_HOME}"
  port_forward
  register_site
  nvkectl "$@"
}

cmd_validate() {
  preflight
  if ! kind get clusters 2>/dev/null | grep -Fx "${KIND_CLUSTER_NAME}" >/dev/null || ! management_running; then
    die "the management cluster ${KIND_CLUSTER_NAME} is not running; run $0 up"
  fi
  if [[ "${TRIAL_SKIP_TENANT_VALIDATE:-0}" != 1 ]] && ! tenant_reachable; then
    port_forward
    register_site
    tenant_kubeconfig
  fi
  validate
}

cmd_kubeconfig() {
  preflight
  port_forward
  register_site
  tenant_kubeconfig
}

cmd_down() {
  preflight
  if kind get clusters 2>/dev/null | grep -Fx "${KIND_CLUSTER_NAME}" >/dev/null; then
    if management_running; then
      delete_tenant
    else
      log "the management cluster ${KIND_CLUSTER_NAME} is not running; deleting it without deleting the tenant first"
    fi
    stop_port_forward
    log "deleting the management cluster ${KIND_CLUSTER_NAME}"
    kind delete cluster --name "${KIND_CLUSTER_NAME}"
  fi
  stop_port_forward

  # CAPD labels each tenant container with its CAPI cluster name. Remove any
  # the tenant delete left behind.
  if [[ -f "${CAPI_CLUSTER_FILE}" ]]; then
    local capi leftovers
    capi="$(cat "${CAPI_CLUSTER_FILE}")"
    leftovers="$(docker ps -aq --filter "label=io.x-k8s.kind.cluster=${capi}")"
    if [[ -n "${leftovers}" ]]; then
      log "removing leftover tenant containers of ${capi}"
      # shellcheck disable=SC2086
      docker rm -f ${leftovers} >/dev/null
    fi
  fi
  rm -rf "${STATE_DIR}"
  log "removed. The built images stay in Docker: ${CONTROLLER_IMAGE}, ${NODE_IMAGE}"
}

# A kind create that was interrupted leaves a node container that never ran
# kubeadm: kind lists the cluster but has no kubeconfig for it.
management_running() {
  kind get kubeconfig --name "${KIND_CLUSTER_NAME}" >"${KUBECONFIG}" 2>/dev/null &&
    kubectl get --raw /readyz >/dev/null 2>&1
}

delete_tenant() {
  local capi
  capi="$(capi_cluster_name || true)"
  if [[ -n "${capi}" ]]; then
    printf '%s\n' "${capi}" >"${CAPI_CLUSTER_FILE}"
    # Deleting a control-plane machine rewrites the load balancer's config,
    # and a Docker restart leaves that container stopped.
    local stopped
    stopped="$(docker ps -aq --filter "label=io.x-k8s.kind.cluster=${capi}" --filter status=exited)"
    # shellcheck disable=SC2086
    [[ -z "${stopped}" ]] || docker start ${stopped} >/dev/null
  fi
  if [[ -x "${NVKECTL}" ]] && kubectl get deployment -n nvke-controller nvke-controller >/dev/null 2>&1; then
    port_forward
    register_site
    if tenant_json >/dev/null; then
      log "deleting tenant $(tenant_name)"
      nvkectl delete cluster "$(tenant_name)" || true
      local _
      for _ in $(seq 1 60); do
        [[ -z "$(kubectl get clusters.cluster.x-k8s.io -A --no-headers 2>/dev/null)" ]] && break
        sleep 10
      done
    fi
  fi
}

case "${1:-up}" in
  up) cmd_up ;;
  validate) cmd_validate ;;
  status) cmd_status ;;
  kubeconfig) cmd_kubeconfig ;;
  down) cmd_down ;;
  nvkectl) cmd_nvkectl "${@:2}" ;;
  -h | --help | help) sed -n '5,34p' "$0" | sed 's/^# \{0,1\}//' ;;
  *) die "unknown command '$1'; use up, validate, status, kubeconfig, nvkectl or down" ;;
esac
