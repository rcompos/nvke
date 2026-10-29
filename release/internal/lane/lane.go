// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package lane turns one stream of a qualification plan into what the
// hardware lane runs: the site-inputs build, the e2e config, and the suite's
// command line. It mirrors the qualify-run job of qualify-e2e.yml, so a run
// from a workstation asks the suite for the same thing a dispatched one does.
package lane

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

// The suite's three clocks, shortest first. Only the Ginkgo timeout unwinds
// and releases the node; the go test timeout panics past teardown. They match
// the run step of qualify-e2e.yml, whose comment sizes the gap between them.
const (
	GinkgoTimeout = "150m"
	GoTestTimeout = "220m"
)

// The stages of the site controller run, as -e2e.site-stage takes them.
var stages = []string{"provision", "bootstrap", "full"}

// Inputs is one stream's entry in the workflow's matrix: everything the plan
// resolved that the lane reads.
type Inputs struct {
	Combination string `json:"combination"`
	Stream      string `json:"stream"`
	Arch        string `json:"arch"`
	Provider    string `json:"provider"`
	GPU         string `json:"gpu,omitempty"`

	Controller                string `json:"controller,omitempty"`
	ControllerVersionStrategy string `json:"controllerVersionStrategy,omitempty"`
	CLI                       string `json:"cli,omitempty"`
	CLIRef                    string `json:"cliRef,omitempty"`
	CAPIProvisioned           string `json:"capiProvisioned"`

	Kube       string `json:"kube"`
	CNIOVN     string `json:"cniOvn"`
	Kind       string `json:"kind"`
	Clusterctl string `json:"clusterctl"`
	Nerdctl    string `json:"nerdctl"`

	OSImage                string `json:"osImage,omitempty"`
	OSImageAMD64           string `json:"osImageAmd64,omitempty"`
	OSImageARM64           string `json:"osImageArm64,omitempty"`
	OSImageBootloaderAMD64 string `json:"osImageBootloaderAmd64,omitempty"`
	OSImageBootloaderARM64 string `json:"osImageBootloaderArm64,omitempty"`
}

// FromPlan reads one stream's inputs out of a plan, the way the plan job's
// "Build the matrix" step does. An empty combination or stream is accepted
// when the plan has exactly one to choose from.
func FromPlan(plan *qualification.Plan, combinationID, streamName string) (Inputs, error) {
	combo, err := pickCombination(plan, combinationID)
	if err != nil {
		return Inputs{}, err
	}
	stream, err := pickStream(combo, streamName)
	if err != nil {
		return Inputs{}, err
	}

	all := make([]qualification.ComponentVersion, 0, len(combo.Components)+len(combo.Dependencies)+len(stream.Dependencies))
	all = append(all, combo.Components...)
	all = append(all, combo.Dependencies...)
	all = append(all, stream.Dependencies...)
	entry := func(name string) qualification.ComponentVersion {
		for _, e := range all {
			if e.Name == name {
				return e
			}
		}
		return qualification.ComponentVersion{}
	}
	value := func(name string) string {
		if e := entry(name); e.URL != "" {
			return e.URL
		}
		return entry(name).Version
	}

	arch := stream.Capabilities["arch"]
	return Inputs{
		Combination:               combo.ID,
		Stream:                    stream.Name,
		Arch:                      arch,
		Provider:                  stream.Capabilities["provider"],
		GPU:                       stream.Capabilities["gpu"],
		Controller:                entry("controller").Version,
		ControllerVersionStrategy: entry("controller").VersionStrategy,
		CLI:                       entry("cli").Version,
		CLIRef:                    entry("cli-" + arch).URL,
		CAPIProvisioned:           entry("capi-provisioned").Version,
		Kube:                      value("kube"),
		CNIOVN:                    value("cni-ovn"),
		Kind:                      value("kind"),
		Clusterctl:                value("clusterctl"),
		Nerdctl:                   value("nerdctl"),
		OSImage:                   entry("os-image").Version,
		OSImageAMD64:              value("os-image-amd64"),
		OSImageARM64:              value("os-image-arm64"),
		OSImageBootloaderAMD64:    value("os-image-bootloader-amd64"),
		OSImageBootloaderARM64:    value("os-image-bootloader-arm64"),
	}, nil
}

