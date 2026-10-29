// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lane

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

// The components a run can build from the checkout, by the names the plan
// gives them.
const (
	ComponentController      = "controller"
	ComponentCLI             = "cli"
	ComponentCAPIProvisioned = "capi-provisioned"
)

// BuildableComponents are the components Build can name, in order.
var BuildableComponents = []string{ComponentController, ComponentCLI, ComponentCAPIProvisioned}

// VersionStrategyCheckout is what a component built from the checkout records
// as its version strategy: it was not resolved, and nothing published holds it.
const VersionStrategyCheckout = "checkout"

// Build names the components a run builds from the checkout and stages on
// the node, instead of the published ones the plan names.
type Build struct {
	Controller, CLI, CAPIProvisioned bool
}

// ParseBuild reads component names. "none" or nothing builds nothing.
func ParseBuild(names []string) (Build, error) {
	var b Build
	for _, name := range names {
		switch name {
		case ComponentController:
			b.Controller = true
		case ComponentCLI:
			b.CLI = true
		case ComponentCAPIProvisioned:
			b.CAPIProvisioned = true
		case "none":
		default:
			return Build{}, fmt.Errorf("cannot build %q: the components built here are %s", name, strings.Join(BuildableComponents, ", "))
		}
	}
	return b, nil
}

// Or is every component either builds.
func (b Build) Or(o Build) Build {
	return Build{b.Controller || o.Controller, b.CLI || o.CLI, b.CAPIProvisioned || o.CAPIProvisioned}
}

// Names are the components b builds, in BuildableComponents order.
func (b Build) Names() []string {
	var names []string
	for _, c := range []struct {
		name string
		on   bool
	}{{ComponentController, b.Controller}, {ComponentCLI, b.CLI}, {ComponentCAPIProvisioned, b.CAPIProvisioned}} {
		if c.on {
			names = append(names, c.name)
		}
	}
	return names
}

// The checkout's paths that go into each build, from the go.mod replace
// directives of controller, cli and cluster-api-provider-provisioned. The
// deploy path goes with the controller: it is exported from the commit the
// controller was built at.
var (
	controllerPaths = []string{"controller/", "deploy/", "agent/", "auth/", "management-engine/", "go.work", "go.work.sum"}
	cliPaths        = []string{"cli/", "controller/api/", "controller/deploy/charts/", "management-engine/", "go.work", "go.work.sum"}
	// Not part of any build: the suite is compiled from the checkout, nico-sim
	// and the CAPD reboot controller are other lanes' tools.
	notBuilt = []string{"controller/test/", "controller/nico-sim/", "controller/hack/capd-reboot-controller/"}
	// What reaches the node only through an artifact built elsewhere: the
	// agents are in the OS image, and NKTP is a published image the deploy
	// path installs.
	offNode = []string{"agent/", "os-image/", "nktp/"}
)

// ChangedBuild is what changes to paths need built for the node to run them,
// and the top-level directories among them that no build here puts there.
func ChangedBuild(paths []string) (b Build, unexercised []string) {
	for _, p := range paths {
		if hasPrefix(p, offNode) {
			top, _, _ := strings.Cut(p, "/")
			if !slices.Contains(unexercised, top+"/") {
				unexercised = append(unexercised, top+"/")
			}
		}
		if hasPrefix(p, notBuilt) {
			continue
		}
		b.Controller = b.Controller || hasPrefix(p, controllerPaths)
		b.CLI = b.CLI || hasPrefix(p, cliPaths)
		b.CAPIProvisioned = b.CAPIProvisioned || strings.HasPrefix(p, "cluster-api-provider-provisioned/")
	}
	return b, unexercised
}

// BuildPaths are git pathspecs for what b builds from, for checking it is
// committed: a build is tagged with the commit, and the deploy path is
// exported from it.
func BuildPaths(b Build) []string {
	var paths []string
	if b.Controller {
		paths = append(paths, controllerPaths...)
	}
	if b.CLI {
		paths = append(paths, cliPaths...)
	}
	if b.CAPIProvisioned {
		paths = append(paths, "cluster-api-provider-provisioned/")
	}
	if len(paths) == 0 {
		return nil
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	if b.Controller {
		for _, p := range notBuilt {
			paths = append(paths, ":(exclude)"+p)
		}
	}
	return paths
}

func hasPrefix(p string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// AsRun is plan with the components r built recorded as built from the
// checkout at r's commit, and their published locations dropped: what the
// run staged, for the artifact check and the result to read. plan is not
// changed.
func AsRun(plan *qualification.Plan, r Run) (*qualification.Plan, error) {
	built := map[string]string{}
	if r.ControllerBuild {
		built[ComponentController] = r.ControllerVersion
	}
	if r.CLIBuild {
		built[ComponentCLI] = r.CLIVersion
		built["cli-amd64"] = r.CLIVersion
		built["cli-arm64"] = r.CLIVersion
	}
	if r.ProvisionedBuild {
		built[ComponentCAPIProvisioned] = r.ProvisionedVersion
	}
	out := *plan
	out.Combinations = slices.Clone(plan.Combinations)
	found := false
	for i := range out.Combinations {
		c := &out.Combinations[i]
		if c.ID != r.Inputs.Combination {
			continue
		}
		found = true
		c.Components = asBuilt(c.Components, built)
		c.Dependencies = asBuilt(c.Dependencies, built)
		c.Streams = slices.Clone(c.Streams)
		for j := range c.Streams {
			c.Streams[j].Dependencies = asBuilt(c.Streams[j].Dependencies, built)
		}
	}
	if !found {
		return nil, fmt.Errorf("the plan has no combination %s", r.Inputs.Combination)
	}
	return &out, nil
}

func asBuilt(entries []qualification.ComponentVersion, built map[string]string) []qualification.ComponentVersion {
	out := slices.Clone(entries)
	for i, e := range out {
		if version, ok := built[e.Name]; ok {
			out[i] = qualification.ComponentVersion{Name: e.Name, Version: version, VersionStrategy: VersionStrategyCheckout}
		}
	}
	return out
}
