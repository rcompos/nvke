// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lane

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

func testPlan() *qualification.Plan {
	stream := func(name, arch, gpu string) qualification.Stream {
		caps := map[string]string{"arch": arch, "provider": "forge"}
		if gpu != "" {
			caps["gpu"] = gpu
		}
		return qualification.Stream{
			Name:         name,
			Capabilities: caps,
			Dependencies: []qualification.ComponentVersion{
				{Name: "os-image-amd64", Version: "v0.9.0", URL: "https://example.test/amd64.qcow2"},
				{Name: "os-image-bootloader-amd64", URL: "https://example.test/amd64.imager"},
				{Name: "os-image-arm64", URL: "https://example.test/arm64.qcow2"},
				{Name: "os-image-bootloader-arm64", URL: "https://example.test/arm64.imager"},
				{Name: "kube", Version: "1.36.2"},
			},
		}
	}
	return &qualification.Plan{
		Subject: "nvke",
		Version: "v0.23.0-beta.11",
		Combinations: []qualification.Combination{{
			ID: "combo-1",
			Components: []qualification.ComponentVersion{
				{Name: "controller", Version: "v0.2.0-98-gabc", VersionStrategy: "commitDescribe"},
				{Name: "cli", Version: "v0.3.0-rc.2-67-gabc"},
				{Name: "capi-provisioned", Version: "v0.2.0-98-gabc"},
				{Name: "os-image", Version: "v0.9.0"},
			},
			Dependencies: []qualification.ComponentVersion{
				{Name: "cli-amd64", Version: "v0.3.0-rc.2-67-gabc", URL: "oci://nvcr.io/org/nvkectl:amd64"},
				{Name: "cli-arm64", Version: "v0.3.0-rc.2-67-gabc", URL: "oci://nvcr.io/org/nvkectl:arm64"},
				{Name: "cni-ovn", Version: "v1.2.0"},
				{Name: "kind", Version: "v0.31.0"},
				{Name: "clusterctl", Version: "v1.12.0"},
				{Name: "nerdctl", Version: "2.2.0"},
			},
			Streams: []qualification.Stream{
				stream("forge-gb200-arm64", "arm64", "gb200"),
				stream("forge-cpu-amd64", "amd64", ""),
			},
		}},
	}
}

func TestFromPlanReadsTheMatrixEntry(t *testing.T) {
	in, err := FromPlan(testPlan(), "", "forge-gb200-arm64")
	if err != nil {
		t.Fatal(err)
	}
	want := Inputs{
		Combination:               "combo-1",
		Stream:                    "forge-gb200-arm64",
		Arch:                      "arm64",
		Provider:                  "forge",
		GPU:                       "gb200",
		Controller:                "v0.2.0-98-gabc",
		ControllerVersionStrategy: "commitDescribe",
		CLI:                       "v0.3.0-rc.2-67-gabc",
		// The cli entry for the stream's own architecture.
		CLIRef:          "oci://nvcr.io/org/nvkectl:arm64",
		CAPIProvisioned: "v0.2.0-98-gabc",
		Kube:            "1.36.2",
		CNIOVN:          "v1.2.0",
		Kind:            "v0.31.0",
		Clusterctl:      "v1.12.0",
		Nerdctl:         "2.2.0",
		OSImage:         "v0.9.0",
		// A url wins over a version, as the matrix's value() has it.
		OSImageAMD64:           "https://example.test/amd64.qcow2",
		OSImageARM64:           "https://example.test/arm64.qcow2",
		OSImageBootloaderAMD64: "https://example.test/amd64.imager",
		OSImageBootloaderARM64: "https://example.test/arm64.imager",
	}
	if in != want {
		t.Errorf("FromPlan:\n got %+v\nwant %+v", in, want)
	}
}

