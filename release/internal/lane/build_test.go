// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lane

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

func TestResolveBuildsWhatItIsAskedToOverThePlan(t *testing.T) {
	in, _ := FromPlan(testPlan(), "", "forge-cpu-amd64")
	o := options("/inputs")
	o.Build = Build{Controller: true, CLI: true, CAPIProvisioned: true}
	r, err := Resolve(in, o)
	if err != nil {
		t.Fatal(err)
	}
	if !r.ControllerBuild || r.ControllerVersion != "0123456789ab" {
		t.Errorf("controller: build %v version %q", r.ControllerBuild, r.ControllerVersion)
	}
	if !r.CLIBuild || r.CLIVersion != "0123456789ab" || r.CLIRef != "" || r.NVKECTL != "/repo/cli/dist/nvkectl-linux-amd64" {
		t.Errorf("cli: build %v version %q ref %q nvkectl %q", r.CLIBuild, r.CLIVersion, r.CLIRef, r.NVKECTL)
	}
	if !r.ProvisionedBuild || r.ProvisionedImage() != "nvcr.io/org/cluster-api-provider-provisioned:0123456789ab" {
		t.Errorf("capi-provisioned: build %v image %q", r.ProvisionedBuild, r.ProvisionedImage())
	}
	if !strings.HasPrefix(r.Versions(), "controller=0123456789ab,cli=0123456789ab,capi-provisioned=0123456789ab,") {
		t.Errorf("versions name the builds: %s", r.Versions())
	}
	vars := r.SiteInputsVars()
	for _, want := range []string{"SITE_NVKE_CONTROLLER_BUILD=1", "SITE_CLI_BUILD=1", "SITE_PROVISIONED_BUILD=1", "SITE_PROVISIONED_VERSION=0123456789ab"} {
		if !slices.Contains(vars, want) {
			t.Errorf("site-inputs vars have no %s: %v", want, vars)
		}
	}
}

func TestResolveBuildsCAPIProvisionedForArm64(t *testing.T) {
	in, _ := FromPlan(testPlan(), "", "forge-gb200-arm64")
	o := options("/inputs")
	o.Build.CAPIProvisioned = true
	if _, err := Resolve(in, o); err != nil {
		t.Fatalf("buildx builds capi-provisioned for either arch: %v", err)
	}
}

func TestParseBuild(t *testing.T) {
	b, err := ParseBuild([]string{"cli", "capi-provisioned", "none"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Build{CLI: true, CAPIProvisioned: true}); b != want {
		t.Errorf("got %+v, want %+v", b, want)
	}
	if _, err := ParseBuild([]string{"os-image"}); err == nil || !strings.Contains(err.Error(), "controller, cli, capi-provisioned") {
		t.Errorf("want an error naming what can be built, got %v", err)
	}
}

func TestChangedBuild(t *testing.T) {
	for name, tc := range map[string]struct {
		paths       []string
		want        Build
		unexercised []string
	}{
		"nothing":             {nil, Build{}, nil},
		"docs and release":    {[]string{"README.md", "release/cmd/qualify/local.go"}, Build{}, nil},
		"the suite":           {[]string{"controller/test/e2e/site_test.go", "controller/nico-sim/main.go"}, Build{}, nil},
		"the controller":      {[]string{"controller/internal/x.go"}, Build{Controller: true}, nil},
		"the deploy path":     {[]string{"deploy/nico/controller/release.yaml"}, Build{Controller: true}, nil},
		"the cluster API":     {[]string{"controller/api/v1/types.go"}, Build{Controller: true, CLI: true}, nil},
		"the charts":          {[]string{"controller/deploy/charts/x.yaml"}, Build{Controller: true, CLI: true}, nil},
		"nvkectl":             {[]string{"cli/cmd/root.go"}, Build{CLI: true}, nil},
		"capi-provisioned":    {[]string{"cluster-api-provider-provisioned/main.go"}, Build{CAPIProvisioned: true}, nil},
		"the workspace":       {[]string{"go.work.sum"}, Build{Controller: true, CLI: true}, nil},
		"the agent":           {[]string{"agent/a.go", "agent/b.go"}, Build{Controller: true}, []string{"agent/"}},
		"the os image, nktp":  {[]string{"os-image/x", "nktp/y"}, Build{}, []string{"os-image/", "nktp/"}},
		"a management engine": {[]string{"management-engine/x.go"}, Build{Controller: true, CLI: true}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, unexercised := ChangedBuild(tc.paths)
			if got != tc.want || !reflect.DeepEqual(unexercised, tc.unexercised) {
				t.Errorf("got %+v %v, want %+v %v", got, unexercised, tc.want, tc.unexercised)
			}
		})
	}
}

func TestBuildPathsLeaveTheSuiteOut(t *testing.T) {
	if got := BuildPaths(Build{}); got != nil {
		t.Errorf("nothing built has nothing to check: %v", got)
	}
	if got := BuildPaths(Build{CAPIProvisioned: true}); !reflect.DeepEqual(got, []string{"cluster-api-provider-provisioned/"}) {
		t.Errorf("capi-provisioned: %v", got)
	}
	got := BuildPaths(Build{Controller: true, CLI: true})
	for _, want := range []string{"controller/", "cli/", "deploy/", ":(exclude)controller/test/"} {
		if !slices.Contains(got, want) {
			t.Errorf("no %s in %v", want, got)
		}
	}
	if len(got) != len(slices.Compact(slices.Clone(got))) {
		t.Errorf("repeats: %v", got)
	}
}

func TestAsRunRecordsWhatWasBuilt(t *testing.T) {
	plan := testPlan()
	in, _ := FromPlan(plan, "", "forge-cpu-amd64")
	o := options("/inputs")
	o.Build = Build{CLI: true, CAPIProvisioned: true}
	r, err := Resolve(in, o)
	if err != nil {
		t.Fatal(err)
	}
	got, err := AsRun(plan, r)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]qualification.ComponentVersion{}
	for _, e := range slices.Concat(got.Combinations[0].Components, got.Combinations[0].Dependencies) {
		entries[e.Name] = e
	}
	built := qualification.ComponentVersion{Version: "0123456789ab", VersionStrategy: VersionStrategyCheckout}
	for _, name := range []string{"cli", "cli-amd64", "cli-arm64", "capi-provisioned"} {
		want := built
		want.Name = name
		if entries[name] != want {
			t.Errorf("%s: got %+v, want %+v", name, entries[name], want)
		}
	}
	if entries["controller"].Version != "v0.2.0-98-gabc" {
		t.Errorf("the published controller is kept: %+v", entries["controller"])
	}
	if plan.Combinations[0].Dependencies[0].URL == "" {
		t.Error("AsRun changed the plan it was given")
	}

	r.Inputs.Combination = "nope"
	if _, err := AsRun(plan, r); err == nil {
		t.Error("a combination the plan does not have was accepted")
	}
}
