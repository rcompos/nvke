# Trial: NVKE on Cluster API with Docker

> **Preview:** NVKE is available as a preview. APIs, configuration and behavior can change between releases, and we do not recommend NVKE for production workloads. See [Preview status and known limitations](../overview/preview-status.md).

This page describes how to try NVIDIA Kubernetes Engine (NVKE) with one command, on a workstation with Docker or on GitHub-hosted runners in your fork. The trial builds NVKE from your checkout, deploys an NVKE site, creates one tenant cluster through the NVKE API, and validates both. It needs no GPU, no NVIDIA account and no NVIDIA registry: every NVKE image and binary is built from the checkout, and third-party images come from their upstream registries.

The trial is the first of three levels of open qualification:

| Level | Infrastructure | Proves |
| --- | --- | --- |
| Trial | Docker only: kind and the Cluster API Docker provider (CAPD) | NVKE builds, deploys, creates a tenant cluster and passes validation |
| Provider qualification | Your Cluster API infrastructure provider, on real or virtual machines | NVKE drives your provider through its generic Cluster API path |
| Hardware qualification | Your hardware, with GPU nodes | The full end-to-end run on what you would deploy |

The trial creates these on your Docker daemon:

- A kind cluster named `nvke-trial` that serves as the management cluster, with Cluster API, the kubeadm providers and CAPD installed by `clusterctl`.
- The NVKE site stack in that cluster: PostgreSQL, Temporal, SPIRE, cert-manager and the NVKE controller, which serves the NVKE API.
- A tenant cluster, `trial-1`, with one control-plane node and one worker, each a Docker container, running Kubernetes 1.34.8 with Calico.

The CNI is Calico because Docker Desktop's kernel has no `openvswitch` module, which the default OVN-Kubernetes CNI needs. The trial is for evaluation only. Do not reuse it for a real installation; [Plan your installation](../operate/plan-installation.md) starts the real one.

## Before you begin

- macOS or Linux, on arm64 or amd64, with Docker: Docker Desktop, or Docker Engine with BuildKit usable by your user without `sudo`.
- At least 4 CPUs and 16 GiB of memory for Docker. On Docker Desktop, set them in **Settings > Resources**.
- Free space on Docker's disk of 10 percent of the disk plus 15 GiB. The images, the management cluster and the tenant nodes share that disk, and below 10 percent free the kubelet evicts every pod. The trial checks this before it starts.
- These tools on `PATH`: `git`, GNU Make 4 or later, Go 1.27.3 or later, `kind` v0.32.0, `kubectl`, Helm 4, `jq`, `python3` and `curl`. macOS ships GNU Make 3.81 as `make`; install a newer one with `brew install make`, which provides `gmake`. You do not need `clusterctl`: the trial installs the pinned version into the repository.
- On Linux, `fs.inotify.max_user_instances` at 1024 or more and `fs.inotify.max_user_watches` at 1048576 or more. The management cluster and each tenant node run a containerd that watches files.
- Nothing else listening on port 18080, where the trial serves the NVKE API.
- Outbound internet access to GitHub, the Go module proxy and public container registries.

## Run the trial on your workstation

1. Clone the repository and change into it.

   ```bash
   git clone https://github.com/dsx-ai-factory/nvke.git
   ```

   ```bash
   cd nvke
   ```

2. Run the trial.

   ```bash
   trial/nvke-trial.sh up
   ```

   The first run builds the images and takes about 20 to 40 minutes, depending on the machine. In order, `up`:

   1. Creates the kind management cluster.
   2. Installs Cluster API, the kubeadm providers and CAPD with `clusterctl`.
   3. Builds the tenant node image with the node agent and Calico, the controller image, and `nvkectl`.
   4. Deploys the site stack, and patches the Cluster API provider controllers with the SPIRE exec-auth plugin, which they use to reach tenant clusters.
   5. Forwards the NVKE API to `http://127.0.0.1:18080`.
   6. Creates the tenant cluster from `trial/tenant.yaml` with `nvkectl create cluster` and waits for it to be provisioned and for both nodes to be Ready.
   7. Runs the validation suite against the site and the tenant.

   Every step skips what already exists, so if a run fails, fix the cause and run `up` again. When it finishes, it prints how to reach the tenant:

   ```text
   [nvke-trial] validation passed; results in .../trial/.state/validate
   [nvke-trial] done. The tenant is running:
   ```

