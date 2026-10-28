// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package artifacts reads the artifact file, the JSON document that names
// every image and chart nvkectl installs. File is the contract: its fields
// are decoded and every other key is dropped, so an unmodified nvkr release
// export and a hand-written file with only outputs both satisfy it. Output
// names follow nvkr's NVKE definition (inputs/definitions/nvke/definition.yaml).
package artifacts

import (
	"encoding/json"
	"fmt"
	"io"
)

const (
	KindImage = "oci"
	KindChart = "helm-oci"
)

type Spec struct {
	Kind     string
	Required bool
}

// Outputs is every output nvkectl reads. nico-prereqs is required when nico
// is present.
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

type File struct {
	RootDigest string     `json:"rootDigest"`
	Outputs    []Output   `json:"outputs"`
	Evidence   []Evidence `json:"evidence"`
}

type Output struct {
	Name         string      `json:"name"`
	Kind         string      `json:"kind"`
	Reference    string      `json:"reference"`
	OS           string      `json:"os"`
	Arch         string      `json:"arch"`
	ImageIndex   *ImageIndex `json:"imageIndex"`
	ChartVersion string      `json:"chartVersion"`
}

type ImageIndex struct {
	Children []ImageIndexChild `json:"children"`
}

type ImageIndexChild struct {
	Platform string `json:"platform"`
}

type Evidence struct {
	Binding EvidenceBinding `json:"binding"`
	Source  *Source         `json:"source"`
}

type EvidenceBinding struct {
	Outputs []string `json:"outputs"`
}

type Source struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
}

// Decode does not validate the content.
func Decode(r io.Reader) (*File, error) {
	var f File
	if err := json.NewDecoder(r).Decode(&f); err != nil {
		return nil, fmt.Errorf("artifact file is not valid JSON: %w", err)
	}
	return &f, nil
}

func (f *File) Output(name string) *Output {
	for i := range f.Outputs {
		if f.Outputs[i].Name == name {
			return &f.Outputs[i]
		}
	}
	return nil
}

// Source returns the source an evidence entry binds to the output, or nil.
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

// Platforms returns the os/arch pairs an image serves, from the image index
// when there is one and from OS and Arch otherwise. A chart has none.
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