func TestFromPlanNeedsAStreamWhenThereIsAChoice(t *testing.T) {
	_, err := FromPlan(testPlan(), "", "")
	if err == nil || !strings.Contains(err.Error(), "forge-cpu-amd64, forge-gb200-arm64") {
		t.Fatalf("want an error naming both streams, got %v", err)
	}
	if _, err := FromPlan(testPlan(), "", "nope"); err == nil {
		t.Fatal("an unknown stream was accepted")
	}
	if _, err := FromPlan(testPlan(), "nope", "forge-cpu-amd64"); err == nil {
		t.Fatal("an unknown combination was accepted")
	}
}

func options(dir string) Options {
	return Options{Stage: "provision", Registry: "nvcr", NGCOrg: "org", Head: "0123456789ab", RepoRoot: "/repo", SiteInputs: dir}
}

func TestResolvePublishedComponents(t *testing.T) {
	in, _ := FromPlan(testPlan(), "", "forge-gb200-arm64")
	r, err := Resolve(in, options("/inputs"))
	if err != nil {
		t.Fatal(err)
	}
	if r.ControllerBuild || r.CLIBuild {
		t.Errorf("published components are not built: controller %v cli %v", r.ControllerBuild, r.CLIBuild)
	}
	if r.Capabilities != "arch=arm64,gpu=gb200" {
		t.Errorf("capabilities %q", r.Capabilities)
	}
	if r.CLIRef != "nvcr.io/org/nvkectl:arm64" {
		t.Errorf("cli ref keeps its scheme: %q", r.CLIRef)
	}
	if r.NVKECTL != "/inputs/nvkectl/nvkectl-linux-arm64" {
		t.Errorf("nvkectl %q", r.NVKECTL)
	}
	if r.ControllerImage() != "nvcr.io/org/nvke-controller:v0.2.0-98-gabc" {
		t.Errorf("controller image %q", r.ControllerImage())
	}
	wantVersions := "controller=v0.2.0-98-gabc,cli=v0.3.0-rc.2-67-gabc,capi-provisioned=v0.2.0-98-gabc," +
		"os-image-bootloader-amd64=https://example.test/amd64.imager,os-image-amd64=https://example.test/amd64.qcow2," +
		"os-image-bootloader-arm64=https://example.test/arm64.imager,os-image-arm64=https://example.test/arm64.qcow2," +
		"kube=1.36.2,cni-ovn=v1.2.0,kind=v0.31.0,clusterctl=v1.12.0,nerdctl=2.2.0"
	if r.Versions() != wantVersions {
		t.Errorf("versions:\n got %s\nwant %s", r.Versions(), wantVersions)
	}
}

func TestResolveBuildsWhatThePlanLeavesOpen(t *testing.T) {
	in, _ := FromPlan(testPlan(), "", "forge-cpu-amd64")
	in.Controller, in.CLI = "", ""
	r, err := Resolve(in, options("/inputs"))
	if err != nil {
		t.Fatal(err)
	}
	if !r.ControllerBuild || r.ControllerVersion != "0123456789ab" {
		t.Errorf("controller: build %v version %q", r.ControllerBuild, r.ControllerVersion)
	}
	if !r.CLIBuild || r.CLIVersion != "0123456789ab" || r.NVKECTL != "/repo/cli/dist/nvkectl-linux-amd64" {
		t.Errorf("cli: build %v version %q nvkectl %q", r.CLIBuild, r.CLIVersion, r.NVKECTL)
	}
	if r.SiteInputsEnv() != nil {
		t.Errorf("a built nvkectl has no reference: %v", r.SiteInputsEnv())
	}
}

