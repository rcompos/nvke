// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lane

import (
	"bytes"
	"errors"
	"io/fs"
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
