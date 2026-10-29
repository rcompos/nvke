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

func TestLocalNeedsAStage(t *testing.T) {
	cmd := root()
	cmd.SetArgs([]string{"local", "--stream", "s"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "stage") || strings.Contains(err.Error(), "plan") {
		t.Fatalf("want only --stage required, got %v", err)
	}
}

func TestWithGitConfigAddsToWhatIsSet(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "someone")
	restore := withGitConfig("core.abbrev", "8")
	if os.Getenv("GIT_CONFIG_COUNT") != "2" || os.Getenv("GIT_CONFIG_KEY_1") != "core.abbrev" || os.Getenv("GIT_CONFIG_VALUE_1") != "8" {
		t.Fatalf("set %q %q=%q", os.Getenv("GIT_CONFIG_COUNT"), os.Getenv("GIT_CONFIG_KEY_1"), os.Getenv("GIT_CONFIG_VALUE_1"))
	}
	restore()
	if os.Getenv("GIT_CONFIG_COUNT") != "1" || os.Getenv("GIT_CONFIG_KEY_1") != "" || os.Getenv("GIT_CONFIG_KEY_0") != "user.name" {
		t.Errorf("not restored: count %q", os.Getenv("GIT_CONFIG_COUNT"))
	}
}

// fakeTSH puts a tsh on PATH whose status lists logins and whose ssh reaches
// only reachable as login, failing the way tsh does, with the host named.
func fakeTSH(t *testing.T, validFor time.Duration, logins, reachable string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
status) printf '{"active":{"valid_until":"%s","logins":[%s]}}' "$VALID" "$LOGINS" ;;
ssh) [ "$2" = "$REACHABLE" ] && exit 0; echo "ERROR: access denied to $2 connecting to ${2#*@}" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tsh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("VALID", time.Now().Add(validFor).UTC().Format(time.RFC3339))
	t.Setenv("LOGINS", logins)
	t.Setenv("REACHABLE", reachable)
}

const twoSites = `
sites:
  - jumpHost: "${FORGE_JUMP_HOST:-}"
    teleportLogin: "${FORGE_TELEPORT_LOGIN:-}"
  - jumpHost: "${FORGE_SITE_2_JUMP_HOST:-}"
    teleportLogin: "${FORGE_SITE_2_TELEPORT_LOGIN:-}"
`

func TestCheckTeleportReachesTheJumpHostAsItsLogin(t *testing.T) {
	fakeTSH(t, 4*time.Hour, `"me","root"`, "me@jump-1.example.test")
	var c checks
	c.checkTeleport(t.Context(), os.Environ(), []byte(twoSites), "full")
	env := append(os.Environ(), "FORGE_JUMP_HOST=jump-1.example.test", "FORGE_TELEPORT_LOGIN=me")
	c.checkTeleport(t.Context(), env, []byte(twoSites), "full")
	if len(c.problems) != 1 || !strings.Contains(c.problems[0], "no jump host is set") {
		t.Fatalf("problems %q", c.problems)
	}
	if c.site == nil || c.site.target() != "me@jump-1.example.test" || len(c.warnings) != 0 {
		t.Errorf("site %+v warnings %q", c.site, c.warnings)
	}
}

func TestCheckTeleportNamesTheVariableNotTheHost(t *testing.T) {
	fakeTSH(t, 30*time.Minute, `"me"`, "nobody")
	env := append(os.Environ(),
		"FORGE_JUMP_HOST=jump-1.example.test", "FORGE_TELEPORT_LOGIN=me@example.test",
		"FORGE_SITE_2_JUMP_HOST=jump-2.example.test", "FORGE_SITE_2_TELEPORT_LOGIN=me",
	)
	var c checks
	c.checkTeleport(t.Context(), env, []byte(twoSites), "full")
	all := strings.Join(append(c.problems, c.warnings...), "\n")
	for _, want := range []string{"FORGE_TELEPORT_LOGIN is \"me@example.test\", which is not one of your Teleport logins (me)", "$FORGE_SITE_2_JUMP_HOST is not reachable", "Teleport login ends in"} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q in\n%s", want, all)
		}
	}
	if strings.Contains(all, "jump-1.example.test") || strings.Contains(all, "jump-2.example.test") {
		t.Errorf("a jump host is named:\n%s", all)
	}
	if c.site != nil {
		t.Errorf("site %+v from no host reached", c.site)
	}
}