func TestResolveRefusesBeforeAMachineIsTaken(t *testing.T) {
	arm, _ := FromPlan(testPlan(), "", "forge-gb200-arm64")
	for name, tc := range map[string]struct {
		mutate func(*Inputs, *Options)
		want   string
	}{
		"unknown stage":          {func(_ *Inputs, o *Options) { o.Stage = "tenant" }, "stage"},
		"arm64 controller build": {func(i *Inputs, _ *Options) { i.Controller = "" }, "amd64-only"},
		"bad arch":               {func(i *Inputs, _ *Options) { i.Arch = "riscv64" }, "neither amd64 nor arm64"},
		"no provider":            {func(i *Inputs, _ *Options) { i.Provider = "" }, "no provider"},
		"unknown provider":       {func(i *Inputs, _ *Options) { i.Provider = "aws" }, "no environment file"},
		"half os pair":           {func(i *Inputs, _ *Options) { i.OSImageBootloaderARM64 = "" }, "go together"},
		"no os pair": {func(i *Inputs, _ *Options) {
			i.OSImageAMD64, i.OSImageBootloaderAMD64, i.OSImageARM64, i.OSImageBootloaderARM64 = "", "", "", ""
		}, "no OS image pair"},
		"missing nerdctl":       {func(i *Inputs, _ *Options) { i.Nerdctl = "" }, "nerdctl"},
		"nvcr without org":      {func(_ *Inputs, o *Options) { o.NGCOrg = "" }, "NGC org"},
		"unknown registry":      {func(_ *Inputs, o *Options) { o.Registry = "quay" }, "not ghcr or nvcr"},
		"published cli, no ref": {func(i *Inputs, _ *Options) { i.CLIRef = "" }, "no cli-arm64"},
	} {
		t.Run(name, func(t *testing.T) {
			in, o := arm, options("/inputs")
			tc.mutate(&in, &o)
			_, err := Resolve(in, o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSiteInputsVars(t *testing.T) {
	in, _ := FromPlan(testPlan(), "", "forge-gb200-arm64")
	r, _ := Resolve(in, options("/inputs"))
	want := []string{
		"SITE_NVKE_CONTROLLER_VERSION=v0.2.0-98-gabc",
		"SITE_NVKE_CONTROLLER_BUILD=0",
		"SITE_NVKE_CONTROLLER_REPOSITORY=nvke-controller",
		"SITE_ARCH=arm64",
		"SITE_CLI_BUILD=0",
		"SITE_CLI_VERSION=v0.3.0-rc.2-67-gabc",
		"SITE_IMAGE_REGISTRY=nvcr.io/org",
		"SITE_PROVISIONED_VERSION=v0.2.0-98-gabc",
		"SITE_INPUTS=/inputs",
	}
	if got := r.SiteInputsVars(); !reflect.DeepEqual(got, want) {
		t.Errorf("site-inputs vars:\n got %v\nwant %v", got, want)
	}
	if got := r.SiteInputsEnv(); !reflect.DeepEqual(got, []string{"SITE_CLI_REF=nvcr.io/org/nvkectl:arm64"}) {
		t.Errorf("site-inputs env %v", got)
	}
}

func TestE2EConfig(t *testing.T) {
	in, _ := FromPlan(testPlan(), "", "forge-gb200-arm64")
	o := options("/inputs")
	o.NodeUser = "ubuntu"
	r, _ := Resolve(in, o)
	r.SetControllerRepository("nke-controller")
	want := `# Generated by qualify local.
node:
  user: "ubuntu"
nvkectl:
  binary: /inputs/nvkectl/nvkectl-linux-arm64
site:
  images:
    controller:
      repository: nvcr.io/org/nke-controller
      archive: /inputs/controller.tar
    capi-provisioned:
      repository: nvcr.io/org/cluster-api-provider-provisioned
      archive: /inputs/capi-provisioned.tar
    profiles:
      repository: nke-e2e-profiles
      tag: local
      archive: /inputs/profiles.tar
`
	if got := r.E2EConfig(); got != want {
		t.Errorf("config:\n%s\nwant:\n%s", got, want)
	}

	o.KeepSite = true
	r, _ = Resolve(in, o)
	if !strings.HasSuffix(r.E2EConfig(), "lifecycle:\n  preserveOnFailure: true\n") {
		t.Errorf("keep-site does not preserve on failure:\n%s", r.E2EConfig())
	}
}

func TestGoTestArgs(t *testing.T) {
	in, _ := FromPlan(testPlan(), "", "forge-cpu-amd64")
	o := options("/inputs")
	o.KeepSite = true
	r, _ := Resolve(in, o)
	args := r.GoTestArgs(Reports{Config: "/w/e2e-config.yaml", JUnit: "/w/j.xml", JSON: "/w/r.json"}, []string{"-e2e.extra"})

	head := []string{"test", "-v", "-timeout", "220m", ".", "-args"}
	if !reflect.DeepEqual(args[:len(head)], head) {
		t.Errorf("go test head %v", args[:len(head)])
	}
	for _, want := range []string{
		"-ginkgo.label-filter=site-controller", "-ginkgo.timeout=150m", "-ginkgo.fail-on-empty", "-ginkgo.no-color",
		"-ginkgo.junit-report=/w/j.xml", "-ginkgo.json-report=/w/r.json", "-e2e.config=/w/e2e-config.yaml",
		"-e2e.environment=forge", "-e2e.site-stage=provision", "-e2e.capabilities=arch=amd64", "-e2e.versions=" + r.Versions(),
		"-e2e.keep-site",
	} {
		if !contains(args, want) {
			t.Errorf("missing %s in %v", want, args)
		}
	}
	if args[len(args)-1] != "-e2e.extra" {
		t.Errorf("extra arguments go last: %v", args)
	}
}

func TestRequiredEnv(t *testing.T) {
	file := []byte(`id: "${FORGE_ORG_ID}"
site: "${FORGE_SITE_ID}"
again: "${FORGE_ORG_ID}"
jump: "${FORGE_JUMP_HOST:-}"
`)
	if got := RequiredEnv(file); !reflect.DeepEqual(got, []string{"FORGE_ORG_ID", "FORGE_SITE_ID"}) {
		t.Errorf("required %v", got)
	}
	if got := Missing([]string{"FORGE_ORG_ID=x", "FORGE_SITE_ID="}, []string{"FORGE_ORG_ID", "FORGE_SITE_ID", "NGC_API_KEY"}); !reflect.DeepEqual(got, []string{"FORGE_SITE_ID", "NGC_API_KEY"}) {
		t.Errorf("missing %v", got)
	}
}

// The real environment files are what a run checks against, so every
// variable they require has to parse.
func TestRequiredEnvReadsTheForgeEnvironment(t *testing.T) {
	b, err := os.ReadFile("../../../controller/test/e2e/environments/forge.yaml")
	if err != nil {
		t.Skip(err)
	}
	got := RequiredEnv(b)
	if !contains(got, "FORGE_ORG_ID") || contains(got, "FORGE_JUMP_HOST") {
		t.Errorf("forge.yaml requires %v", got)
	}
}

func TestWithhold(t *testing.T) {
	values := Withheld([]string{
		"FORGE_JUMP_HOST=jump.example.test",
		"FORGE_SITE_2_JUMP_HOST=",
		"SSA_ISSUER= https://issuer.example.test/ ",
		"FORGE_ORG_ID=not-withheld",
	})
	for _, want := range []string{"jump.example.test", "https://issuer.example.test"} {
		if !contains(values, want) {
			t.Errorf("%q is not withheld: %v", want, values)
		}
	}
	if contains(values, "not-withheld") || contains(values, "") {
		t.Errorf("withheld too much: %q", values)
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "qualify-e2e.log")
	if err := os.WriteFile(log, []byte("dial jump.example.test failed; issuer https://issuer.example.test/token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Withhold([]string{log, filepath.Join(dir, "absent.json")}, values); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if got := string(b); got != "dial <withheld> failed; issuer <withheld>/token\n" {
		t.Errorf("withheld log %q", got)
	}
	if info, _ := os.Stat(log); info.Mode().Perm() != 0o600 {
		t.Errorf("mode changed to %v", info.Mode().Perm())
	}
}
