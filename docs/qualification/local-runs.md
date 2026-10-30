<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Running the hardware lane from a workstation

`qualify local` runs the hardware qualification lane from your own checkout.
One command takes one stream through the same steps as the `qualify-e2e`
workflow, on the same Forge hardware: it provisions a site, bootstraps it,
runs the e2e suite against it, tears it down, and records the result.

You need it when your change comes from a fork. The workflow does not run for
a fork's pull request, because a fork cannot reach the Forge API key or the
Teleport join token. With `qualify local` you run the lane yourself, with your
own credentials. It runs the workflow's own steps rather than a copy of them,
so a pass from a clean checkout, with nothing built that CI would pull, is the
same evidence a CI run would give.

It is also how the lane itself is developed. A change to the workflow's steps
can be run and debugged from a workstation, with every file the run wrote kept
locally, instead of pushing and waiting on CI for each try. `--keep-site` and
`--keep-tenant` leave the machines up to inspect afterwards.

With `--from-source` it builds every image the site needs from the checkout
and pulls none, so it needs no registry credential at all.

[hardware-lane.md](hardware-lane.md) says what each step of the lane does and
why; this page says how to run it from a workstation and where that differs
from CI.

## How it stays the same run as CI

The workflow's per-stream steps live in
`controller/test/e2e/hack/qualify-lane.sh`. Each workflow step is one call to
that script, and `qualify local` calls the same steps in the same order, so no
run logic exists twice.

| Stage | GitHub CI | `qualify local` |
|---|---|---|
| Plan | `qualify plan` | `qualify plan`, in-process, after fetching upstream's tags |
| Stream values | `qualify-lane.sh matrix`, `stream-env` | `qualify-lane.sh stream-env` |
| Check artifacts | `qualify verify` | the same checker for registry images; the CDN is asked through the jump host, because it refuses workstations |
| Resolve | `qualify-lane.sh resolve`, `controller-image` | the same |
| Site inputs | `qualify-lane.sh site-inputs` | the same |
| Teleport | a `tbot` bot identity | your `tsh login` |
| Config, suite | `qualify-lane.sh config`, `run` | the same |
| Scrub | `qualify-lane.sh withhold` | the same |
| Record | `qualify result` | `qualify result`, in-process |
| Publish, upload | TestGrid if trusted; workflow artifacts | nothing is published; the files are kept in the run directory |

The script also has an `artifacts` step that only `qualify local` calls. It
lists what the suite wrote in `controller/test/e2e/`, which `qualify local`
then moves into the run directory.

## Run

From the repository root, with a current Teleport login:

```bash
tsh login
```

```bash
go run ./release/cmd/qualify local --stream forge-l40s-amd64-ubuntu-24.04-kube-1.36 --stage full --from-source
```

`--stream` and `--stage` (`provision`, `bootstrap` or `full`) are required.
`go run ./release/cmd/qualify local --help` lists every flag.

| Flag | Default | What it does |
|---|---|---|
| `--build` | pull all | builds the named components from the checkout instead of pulling them: `controller`, `cli`, `capi-provisioned`. Verify does not ask a registry for them |
| `--from-source` | off | builds all three and names the images under `localhost/nvke` (`--registry local`), so no registry and no registry credential is involved |
| `--dry-run` | off | plans, verifies, resolves and stages the site inputs, then prints the suite's arguments without taking a machine |
| `--keep-site` | off | keeps the site node after the run, pass or fail, and a tenant whose specs failed |
| `--keep-tenant` | off | keeps every tenant, passed or not, and the site node they hang off. The spec that deletes a tenant is skipped, so tenant deletion is not tested |
| `--plan FILE` | plan `--scope` | reads that plan instead of planning |
| `--scope`, `--subject` | `full`, `nvke` | what to plan when `--plan` is not given |
| `--version` | the highest version | the version of the subject to plan, as `qualify plan --version` takes it |
| `--overrides` | none | values to plan in place of what resolves, as `qualify plan --overrides` takes them: `name=value` pairs separated by commas. An OS image's value is a URL, which is how a run uses a published image while the one its version names is not published yet. Refused for a component `--build` builds, and with `--plan` |
| `--combination` | the only one | needed only when the plan has more than one |
| `--registry` | `nvcr`, or `local` with `--from-source` | where the site images are pulled from: `nvcr`, `ghcr`, or `local` when every image is built |
| `--ngc-org` | `$QUALIFY_E2E_NGC_ORG` | the NGC org the NVCR images are published under |
| `--node-user` | `$QUALIFY_E2E_NODE_USER` | the account the OS image bakes; empty for the OSS images |
| `--wait` | `0` | how long to keep asking for an artifact that is not published yet |
| `--run-dir DIR` | under the user cache | where the run's files are kept |

The OS images are never built here. Even with `--from-source`, a stream names
where the provider reads them from, and verify checks that they are there.

Whatever `--keep-site` or `--keep-tenant` keeps must be deleted by hand. A kept
site node can be logged into, because the provider writes the run's key to it.
A kept tenant node cannot: nothing puts a key there and the image locks the
account, so reach the tenant through its kubeconfig on the site, or read its
serial console.

