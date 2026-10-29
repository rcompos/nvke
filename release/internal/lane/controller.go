// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lane

import (
	"errors"
	"fmt"

	"sigs.k8s.io/yaml"
)

// ControllerReleasePath is the deploy path's controller release, relative to
// the repository root. Its release namespace is the name the controller is
// installed under, which the suite reads as NVKE_CONTROLLER_NAMESPACE and
// NVKE_CONTROLLER_SERVICE.
const ControllerReleasePath = "deploy/nico/controller/release.yaml"

// ControllerNameVars are every variable the suite reads the controller's name
// from, the deprecated ones included, so a name set in any of them is left
// to the suite.
var ControllerNameVars = []string{
	"NVKE_CONTROLLER_NAMESPACE", "NVKE_CONTROLLER_SERVICE",
	"NKE_CONTROLLER_NAMESPACE", "NKE_CONTROLLER_SERVICE",
	"CWE_NAMESPACE", "CWE_SERVICE",
}

// ControllerNameEnv is the suite's environment for a controller installed as
// name, as the workflow's controller-name input sets it.
func ControllerNameEnv(name string) []string {
	return []string{"NVKE_CONTROLLER_NAMESPACE=" + name, "NVKE_CONTROLLER_SERVICE=" + name}
}

// ReleaseNamespace is the release namespace a controller release file names.
func ReleaseNamespace(release []byte) (string, error) {
	var r struct {
		Release struct {
			Namespace string `json:"namespace"`
		} `json:"release"`
	}
	if err := yaml.Unmarshal(release, &r); err != nil {
		return "", fmt.Errorf("parsing: %w", err)
	}
	if r.Release.Namespace == "" {
		return "", errors.New("names no release namespace")
	}
	return r.Release.Namespace, nil
}
