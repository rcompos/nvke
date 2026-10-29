// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSuiteEnvTakesTheTeleportProxyFromForge(t *testing.T) {
	env := suiteEnv([]string{"FORGE_TELEPORT_PROXY=proxy.example.test:443"})
	if got := firstEnvIn(env, "TELEPORT_PROXY"); got != "proxy.example.test:443" {
		t.Errorf("TELEPORT_PROXY %q", got)
	}
	env = suiteEnv([]string{"FORGE_TELEPORT_PROXY=forge.example.test:443", "TELEPORT_PROXY=mine.example.test:443"})
	if got := firstEnvIn(env, "TELEPORT_PROXY"); got != "mine.example.test:443" {
		t.Errorf("a TELEPORT_PROXY already set was replaced: %q", got)
	}
}

func TestFirstEnvInTakesTheLastAssignment(t *testing.T) {
	env := []string{"A=first", "B=", "A=second"}
	if got := firstEnvIn(env, "B", "A"); got != "second" {
		t.Errorf("got %q", got)
	}
	if got := firstEnvIn(env, "C"); got != "" {
		t.Errorf("unset gave %q", got)
	}
}

func TestMakeWorkDirIsPrivate(t *testing.T) {
	dir, err := makeWorkDir(filepath.Join(t.TempDir(), "run"), "s", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("work directory mode %v", info.Mode().Perm())
	}
}

func TestCollectDiagnosticsMovesWhatTheSuiteLeft(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(from, "tenant-diagnostics.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	moved := collectDiagnostics(&out, from, to)
	if len(moved) != 1 || moved[0] != "tenant-diagnostics.json" {
		t.Fatalf("moved %v", moved)
	}
	if _, err := os.Stat(filepath.Join(to, "tenant-diagnostics.json")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(from, "tenant-diagnostics.json")); err == nil {
		t.Error("left behind in the suite's directory")
	}
}

func TestLocalNeedsAPlanAndAStage(t *testing.T) {
	cmd := root()
	cmd.SetArgs([]string{"local", "--stream", "s"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "plan") || !strings.Contains(err.Error(), "stage") {
		t.Fatalf("want both required flags named, got %v", err)
	}
}