## What it needs from your shell

A run reads the same names the workflow's job puts in its environment, from
the shell that starts it. Most of them are the same for everyone who runs the
lane, and the lane owners can give them to you. Only the jump hosts and your
Teleport login on each are your own, plus the Forge API key, which is secret.

### The simplest case

A `--from-source` run needs nothing from a registry, so this is all of it.
Keep it in a file only you can read, outside the repository:

```bash
# ~/nvke-forge.env (chmod 600)

# The same for everyone; ask the lane owners.
export FORGE_TELEPORT_PROXY=<teleport proxy>
export QUALIFY_E2E_NGC_ORG=<ngc org>
export FORGE_ORG_ID=<org id>
export FORGE_SITE_ID=<site-1 id>
export FORGE_VPC_ID=<site-1 vpc id>
export FORGE_SUBNET_ID=<site-1 subnet id>
export FORGE_SSH_KEY_GROUP_ID=<site-1 key group id>
export FORGE_INSTANCE_TYPE_CPU_ID=<site-1 cpu type id>
export FORGE_INSTANCE_TYPE_GPU_ID=<site-1 gpu type id>

# Yours: the site-1 jump host, and one of the Logins `tsh status` lists.
export FORGE_JUMP_HOST=<jump host>
export FORGE_TELEPORT_LOGIN=<login>

# Secret.
export FORGE_API_KEY=<forge api key>
```

Then, from the repository root:

```bash
source ~/nvke-forge.env
tsh login --ttl=720
go run ./release/cmd/qualify local --stream forge-l40s-amd64-ubuntu-24.04-kube-1.36 --stage full --from-source
```

For a GB200 stream, add site-2. The ids are shared; the jump host and login
are yours:

```bash
export FORGE_SITE_2_SITE_ID=<site-2 id>
export FORGE_SITE_2_VPC_ID=<site-2 vpc id>
export FORGE_SITE_2_VPC_PREFIX_ID=<site-2 vpc prefix id>
export FORGE_SITE_2_SSH_KEY_GROUP_ID=<site-2 key group id>
export FORGE_SITE_2_INSTANCE_TYPE_CPU_AMD64_ID=<site-2 amd64 cpu type id>
export FORGE_SITE_2_INSTANCE_TYPE_CPU_ARM64_ID=<site-2 arm64 cpu type id>
export FORGE_SITE_2_INSTANCE_TYPE_GPU_GB200_ID=<site-2 gb200 type id>
export FORGE_SITE_2_JUMP_HOST=<jump host>
export FORGE_SITE_2_TELEPORT_LOGIN=<login>
```

Pulling images instead of building them adds a registry credential; see
[Registry](#registry). The tables below are the reference for every name.

### Tools

| Tool | Used for | Checked up front |
|---|---|---|
| `go` | Running `qualify local` itself, and building nvkectl from the checkout | Needed to start |
| `bash`, `jq` | The lane script, `controller/test/e2e/hack/qualify-lane.sh` | Yes |
| `git` | Planning from upstream's tags, and describing what was built | Yes |
| `make` | Building nvkectl and the site inputs | Yes |
| `docker` | Building the site images, and pulling them unless every one is built. The controller image cross-compiles for the site's architecture, which needs BuildKit (Docker Desktop's default) | Yes |
| `tsh` | Reaching the site's jump host through Teleport | Yes |
| `oras` | Pulling nvkectl when it is not built | No; only that path needs it |

### Teleport

| Name | Required | What it is |
|---|---|---|
| `FORGE_TELEPORT_PROXY` | Yes, unless `TELEPORT_PROXY` is set | The Teleport proxy. It is passed on as `TELEPORT_PROXY` unless that is already set. |
| A current `tsh login` | Yes | Checked with `tsh status` after planning and before verify, which asks the CDN through the jump host. A dry run is checked too. The login must last the whole run: the suite opens relays until the end, teardown included, and a `full` run is about an hour plus builds. Ask for a longer one with `tsh login --ttl=<minutes>` if yours is short. |

### Forge

`environments/forge.yaml` reads these. An unset or empty variable is an
error, so every site-1 id is required on every run, a GB200 run included. The
jump hosts and Teleport logins default to empty, and a site run stops on an
empty jump host before a machine exists. Site-2 is skipped unless all of its
ids are set, and a GB200 stream needs it.

