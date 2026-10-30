// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The steps append, so a value resolved twice is read back as the last one,
// which is what $GITHUB_ENV does in the workflow.
func TestReadEnvFileKeepsTheOrderWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lane.env")
	if err := os.WriteFile(path, []byte("CLI_VERSION=v1\nnot a value\nSITE_ARCH=amd64\nCLI_VERSION=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"CLI_VERSION=v1", "SITE_ARCH=amd64", "CLI_VERSION="}; !slices.Equal(got, want) {
		t.Errorf("readEnvFile = %q, want %q", got, want)
	}
}

func TestLocalBuildsOnlyWhatTheLaneCanBuild(t *testing.T) {
	err := runLocal(context.Background(), io.Discard, localOptions{build: []string{"os-image"}})
	if err == nil || !strings.Contains(err.Error(), "only controller, cli, capi-provisioned") {
		t.Errorf("runLocal(--build os-image) = %v, want it refused", err)
	}
}

func TestResolveBuild(t *testing.T) {
	all := []string{"controller", "cli", "capi-provisioned"}
	for _, tc := range []struct {
		name                string
		in                  localOptions
		wantBuild           []string
		wantRegistry, wantE string
	}{
		{name: "the default pulls from nvcr", in: localOptions{}, wantRegistry: "nvcr"},
		{name: "a build keeps the registry it names", in: localOptions{build: []string{"capi-provisioned"}, registry: "ghcr"}, wantBuild: []string{"capi-provisioned"}, wantRegistry: "ghcr"},
		{name: "from source builds all and names them locally", in: localOptions{fromSource: true}, wantBuild: all, wantRegistry: "local"},
		{name: "from source refuses a registry to pull from", in: localOptions{fromSource: true, registry: "nvcr"}, wantE: "pulls nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.in
			err := o.resolveBuild()
			if tc.wantE != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantE) {
					t.Fatalf("resolveBuild = %v, want %q", err, tc.wantE)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(o.build, tc.wantBuild) || o.registry != tc.wantRegistry {
				t.Errorf("resolveBuild = build %q registry %q, want %q %q", o.build, o.registry, tc.wantBuild, tc.wantRegistry)
			}
		})
	}
}

func TestCheckPlanInputs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    localOptions
		wantE string
	}{
		{name: "nothing chosen", in: localOptions{}},
		{name: "a version and a pulled override", in: localOptions{version: "v1.0.0", overrides: "os-image-arm64=https://example.com/a.qcow2,controller=v1.0.0"}},
		{name: "a version with a plan to read", in: localOptions{plan: "plan.json", version: "v1.0.0"}, wantE: "--plan skips planning"},
		{name: "overrides with a plan to read", in: localOptions{plan: "plan.json", overrides: "kube=1.36.3"}, wantE: "--plan skips planning"},
		{name: "a malformed override", in: localOptions{overrides: "kube"}, wantE: "not name=version"},
		{name: "an override of a built component", in: localOptions{build: []string{"cli"}, overrides: "cli-arm64=v1.0.0"}, wantE: "cli-arm64 is built from this checkout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.checkPlanInputs()
			if tc.wantE == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantE) {
				t.Fatalf("checkPlanInputs = %v, want %q", err, tc.wantE)
			}
		})
	}
}

// A build stands in for every plan entry of that component, and only those:
// a location still holding something pulled is still asked about.
func TestPlannedCoversWhatIsBuilt(t *testing.T) {
	o := localOptions{build: []string{"cli", "capi-provisioned"}}
	got := o.planned()
	if want := []string{"cli", "cli-amd64", "cli-arm64", "capi-provisioned"}; !slices.Equal(got, want) {
		t.Errorf("planned = %q, want %q", got, want)
	}
	if !subset([]string{"cli-amd64"}, got) || subset([]string{"cli-amd64", "controller"}, got) {
		t.Errorf("subset: a location is skipped only when everything it holds is built")
	}
}

func TestJumpHostTakesTheFirstSiteSet(t *testing.T) {
	t.Setenv("FORGE_JUMP_HOST", "")
	t.Setenv("FORGE_SITE_2_JUMP_HOST", "two.example")
	t.Setenv("FORGE_SITE_2_TELEPORT_LOGIN", "root")
	t.Setenv("DSX_DATAHALL_1_JUMP_HOST", "dsx.example")
	if v, h, l := jumpHost(); v != "FORGE_SITE_2_JUMP_HOST" || h != "two.example" || l != "root" {
		t.Errorf("jumpHost = %q, %q, %q; want site 2's", v, h, l)
	}
}