func pickCombination(plan *qualification.Plan, id string) (*qualification.Combination, error) {
	if id != "" {
		combo, ok := plan.FindCombination(id)
		if !ok {
			return nil, fmt.Errorf("combination %q not found in the plan", id)
		}
		return combo, nil
	}
	if len(plan.Combinations) != 1 {
		ids := make([]string, 0, len(plan.Combinations))
		for _, c := range plan.Combinations {
			ids = append(ids, c.ID)
		}
		return nil, fmt.Errorf("the plan has %d combinations, so one has to be named: %s", len(plan.Combinations), strings.Join(ids, ", "))
	}
	return &plan.Combinations[0], nil
}

func pickStream(combo *qualification.Combination, name string) (*qualification.Stream, error) {
	if name != "" {
		stream, ok := combo.FindStream(name)
		if !ok {
			return nil, fmt.Errorf("stream %q not found in combination %q; it has %s", name, combo.ID, streamNames(combo))
		}
		return stream, nil
	}
	if len(combo.Streams) != 1 {
		return nil, fmt.Errorf("combination %q has %d streams, so one has to be named: %s", combo.ID, len(combo.Streams), streamNames(combo))
	}
	return &combo.Streams[0], nil
}

func streamNames(combo *qualification.Combination) string {
	names := make([]string, 0, len(combo.Streams))
	for _, s := range combo.Streams {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Options are what a run chooses for itself rather than reads from the plan.
type Options struct {
	// Stage is -e2e.site-stage: provision, bootstrap or full.
	Stage string
	// Registry is where the runner pulls the site images from, ghcr or nvcr.
	Registry string
	// NGCOrg is the org the NVCR images are published under. nvcr needs it.
	NGCOrg string
	// NodeUser is the account the OS image bakes. Empty is the OSS images'.
	NodeUser string
	// KeepSite holds the site node on failure.
	KeepSite bool
	// Head is the checkout's commit, abbreviated to twelve characters. It
	// tags whatever is built from the checkout.
	Head string
	// Build is what to build from the checkout even though the plan names a
	// published version of it.
	Build Build
	// RepoRoot is the checkout, where a built nvkectl lands.
	RepoRoot string
	// SiteInputs is where site-inputs writes nvkectl and the image archives.
	SiteInputs string
}

// Run is one stream resolved for a run, carrying every value a later step
// reads, so no step derives one of its own.
type Run struct {
	Inputs Inputs `json:"inputs"`

	Stage        string `json:"stage"`
	Environment  string `json:"environment"`
	Arch         string `json:"arch"`
	Capabilities string `json:"capabilities"`
	// NodeVersions are the os-image pairs and component versions, keyed as
	// the suite reads them from -e2e.versions.
	NodeVersions string `json:"nodeVersions"`

	// Images is the registry prefix the site images are pulled from.
	Images string `json:"images"`

	ControllerVersion string `json:"controllerVersion"`
	ControllerBuild   bool   `json:"controllerBuild"`
	// ControllerRepository is the controller image's name under Images. Only
	// a published release on GHCR from before the rename differs from the
	// default; see SetControllerRepository.
	ControllerRepository string `json:"controllerRepository"`

	CLIVersion string `json:"cliVersion"`
	CLIBuild   bool   `json:"cliBuild"`
	// CLIRef is the published nvkectl's reference, without its scheme.
	CLIRef string `json:"cliRef,omitempty"`
	// NVKECTL is where the staged nvkectl ends up.
	NVKECTL string `json:"nvkectl"`

	ProvisionedVersion string `json:"provisionedVersion"`
	ProvisionedBuild   bool   `json:"provisionedBuild"`

	NodeUser   string `json:"nodeUser,omitempty"`
	KeepSite   bool   `json:"keepSite"`
	SiteInputs string `json:"siteInputs"`
}

// DefaultControllerRepository is the controller image's name since the rename.
const DefaultControllerRepository = "nvke-controller"

// Resolve checks one stream's inputs and resolves what the run needs, with
// the checks the workflow's resolve steps make before a machine is taken.
func Resolve(in Inputs, o Options) (Run, error) {
	if !contains(stages, o.Stage) {
		return Run{}, fmt.Errorf("stage %q is not one of %s", o.Stage, strings.Join(stages, ", "))
	}
	if o.SiteInputs == "" {
		return Run{}, fmt.Errorf("no site-inputs directory")
	}
	r := Run{
		Inputs:               in,
		Stage:                o.Stage,
		ControllerRepository: DefaultControllerRepository,
		NodeUser:             o.NodeUser,
		KeepSite:             o.KeepSite,
		SiteInputs:           o.SiteInputs,
	}

	r.Arch = in.Arch
	if r.Arch == "" {
		r.Arch = "amd64"
	}
	if r.Arch != "amd64" && r.Arch != "arm64" {
		return Run{}, fmt.Errorf("arch is %s, which is neither amd64 nor arm64", r.Arch)
	}

	r.ControllerVersion, r.ControllerBuild = in.Controller, in.Controller == "" || o.Build.Controller
	if r.ControllerBuild {
		if o.Head == "" {
			return Run{}, fmt.Errorf("the controller is built from the checkout, and the checkout's commit is unknown")
		}
		if r.Arch != "amd64" {
			return Run{}, fmt.Errorf("arch=%s needs a published controller: building the controller image here is amd64-only", r.Arch)
		}
		r.ControllerVersion = o.Head
	}

	r.ProvisionedVersion, r.ProvisionedBuild = in.CAPIProvisioned, o.Build.CAPIProvisioned
	if r.ProvisionedBuild {
		if o.Head == "" {
			return Run{}, fmt.Errorf("capi-provisioned is built from the checkout, and the checkout's commit is unknown")
		}
		r.ProvisionedVersion = o.Head
	}

	switch in.Provider {
	case "forge", "dsx":
		r.Environment = in.Provider
	case "":
		return Run{}, fmt.Errorf("stream %s names no provider capability", in.Stream)
	default:
		return Run{}, fmt.Errorf("provider is %s, which has no environment file", in.Provider)
	}

	r.Capabilities = "arch=" + r.Arch
	if in.GPU != "" {
		r.Capabilities += ",gpu=" + in.GPU
	}

	versions, err := nodeVersions(in)
	if err != nil {
		return Run{}, err
	}
	r.NodeVersions = versions

	switch o.Registry {
	case "ghcr":
		r.Images = "ghcr.io/dsx-ai-factory/nvke"
	case "nvcr":
		if o.NGCOrg == "" {
			return Run{}, fmt.Errorf("image registry nvcr needs the NGC org the images are published under")
		}
		r.Images = "nvcr.io/" + o.NGCOrg
	default:
		return Run{}, fmt.Errorf("image registry %q is not ghcr or nvcr", o.Registry)
	}

	r.CLIVersion, r.CLIBuild = in.CLI, in.CLI == "" || o.Build.CLI
	if r.CLIBuild {
		if o.Head == "" {
			return Run{}, fmt.Errorf("nvkectl is built from the checkout, and the checkout's commit is unknown")
		}
		r.CLIVersion = o.Head
		r.NVKECTL = o.RepoRoot + "/cli/dist/nvkectl-linux-" + r.Arch
	} else {
		if in.CLIRef == "" {
			return Run{}, fmt.Errorf("no cli-%s in the plan, so there is no nvkectl artifact to stage for %s", r.Arch, in.CLI)
		}
		r.CLIRef = strings.TrimPrefix(in.CLIRef, "oci://")
		r.NVKECTL = o.SiteInputs + "/nvkectl/nvkectl-linux-" + r.Arch
	}
	return r, nil
}

// nodeVersions joins every os-image pair present and the five component
// versions the suite requires, failing on a half pair or a missing version.
func nodeVersions(in Inputs) (string, error) {
	var keys []string
	pairs := 0
	for _, p := range []struct{ arch, image, loader string }{
		{"amd64", in.OSImageAMD64, in.OSImageBootloaderAMD64},
		{"arm64", in.OSImageARM64, in.OSImageBootloaderARM64},
	} {
		if p.image == "" && p.loader == "" {
			continue
		}
		if p.image == "" || p.loader == "" {
			return "", fmt.Errorf("os-image-%s and os-image-bootloader-%s go together: a qcow2 needs its imager", p.arch, p.arch)
		}
		keys = append(keys, "os-image-bootloader-"+p.arch+"="+p.loader, "os-image-"+p.arch+"="+p.image)
		pairs++
	}
	if pairs == 0 {
		return "", fmt.Errorf("the plan has no OS image pair for any architecture")
	}
	for _, kv := range []struct{ key, value string }{
		{"kube", in.Kube},
		{"cni-ovn", in.CNIOVN},
		{"kind", in.Kind},
		{"clusterctl", in.Clusterctl},
		{"nerdctl", in.Nerdctl},
	} {
		if kv.value == "" {
			return "", fmt.Errorf("no value for %s: the plan did not resolve one for stream %s", kv.key, in.Stream)
		}
		keys = append(keys, kv.key+"="+kv.value)
	}
	return strings.Join(keys, ","), nil
}

// SetControllerRepository names the controller image under Images.
func (r *Run) SetControllerRepository(name string) { r.ControllerRepository = name }

// ControllerImage is the controller image site-inputs pulls or builds.
func (r Run) ControllerImage() string {
	return r.Images + "/" + r.ControllerRepository + ":" + r.ControllerVersion
}

// ProvisionedImage is the capi-provisioned image site-inputs pulls or builds.
func (r Run) ProvisionedImage() string {
	return r.Images + "/cluster-api-provider-provisioned:" + r.ProvisionedVersion
}

// SiteInputsVars are the make variables the "Build the site inputs" step
// passes to `make -C controller/test/e2e site-inputs`.
func (r Run) SiteInputsVars() []string {
	return []string{
		"SITE_NVKE_CONTROLLER_VERSION=" + r.ControllerVersion,
		"SITE_NVKE_CONTROLLER_BUILD=" + bit(r.ControllerBuild),
		"SITE_NVKE_CONTROLLER_REPOSITORY=" + r.ControllerRepository,
		"SITE_ARCH=" + r.Arch,
		"SITE_CLI_BUILD=" + bit(r.CLIBuild),
		"SITE_CLI_VERSION=" + r.CLIVersion,
		"SITE_IMAGE_REGISTRY=" + r.Images,
		"SITE_PROVISIONED_VERSION=" + r.ProvisionedVersion,
		"SITE_PROVISIONED_BUILD=" + bit(r.ProvisionedBuild),
		"SITE_INPUTS=" + r.SiteInputs,
	}
}

// SiteInputsEnv is the environment site-inputs reads rather than takes as a
// make variable: the published nvkectl's reference.
func (r Run) SiteInputsEnv() []string {
	if r.CLIRef == "" {
		return nil
	}
	return []string{"SITE_CLI_REF=" + r.CLIRef}
}

// E2EConfig is the e2e-config.yaml the "Write the e2e config" step writes:
// where each staged image came from, and nothing else.
func (r Run) E2EConfig() string {
	var b strings.Builder
	fmt.Fprintf(&b, `# Generated by qualify local.
node:
  user: %q
nvkectl:
  binary: %s
site:
  images:
    controller:
      repository: %s/%s
      archive: %s/controller.tar
    capi-provisioned:
      repository: %s/cluster-api-provider-provisioned
      archive: %s/capi-provisioned.tar
    profiles:
      repository: nke-e2e-profiles
      tag: local
      archive: %s/profiles.tar
`, r.NodeUser, r.NVKECTL, r.Images, r.ControllerRepository, r.SiteInputs, r.Images, r.SiteInputs, r.SiteInputs)
	if r.KeepSite {
		b.WriteString("lifecycle:\n  preserveOnFailure: true\n")
	}
	return b.String()
}

// Versions is -e2e.versions: the components under qualification, then the
// node's.
func (r Run) Versions() string {
	return "controller=" + r.ControllerVersion + ",cli=" + r.CLIVersion +
		",capi-provisioned=" + r.ProvisionedVersion + "," + r.NodeVersions
}

// Reports are where the suite writes what it reports.
type Reports struct {
	Config string
	JUnit  string
	JSON   string
}

// GoTestArgs is the run step's `go test` command line, after `go`, run from
// controller/test/e2e. extra is appended to the suite's own arguments.
func (r Run) GoTestArgs(reports Reports, extra []string) []string {
	args := []string{
		"test", "-v", "-timeout", GoTestTimeout, ".", "-args",
		"-ginkgo.label-filter=site-controller",
		"-ginkgo.timeout=" + GinkgoTimeout,
		"-ginkgo.v",
		"-ginkgo.fail-on-empty",
		"-ginkgo.no-color",
		"-ginkgo.junit-report=" + reports.JUnit,
		"-ginkgo.json-report=" + reports.JSON,
		"-e2e.config=" + reports.Config,
		"-e2e.environment=" + r.Environment,
		"-e2e.site-stage=" + r.Stage,
		"-e2e.versions=" + r.Versions(),
		"-e2e.capabilities=" + r.Capabilities,
	}
	if r.KeepSite {
		args = append(args, "-e2e.keep-site")
	}
	return append(args, extra...)
}

func bit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