| Name | Site | What it is |
|---|---|---|
| `FORGE_API_KEY` | All | The Forge API key. Secret; read by the suite. |
| `FORGE_ORG_ID` | All | The Forge org the machines are created in |
| `FORGE_SITE_ID`, `FORGE_VPC_ID`, `FORGE_SUBNET_ID` | site-1 | The site, and the VPC and subnet its machines attach to |
| `FORGE_SSH_KEY_GROUP_ID` | site-1 | The key group for serial console access. It grants no shell on a node. |
| `FORGE_INSTANCE_TYPE_CPU_ID`, `FORGE_INSTANCE_TYPE_GPU_ID` | site-1 | The CPU and GPU (L40S) machine types |
| `FORGE_JUMP_HOST`, `FORGE_TELEPORT_LOGIN` | site-1 | The jump host in front of the machines, and your Teleport login on it (one of the Logins `tsh status` lists) |
| `FORGE_SITE_2_SITE_ID`, `FORGE_SITE_2_VPC_ID`, `FORGE_SITE_2_VPC_PREFIX_ID` | site-2 | The site, and the VPC and VPC prefix its machines attach to |
| `FORGE_SITE_2_SSH_KEY_GROUP_ID` | site-2 | As for site-1 |
| `FORGE_SITE_2_INSTANCE_TYPE_CPU_AMD64_ID`, `FORGE_SITE_2_INSTANCE_TYPE_CPU_ARM64_ID`, `FORGE_SITE_2_INSTANCE_TYPE_GPU_GB200_ID` | site-2 | The machine types: a site controller of either architecture, and the arm64 GB200 tenant |
| `FORGE_SITE_2_JUMP_HOST`, `FORGE_SITE_2_TELEPORT_LOGIN` | site-2 | As for site-1 |

### Registry

Where the site images come from is set by `--registry`.

| `--registry` | Needs |
|---|---|
| `nvcr`, the default | `NGC_API_KEY`, `QUALIFY_E2E_NGC_ORG` (or `--ngc-org`), and `docker login nvcr.io` |
| `ghcr` | `GITHUB_ACTOR`, `GITHUB_TOKEN`, and `docker login ghcr.io` |
| `local`, the default with `--from-source` | Nothing; every image is built |

Planning needs `QUALIFY_E2E_NGC_ORG` whatever the registry, because
`release/registry.yaml` names the artifacts by it. A run given `--plan` and
`--from-source` does not plan, and needs none of these.

### Optional

| Name | Default | What it changes |
|---|---|---|
| `QUALIFY_E2E_NODE_USER` (or `--node-user`) | The OSS images' account | The account the OS image bakes. Set it for an image from another pipeline. |
| `QUALIFY_E2E_SITE_CONTROLLER_VERSION` | The plan's controller | The site controller version, in place of the one the plan names |
| `SITE_DEPLOY_PATH_REF` | The controller's commit | The commit the deploy path is exported from |
| `NVKE_CONTROLLER_NAMESPACE`, `NVKE_CONTROLLER_SERVICE` | `nvke-controller` | The name the controller is installed under. A release cut before the rename uses `nke-controller`. |

## Run directory

By default, a new directory under the user cache:
`~/Library/Caches/nvke/qualify-local/<time>-<stream>/` on macOS,
`~/.cache/nvke/qualify-local/<time>-<stream>/` on Linux. It holds `plan.json`
(unless `--plan` named one), `lane.env` (what each step resolved),
`e2e-config.yaml`, `site-inputs/`, `qualify-e2e.log`, the JUnit and JSON
reports, any diagnostics, and `qualify-result.json`. A dry run writes no
`e2e-config.yaml` and no result. The jump hosts and the token issuer are
scrubbed from the log, the reports and the diagnostics before they are moved
there.

When anything was built, the result is written from `plan-as-run.json`: the
plan with each built component naming the commit it was built at, so the
result records what ran rather than what the plan would have pulled. A build
takes the working tree, uncommitted changes included, and is named for
`HEAD`. The resolve step warns when a tracked file differs from `HEAD`; it
does not look at untracked files, which a build can still pick up.

## Interrupts

The suite runs in its own process group. The first Ctrl-C is passed to it
once, and Ginkgo tears the node down; wait for it. Further Ctrl-Cs are not
passed on, because Ginkgo skips its cleanup on a second interrupt and the
instance would leak.

## Planning from a fork

- **Which remote is upstream.** The one whose URL names
  `dsx-ai-factory/nvke`, whatever it is called. It is needed even with
  `--plan`, because the resolve step fetches the commits it stages from it.
  With none, `qualify local` stops before it creates the run directory and
  says to `git remote add upstream https://github.com/dsx-ai-factory/nvke.git`.
- **Tags.** `qualify local` fetches upstream's tags before planning, and not
  at all with `--plan`. A tag that exists only locally, or only on your fork,
  can still change the plan; delete it locally if it does.
- **Hash length.** Publishers describe commits in a fresh clone of upstream,
  so the planner sizes the abbreviation from the objects reachable from the
  upstream remote's branches and tags. A clone that has also fetched forks
  would otherwise describe a digit longer and name artifacts nobody published.

## Known limits

- Two runs from one checkout at once collide: the suite writes its log and
  reports in `controller/test/e2e/` before they are moved.
- Do not rebase or switch branches in the checkout while a run is going. The
  suite is compiled from it when the `run` step starts, and the site inputs
  are built from it.
- `qualify verify` reports a 401 ahead of a missing artifact, so a credential
  problem can hide what is missing.
- A `--keep-tenant` run skips the tenant-delete spec, which Ginkgo does not
  count as a failure, so its `qualify-result.json` can say `passed` without
  tenant deletion having been tested. Do not take it as a qualification.
