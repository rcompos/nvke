// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lane

import (
	"bytes"
	"errors"
	"io/fs"
	"math/bits"
	"os"
	"regexp"
	"sort"
	"strings"
)

// required matches a ${NAME} reference with no default. The suite's loader
// fails on an unset one; ${NAME:-} is optional.
var required = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// RequiredEnv is every variable an environment file references without a
// default, sorted and without repeats.
func RequiredEnv(environmentFile []byte) []string {
	seen := map[string]bool{}
	var names []string
	for _, m := range required.FindAllSubmatch(environmentFile, -1) {
		name := string(m[1])
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// JumpHost names the variables one site's jump host and its Teleport login
// are read from.
type JumpHost struct {
	HostVar, LoginVar string
}

var jumpHostRef = regexp.MustCompile(`(jumpHost|teleportLogin):\s*"\$\{([A-Za-z_][A-Za-z0-9_]*)`)

// JumpHosts is every site's jump host in an environment file, in order, each
// with the login declared after it.
func JumpHosts(environmentFile []byte) []JumpHost {
	var sites []JumpHost
	for _, m := range jumpHostRef.FindAllSubmatch(environmentFile, -1) {
		switch name := string(m[2]); string(m[1]) {
		case "jumpHost":
			sites = append(sites, JumpHost{HostVar: name})
		case "teleportLogin":
			if len(sites) > 0 && sites[len(sites)-1].LoginVar == "" {
				sites[len(sites)-1].LoginVar = name
			}
		}
	}
	return sites
}

// DescribeAbbrev is the hash length git describe picks on its own in a
// repository of count objects: enough hex digits to expect no collision,
// and never fewer than seven. A clone with more objects than the publishers'
// describes the same commit with a longer name, which names no artifact.
func DescribeAbbrev(count int) int {
	n := (bits.Len(uint(count)) + 1) / 2
	if n < 7 {
		return 7
	}
	return n
}

// DescribeHash is the abbreviated commit in a git describe name such as
// v0.2.0-98-g2cfd0008, and whether there is one.
func DescribeHash(version string) (string, bool) {
	i := strings.LastIndex(version, "-g")
	if i < 0 {
		return "", false
	}
	hash := version[i+2:]
	if len(hash) < 4 || strings.Trim(hash, "0123456789abcdef") != "" {
		return "", false
	}
	return hash, true
}

// Missing is the names in want that environ leaves unset or empty.
func Missing(environ, want []string) []string {
	set := map[string]bool{}
	for _, kv := range environ {
		if name, value, ok := strings.Cut(kv, "="); ok && value != "" {
			set[name] = true
		}
	}
	var missing []string
	for _, name := range want {
		if !set[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// Withheld is every value in environ that must not reach an artifact: each
// *_JUMP_HOST and the SSA issuer, raw and with surrounding space and trailing
// slashes trimmed, the two forms the workflow's withhold step matches.
func Withheld(environ []string) []string {
	seen := map[string]bool{}
	var values []string
	for _, kv := range environ {
		name, raw, ok := strings.Cut(kv, "=")
		if !ok || raw == "" || (!strings.HasSuffix(name, "_JUMP_HOST") && name != "SSA_ISSUER") {
			continue
		}
		for _, v := range []string{raw, strings.TrimRight(strings.TrimSpace(raw), "/")} {
			if v != "" && !seen[v] {
				seen[v] = true
				values = append(values, v)
			}
		}
	}
	// Longest first, so a value that contains another is replaced whole.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

// Withhold replaces every withheld value in each file with <withheld>. A file
// that does not exist is skipped, since which ones a run writes depends on how
// far it got.
func Withhold(paths, values []string) error {
	if len(values) == 0 {
		return nil
	}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		out := b
		for _, v := range values {
			out = bytes.ReplaceAll(out, []byte(v), []byte("<withheld>"))
		}
		if bytes.Equal(out, b) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, out, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}
