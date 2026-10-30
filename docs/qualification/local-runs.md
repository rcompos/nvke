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
so a pass is the same evidence a CI run would give.

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

The same names the workflow's job puts in its environment:

- The Forge identifiers and `FORGE_API_KEY` that `environments/forge.yaml`
  reads, and the site's `*_JUMP_HOST` and `*_TELEPORT_LOGIN`.
- `FORGE_TELEPORT_PROXY`, taken for `TELEPORT_PROXY`, and a current
  `tsh login`. The run checks `tsh status` after planning and verify, before
  any step runs; `--dry-run` skips the check.
- For NVCR, `NGC_API_KEY` and `QUALIFY_E2E_NGC_ORG`; for GHCR, `GITHUB_ACTOR`
  and `GITHUB_TOKEN`; and `docker login` to that registry. `--from-source`
  needs none of these.
- `QUALIFY_E2E_NODE_USER` when the OS image bakes an account other than the
  OSS images'.
- `bash`, `jq`, `git`, `make`, `docker` and `tsh` on `PATH`, checked before
  anything runs, and `oras` when nvkectl is pulled rather than built, which is
  not checked up front.

Keep the values in a file of your own outside the repository and source it;
none of them is checked in.

## Run directory

By default, a new directory under the user cache:
`~/Library/Caches/nvke/qualify-local/<time>-<stream>/` on macOS,
`~/.cache/nvke/qualify-local/<time>-<stream>/` on Linux. It holds `plan.json`,
`lane.env` (what each step resolved), `e2e-config.yaml`, `site-inputs/`,
`qualify-e2e.log`, the JUnit and JSON reports, any diagnostics, and
`qualify-result.json`. Credentials are scrubbed before the files are moved
there.

## Interrupts

The suite runs in its own process group. The first Ctrl-C is passed to it
once, and Ginkgo tears the node down; wait for it. Further Ctrl-Cs are not
passed on, because Ginkgo skips its cleanup on a second interrupt and the
instance would leak.

## Planning from a fork

- **Which remote is upstream.** The one whose URL names
  `dsx-ai-factory/nvke`, whatever it is called. With none, `qualify local`
  stops before it creates the run directory and says to
  `git remote add upstream https://github.com/dsx-ai-factory/nvke.git`.
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
- Building an image for the other architecture than the workstation's runs
  under emulation and is slow, for example an amd64 controller on Apple
  Silicon.
- Do not rebase or switch branches in the checkout while a run is going. The
  suite is compiled from it when the `run` step starts, and the site inputs
  are built from it.
- `qualify verify` reports a 401 ahead of a missing artifact, so a credential
  problem can hide what is missing.
- A `--keep-tenant` run skips the tenant-delete spec, which Ginkgo does not
  count as a failure, so its `qualify-result.json` can say `passed` without
  tenant deletion having been tested. Do not take it as a qualification.
