<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Running the hardware lane from a fork

How to run the site controller lane from your own workstation against a
branch of a fork, with `qualify local`. The lane runs on Forge today.

Alongside this:

- [hardware-lane.md](hardware-lane.md) says what the lane does and why;
- [Running a stream from a workstation](../../release/README.md#running-a-stream-from-a-workstation)
  is the reference for `qualify local` and its flags;
- [triage.md](triage.md) is for a run that goes red.

## Why this exists

The `qualify-e2e` workflow runs the hardware lane only for code on `main`, so
a branch on a fork gets no hardware run before review. `qualify local` does
what one leg of that workflow does, from your shell and with your
credentials:

1. plan;
2. check everything the run needs;
3. build the site inputs;
4. take a Forge node and run the site controller suite;
5. release the node;
6. record the result.

Nothing is published.

What a run tests depends on `--build`:

| Run | Suite | Controller, nvkectl, capi-provisioned | OS image and agents |
|---|---|---|---|
| without `--build` | the fork's | published, from the commit the plan resolved | published |
| `--build changed` | the fork's | the fork's, for whichever the fork changes | published |

Without `--build`, a fork's run tests the fork's suite against the published
product. To test a change to the product, build it.

## What you need

**Access**

- A Forge tenant with an API key, and the ids of a site: the org, site, VPC,
  subnet, SSH key group and instance types.
- A Teleport login on the site's jump host, as one of the Logins
  `tsh status` lists.
- For `--registry nvcr`, an NGC key made in the org the images are published
  under. `--registry ghcr` needs no key.

**Tools on `PATH`**

- `go`, `git`, `make`, and Docker with buildx;
- `tsh`;
- `oras`, only when staging a published nvkectl.

**A checkout of the fork, with the upstream as a remote**

```console
$ git clone https://github.com/<you>/nvke.git && cd nvke
$ git remote add upstream https://github.com/dsx-ai-factory/nvke.git
$ git fetch upstream --tags
```

Pass that remote as `--remote upstream`; the default is `origin`. Planning
reads the upstream's tags, and the upstream's size sets how many hex digits a
described commit gets. That has to match what the publishers tagged; see
[Troubleshooting](#troubleshooting).

## One-time setup

1. **Keep the keys out of files and shell history.** Any secret store works.
   On macOS, the Keychain:

   ```console
   $ security add-generic-password -a "$USER" -s nvke-forge-api-key -w
   $ security add-generic-password -a "$USER" -s nvke-ngc-api-key -w
   ```

2. **Put the site's ids in an env file**, mode 0600, outside any checkout,
   e.g. `~/.config/nvke/forge.env`. It needs every variable
   `controller/test/e2e/environments/forge.yaml` reads. For the first site:

   ```sh
   FORGE_ORG_ID=
   FORGE_SITE_ID=
   FORGE_VPC_ID=
   FORGE_SUBNET_ID=
   FORGE_SSH_KEY_GROUP_ID=
   FORGE_INSTANCE_TYPE_CPU_ID=
   FORGE_INSTANCE_TYPE_GPU_ID=
   FORGE_JUMP_HOST=
   FORGE_TELEPORT_LOGIN=     # one of the Logins tsh status lists, not your Teleport user
   FORGE_TELEPORT_PROXY=
   QUALIFY_E2E_NGC_ORG=      # for --registry nvcr
   QUALIFY_E2E_NODE_USER=    # the account the OS image bakes; empty for the OSS images
   ```

   A second site is the `FORGE_SITE_2_*` set: `SITE_ID`, `VPC_ID`,
   `VPC_PREFIX_ID`, `SSH_KEY_GROUP_ID`, `INSTANCE_TYPE_CPU_AMD64_ID`,
   `INSTANCE_TYPE_CPU_ARM64_ID`, `INSTANCE_TYPE_GPU_GB200_ID`, `JUMP_HOST` and
   `TELEPORT_LOGIN`. Preflight names any that are missing.

3. **Log docker in to NVCR**, for `--registry nvcr`. The username is the
   literal `$oauthtoken`:

   ```console
   $ security find-generic-password -a "$USER" -s nvke-ngc-api-key -w \
       | docker login nvcr.io -u '$oauthtoken' --password-stdin
   ```

4. **Build the tool.** A binary built from one checkout can run another:

   ```console
   $ go build -C release -o ~/bin/qualify ./cmd/qualify
   ```

   Rebuild it after pulling. A stale binary shows up as `unknown flag`.

## Each run

From the root of the checkout, on the branch under test:

```console
$ tsh login --proxy="$FORGE_TELEPORT_PROXY"

$ set -a && . ~/.config/nvke/forge.env && set +a
$ export FORGE_API_KEY="$(security find-generic-password -a "$USER" -s nvke-forge-api-key -w)"
$ export NGC_API_KEY="$(security find-generic-password -a "$USER" -s nvke-ngc-api-key -w)"

$ git status

$ qualify local --remote upstream --scope l40s --stage full --build changed --dry-run
$ qualify local --remote upstream --scope l40s --stage full --build changed
```

Log in to Teleport fresh, so the login outlasts the stage. Check
`git status` first, because what `--build` builds has to be committed.

Do the dry run first. It makes every check, prints the `make` and `go test`
commands it would run, and takes no machine. Preflight names every problem at
once, before a node is taken.

### Choosing the stage

| `--stage` | What it does | Roughly |
|---|---|---|
| `provision` | Takes a Forge node, reaches it through the jump host, and releases it | 45 min |
| `bootstrap` | Also bootstraps the site controller on the node | 90 min |
| `full` | Also creates a tenant cluster: a CPU control plane and one worker | 3 h |

Preflight warns when the Teleport login ends before the stage usually does.
A `tsh login` made during the run is picked up.

### Choosing what to build

| `--build` | Builds |
|---|---|
| (none) | nothing; stages what the plan names |
| `changed` | whichever of the three below the branch changes since the commit the plan resolved |
| `controller` | the controller image, and with it the deploy path |
| `cli` | nvkectl |
| `capi-provisioned` | the cluster-api-provider-provisioned image |

Names combine, as in `--build controller,cli`. `changed` reads the paths that
differ, uncommitted changes included:

| A change to | Builds |
|---|---|
| `controller/`, `deploy/`, `auth/` | the controller |
| `cli/` | nvkectl |
| `controller/api/`, `controller/deploy/charts/`, `management-engine/`, `go.work*` | the controller and nvkectl, which both import them |
| `cluster-api-provider-provisioned/` | capi-provisioned |
| `agent/` | the controller, with a warning that the node runs the published agent |
| `os-image/`, `nktp/` | nothing, with a warning |
| `controller/test/` | nothing: the suite is always compiled from the checkout |

Each build is tagged with the checkout's 12-character commit, and the suite is
told to expect that version. That is why preflight refuses uncommitted
changes in what a build reads. The image would carry them, but the deploy
path, exported from the commit, would not. Commit them on the branch, or
stash them.

### Choosing the stream

`--scope` is a scope from `release/policy.yaml`. `--stream` picks one stream
from a scope that has several, and the error lists them.

The controller image builds for amd64 only. On an arm64 stream, such as the
GB200 ones, a controller build is refused before a machine is taken.
`capi-provisioned` builds for either architecture.

## While it runs

- **Interrupting.** Press Ctrl-C **once**. Ginkgo then runs its cleanup,
  which is what releases the node. A second Ctrl-C skips the cleanup and
  leaks the node.
- **Sleep.** On macOS, `caffeinate` holds off sleep for the run.
- **Progress.** Press Ctrl-T for a Ginkgo progress report.
- **More output.** Add `--suite-arg=-ginkgo.v=false --suite-arg=-ginkgo.vv`.
- **Focusing.** `--suite-arg=-ginkgo.focus=<regex>` runs only matching specs.
- **Keeping the node.** `--keep-site` keeps the site node when the run fails,
  for debugging. You then delete it by hand.

If a run dies before its teardown, delete the site node by hand. It is the
Forge instance labelled `purpose=nke-e2e-site-controller`.

## What a run leaves

Everything goes to the work directory, all mode 0600. The default is a new
directory under the user cache; `--work-dir` sets it. It holds:

- `plan.json`, the plan as resolved;
- `plan-as-run.json`, written when something was built: built components have
  version strategy `checkout`, the commit as their version, and no location;
- `e2e-config.yaml` and `resolved-inputs.json`;
- `qualify-e2e.log`, and the JUnit and JSON reports;
- the suite's diagnostics;
- `qualify-result.json`. Its identity is the fork, the full head commit and
  your git `user.name`. The workflow is `qualify local`, or
  `qualify local, with uncommitted changes`.

Jump hosts and the SSA issuer are withheld from the text files there. The
reports still carry remote command output, so do not attach the directory
anywhere public.

## Limits

- **Forge only.** The suite also has a DSX provider, but preflight does not
  check its SSA credentials (`<SITE>_SSA_CLIENT_ID`,
  `<SITE>_SSA_CLIENT_SECRET` and `SSA_ISSUER`), and the `full` stage accepts
  Forge only.
- **The controller builds for amd64 only.** On an arm64 workstation it builds
  under emulation, which is slow.
- **The OS image cannot be built here.** Neither can the agents in it. Forge
  boots the published image from the CDN.
- **The tenant's GPU checks do not run yet.** The lane creates a GPU worker
  but does not turn on the tenant GPU checks.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `unknown flag` | a stale `qualify` binary | rebuild it |
| artifacts for a described commit do not exist | the plan describes commits longer than the publishers did, because the clone that made it has more objects, such as forks fetched | plan here rather than with `--plan`, with `--remote` naming the upstream |
| `docker may not read … nvcr.io` | the NGC key is from another org | log docker in with a key made in the org the images are published under |
| `… does not exist; check the version` | that tag was never published | build the component, or plan another version |
| an OS image check fails with 403 | the CDN refuses your network | none needed: those locations are already asked from the site, through the jump host. A failure there is real |
| a Teleport warning, or `tsh ssh` failing | an expired login, or the wrong one | `tsh login`, and set `FORGE_TELEPORT_LOGIN` to one of the Logins listed |
| `what the run builds has uncommitted changes` | uncommitted changes in what a build reads | commit or stash them |
| `amd64-only` | a controller build for an arm64 stream | run without building the controller, or pick an amd64 stream |
| `BeforeAll` fails on the controller name | a controller from before the rename | the name is read from the deploy path; `--controller-name` overrides it |
| `… is left from an earlier run` | files an earlier run left in `controller/test/e2e` | move them out |
