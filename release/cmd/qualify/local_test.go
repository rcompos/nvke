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
	err := runLocal(context.Background(), io.Discard, localOptions{build: []string{"capi-provisioned"}})
	if err == nil || !strings.Contains(err.Error(), "only controller and cli") {
		t.Errorf("runLocal(--build capi-provisioned) = %v, want it refused", err)
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