3. Use the tenant cluster.

   ```bash
   export KUBECONFIG="$PWD/trial/.state/tenant.kubeconfig"
   ```

   ```bash
   kubectl get nodes
   ```

4. Use the NVKE API with `nvkectl`. The trial's `nvkectl` command starts the port-forward when it is not running and points `nvkectl` at the trial's site, keeping its configuration out of your own `~/.config/nvkectl`.

   ```bash
   trial/nvke-trial.sh nvkectl get clusters
   ```

   The NVKE API's reference is at `http://127.0.0.1:18080/api/docs` while the port-forward runs.

## Check and validate

`status` shows the management plane pods that are not running, the Cluster API objects, the NVKE tenants and the tenant's containers, and says why the tenant's API does not answer when it does not. It restarts the NVKE API port-forward if it stopped.

```bash
trial/nvke-trial.sh status
```

`validate` runs the validation suite again: the site checks, and the tenant checks as a read-mostly service account in the tenant. Results are in `trial/.state/validate`: the log, a JUnit report and a JSON report.

```bash
trial/nvke-trial.sh validate
```

`kubeconfig` writes the tenant kubeconfig again.

```bash
trial/nvke-trial.sh kubeconfig
```

## Remove the trial

`down` deletes the tenant cluster through the NVKE API, then the management cluster, and removes `trial/.state`. The images it built stay in Docker, so the next `up` is faster.

```bash
trial/nvke-trial.sh down
```

## Settings

Set these in the environment of `up` and the other commands.

| Variable | Default | Effect |
| --- | --- | --- |
| `KIND_CLUSTER_NAME` | `nvke-trial` | The management cluster's name |
| `TRIAL_API_PORT` | `18080` | The local port for the NVKE API |
| `TRIAL_WAIT_MINUTES` | `30` | How long `up` waits for the tenant cluster to be provisioned |
| `TRIAL_REBUILD` | unset | `1` rebuilds the images and `nvkectl` even if they exist |
| `TRIAL_SKIP_DISK_CHECK` | unset | `1` skips the free-disk check |
| `TRIAL_SKIP_VALIDATE` | unset | `1` ends `up` without validating |
| `TRIAL_SKIP_TENANT_VALIDATE` | unset | `1` validates the site alone |

## Run the trial on GitHub Actions in your fork

The `Trial` workflow, `.github/workflows/trial.yml`, runs the same trial on GitHub-hosted runners, one amd64 and one arm64. It uses no secrets, so it runs as it is in a fork.

1. Fork the repository on GitHub, and enable Actions in the fork under **Actions**.
2. Under **Actions > Trial**, select **Run workflow**.

Each job runs `up`, checks the tenant, then runs `down`, and takes about 15 to 25 minutes. It uploads the validation results as `trial-validation-<arch>`, and diagnostics as `trial-diagnostics-<arch>` when a step fails.

## Troubleshooting

**The tenant's pods are evicted, or the CNI install keeps retrying.** Docker's disk is nearly full. Check the space with `docker system df`, free it with `docker builder prune`, or raise the disk limit; on Docker Desktop, in **Settings > Resources**.

**The tenant's API stops answering after Docker restarts.** A CAPD tenant cluster does not survive a restart of Docker: its node containers come back on new addresses, while its API server, its certificate and etcd keep the old ones. `status` reports this. Remove the trial with `down` and create it again with `up`.

**`up` stops with "exists but is not running".** An interrupted run left the management cluster half created. Run `down`, then `up`.

**On macOS, the tenant kubeconfig points at `127.0.0.1`.** Docker Desktop does not route container addresses to the Mac, so the trial points the tenant kubeconfig at the port Docker publishes for CAPD's load balancer. This is expected.

## Next steps

To go from the trial to provider qualification, replace CAPD with your own Cluster API infrastructure provider:

- Install your provider with `clusterctl` before the NVKE controller, and grant the controller access to your provider's kinds with the controller chart's `capi.rbac.infrastructureResources`, as `controller/deploy/capi-trial/controller/overlay.yaml` does for CAPD.
- In the cluster request, name your provider's cluster and machine template kinds in `infra.cluster` and `infra.machineTemplate`, as `trial/tenant.yaml` does for CAPD.
- Patch your provider's controllers with the SPIRE exec-auth plugin: `controller/hack/apply-capi-spiffe-exec-auth.sh --providers` patches every provider `clusterctl` installed. See [Install the management plane](../operate/install-management-plane.md).
