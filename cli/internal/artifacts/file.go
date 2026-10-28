// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package artifacts reads the artifact file: the JSON document that names
// every image and chart nvkectl installs. It is the only source of images
// and charts; the stack config names none.
//
// The contract is the type File. nvkectl decodes its fields and drops every
// other key, so where the document comes from does not matter. nvkr release
// export produces one that satisfies the contract unmodified. A build that
// does not use NVKR writes the same fields by hand; the smallest such file
// is testdata/cases/minimal-hand-written.json.
//
// Outputs lists the names nvkectl expects. The output names follow NVKR's
// NVKE definition, inputs/definitions/nvke/definition.yaml in the nvkr
// repository. Outputs with other names are dropped.
//
// Validation runs before anything is installed, in this order: the file is
// JSON; every output name appears once and every required output is
// present, nico-prereqs with nico; every expected output has the kind its
// role needs and a reference with a digest; every image output serves the
// target platform; every chart version is in the range the build supports;
// every reference resolves; every chart renders with the embedded values
// and passes a server-side dry run; the cluster preconditions hold. A
// failure on one output names it as outputs[name=<name>]. testdata/cases.yaml
// lists the expected result for every test file.
package artifacts

import (
	"encoding/json"
	"fmt"
	"io"
)

// Kinds of the outputs nvkectl installs.
const (
	KindImage = "oci"
	KindChart = "helm-oci"
)

// Spec says what nvkectl expects of one named output.
type Spec struct {
	// Kind is KindImage or KindChart.
	Kind string
	// Required outputs must be present. nico and nico-prereqs are optional;
	// nico-prereqs is required when nico is present.
	Required bool
}

// Outputs is the contract: every output name nvkectl reads, with its kind
// and whether it is required.
var Outputs = map[string]Spec{
	"cwe":                 {Kind: KindImage, Required: true},
	"controller-profiles": {Kind: KindImage, Required: true},
	"capnico-controller":  {Kind: KindImage, Required: true},
	"controller-chart":    {Kind: KindChart, Required: true},
	"nktp":                {Kind: KindChart, Required: true},
	"postgresql":          {Kind: KindChart, Required: true},
	"spire-crds":          {Kind: KindChart, Required: true},
	"spire":               {Kind: KindChart, Required: true},
	"temporal":            {Kind: KindChart, Required: true},
	"nico":                {Kind: KindChart},
	"nico-prereqs":        {Kind: KindChart},
}

// File is the subset of the artifact file that nvkectl reads.
type File struct {
	// RootDigest identifies the release the file describes. It is recorded
	// in the install record when present.
	RootDigest string `json:"rootDigest"`
	// Outputs lists the images and charts, one entry per name.
	Outputs []Output `json:"outputs"`
	// Evidence carries the source of an output, when the producer knows it.
	Evidence []Evidence `json:"evidence"`
}

// Output is one image or chart.
type Output struct {
	Name string `json:"name"`
	// Kind is oci for an image and helm-oci for a chart.
	Kind string `json:"kind"`
	// Reference is the digest-pinned OCI reference the install pulls from.
	Reference string `json:"reference"`
	// OS and Arch name the platform of a single-platform image.
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// ImageIndex lists the platforms of a multi-platform image.
	ImageIndex *ImageIndex `json:"imageIndex"`
	// ChartVersion is set on a chart.
	ChartVersion string `json:"chartVersion"`
}

// ImageIndex is the platform membership of a multi-platform image.
type ImageIndex struct {
	Children []ImageIndexChild `json:"children"`
}

// ImageIndexChild is one platform of a multi-platform image.
type ImageIndexChild struct {
	// Platform is os/arch, for example linux/amd64.
	Platform string `json:"platform"`
}

// Evidence binds a source to the outputs it describes.
type Evidence struct {
	Binding EvidenceBinding `json:"binding"`
	Source  *Source         `json:"source"`
}

// EvidenceBinding names the outputs an evidence entry describes.
type EvidenceBinding struct {
	Outputs []string `json:"outputs"`
}

// Source is the checkout an output was built from.
type Source struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
}

// Decode reads an artifact file from r. It accepts any JSON object and keeps
// the fields of File; unknown keys are dropped. Decode does not validate the
// content.
func Decode(r io.Reader) (*File, error) {
	var f File
	if err := json.NewDecoder(r).Decode(&f); err != nil {
		return nil, fmt.Errorf("artifact file is not valid JSON: %w", err)
	}
	return &f, nil
}

// Output returns the output with the given name, or nil.
func (f *File) Output(name string) *Output {
	for i := range f.Outputs {
		if f.Outputs[i].Name == name {
			return &f.Outputs[i]
		}
	}
	return nil
}

// Source returns the source bound to the named output, or nil when no
// evidence entry carries one.
func (f *File) Source(name string) *Source {
	for _, e := range f.Evidence {
		if e.Source == nil {
			continue
		}
		for _, o := range e.Binding.Outputs {
			if o == name {
				return e.Source
			}
		}
	}
	return nil
}

// Platforms returns the os/arch platforms an image output serves: the
// single platform from OS and Arch, or every child of the image index. A
// chart has none.
func (o *Output) Platforms() []string {
	if o.ImageIndex != nil {
		platforms := make([]string, 0, len(o.ImageIndex.Children))
		for _, c := range o.ImageIndex.Children {
			platforms = append(platforms, c.Platform)
		}
		return platforms
	}
	if o.OS != "" && o.Arch != "" {
		return []string{o.OS + "/" + o.Arch}
	}
	return nil
}
