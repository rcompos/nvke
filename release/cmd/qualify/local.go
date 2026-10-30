// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/release/internal/artifact"
	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
	"github.com/dsx-ai-factory/nvke/release/internal/resolve"
)

// laneScript holds every step of one stream's run. The workflow calls the
// same steps, so this command adds no logic of its own to a run.
const laneScript = "controller/test/e2e/hack/qualify-lane.sh"

type localOptions struct {
	plan, scope, subject string
	combination, stream  string
	stage, registry      string
	ngcOrg, nodeUser     string
	runDir               string
	build                []string
	keepSite, dryRun     bool
	wait                 time.Duration
}

func localCmd() *cobra.Command {
	var o localOptions
	cmd := &cobra.Command{
		Use:   "local",
		Short: "Run one stream on hardware from this checkout, as the workflow does",
		Long: `Run one stream on hardware from this checkout, as the workflow does.

For a change that has to be proven on hardware from a fork, whose pull request
cannot start the qualification workflow. It runs that workflow's steps for one
stream, from ` + laneScript + `, in the workflow's order:
plan, verify, resolve, site inputs, config, the suite, withhold, result.

Planning describes commits against the upstream repository's tags, fetched
first, from the remote whose URL names ` + resolve.UpstreamRepository + `, so a
clone whose origin is a fork plans what CI would. The CDN refuses workstations,
so the OS images are asked for through the jump host.

What the workflow takes from its environment, this takes from yours: the
Forge and DSX identifiers and keys the environment files read, NGC_API_KEY for
NVCR, GITHUB_ACTOR and GITHUB_TOKEN for GHCR, docker logged in to the
registry the images come from, and a Teleport login (tsh login) in place of
the workflow's bot. FORGE_TELEPORT_PROXY is taken for TELEPORT_PROXY.

Everything the run writes is kept in --run-dir. One interrupt tears the node
down; wait for it.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLocal(cmd.Context(), cmd.OutOrStdout(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.plan, "plan", "", "the qualification plan to read, instead of planning --scope")
	f.StringVar(&o.scope, "scope", "full", "the scope to plan when --plan is not given")
	f.StringVar(&o.subject, "subject", "nvke", "the subject --scope plans")
	f.StringVar(&o.combination, "combination", "", "the combination to run (required if the plan has more than one)")
	f.StringVar(&o.stream, "stream", "", "the stream to run (required)")
	f.StringVar(&o.stage, "stage", "", "how far the site run goes: provision, bootstrap or full (required)")
	f.StringVar(&o.registry, "registry", "nvcr", "where the site images are pulled from: nvcr or ghcr")
	f.StringVar(&o.ngcOrg, "ngc-org", "", "the NGC org the NVCR images are published under (default: $QUALIFY_E2E_NGC_ORG)")
	f.StringVar(&o.nodeUser, "node-user", "", "the account the OS image bakes (default: $QUALIFY_E2E_NODE_USER; empty is the OSS images')")
	f.StringSliceVar(&o.build, "build", nil, "components to build from the checkout instead of pulling: controller, cli")
	f.BoolVar(&o.keepSite, "keep-site", false, "keep the site node when the run fails; it must then be deleted by hand")
	f.BoolVar(&o.dryRun, "dry-run", false, "plan, verify, resolve and stage, and print the suite's arguments without taking a machine")
	f.DurationVar(&o.wait, "wait", 0, "how long to keep asking for an artifact that is not published yet")
	f.StringVar(&o.runDir, "run-dir", "", "where the run's files are kept (default: a new directory under the user cache)")
	_ = cmd.MarkFlagRequired("stream")
	_ = cmd.MarkFlagRequired("stage")
	return cmd
}

func runLocal(ctx context.Context, out io.Writer, o localOptions) error {
	if o.ngcOrg == "" {
		o.ngcOrg = os.Getenv("QUALIFY_E2E_NGC_ORG")
	}
	if o.nodeUser == "" {
		o.nodeUser = os.Getenv("QUALIFY_E2E_NODE_USER")
	}
	for _, b := range o.build {
		if b != "controller" && b != "cli" {
			return fmt.Errorf("--build %q: only controller and cli are built here", b)
		}
	}
	for _, tool := range []string{"bash", "jq", "git", "make", "docker", "tsh"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s is not on PATH; the run needs it", tool)
		}
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	remote, err := resolve.UpstreamRemote(root)
	if err != nil {
		return err
	}
	if o.runDir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		o.runDir = filepath.Join(cache, "nvke", "qualify-local", time.Now().Format("20060102-150405")+"-"+o.stream)
	}
	if err := os.MkdirAll(o.runDir, 0o700); err != nil {
		return err
	}
	fmt.Fprintf(out, "run directory: %s\n", o.runDir)
	startedAt := time.Now().UTC().Format(time.RFC3339)

	planAt := o.plan
	if planAt == "" {
		planAt = filepath.Join(o.runDir, "plan.json")
		if err := planLocal(ctx, out, root, remote, planAt, o); err != nil {
			return err
		}
	}
	plan, err := qualification.LoadPlan(planAt)
	if err != nil {
		return err
	}
	if o.combination == "" && len(plan.Combinations) == 1 {
		o.combination = plan.Combinations[0].ID
	}
	combo, stream, err := qualification.CombinationStream(plan, o.combination, o.stream)
	if err != nil {
		return err
	}
	if err := verifyLocal(ctx, out, combo, stream, o.wait); err != nil {
		return err
	}
	if !o.dryRun {
		// The suite relays through tsh as whoever is logged in, and runs
		// where it cannot prompt for a login that has lapsed.
		status := exec.CommandContext(ctx, "tsh", "status")
		if b, err := status.CombinedOutput(); err != nil {
			return fmt.Errorf("tsh status: %w: %s; run tsh login first", err, strings.TrimSpace(string(b)))
		}
	}

	l := &lane{root: root, out: out, envFile: filepath.Join(o.runDir, "lane.env")}
	l.env = append(os.Environ(),
		"LANE_ENV="+l.envFile,
		"LANE_REMOTE="+remote,
		"E2E_CONFIG="+filepath.Join(o.runDir, "e2e-config.yaml"),
		"SITE_INPUTS="+filepath.Join(o.runDir, "site-inputs"),
		"SITE_STAGE="+o.stage,
		"SITE_IMAGE_REGISTRY="+o.registry,
		"QUALIFY_E2E_NGC_ORG="+o.ngcOrg,
		"NODE_USER="+o.nodeUser,
		fmt.Sprintf("KEEP_SITE=%t", o.keepSite),
		fmt.Sprintf("DRY_RUN=%t", o.dryRun),
	)
	// The workflow takes these from repository variables and secrets, and
	// they are taken from the shell here. Each step refuses a name it reads
	// that is unset, so one added to the script and not here fails at once.
	for _, name := range []string{
		"NGC_API_KEY",
		"QUALIFY_E2E_SITE_CONTROLLER_VERSION",
		"SITE_DEPLOY_PATH_REF",
		"NVKE_CONTROLLER_NAMESPACE",
		"NVKE_CONTROLLER_SERVICE",
	} {
		l.env = append(l.env, name+"="+os.Getenv(name))
	}
	if os.Getenv("TELEPORT_PROXY") == "" && os.Getenv("FORGE_TELEPORT_PROXY") != "" {
		l.env = append(l.env, "TELEPORT_PROXY="+os.Getenv("FORGE_TELEPORT_PROXY"))
	}
	if err := os.WriteFile(l.envFile, nil, 0o600); err != nil {
		return err
	}

	if err := l.step("stream-env", planAt, combo.ID, stream.Name); err != nil {
		return err
	}
	// Built rather than pulled is what an empty version means to the
	// resolve step, as it does for a workflow run that names none.
	for name, version := range map[string]string{"controller": "SITE_NVKE_CONTROLLER_VERSION", "cli": "CLI_VERSION"} {
		if slices.Contains(o.build, name) {
			if err := l.set(version, ""); err != nil {
				return err
			}
		}
	}
	steps := []string{"resolve", "controller-image", "site-inputs"}
	if !o.dryRun {
		steps = append(steps, "config")
	}
	for _, s := range steps {
		if err := l.step(s); err != nil {
			return err
		}
	}

	runErr := l.step("run")
	if err := l.step("withhold"); err != nil {
		return errors.Join(runErr, err)
	}
	if err := l.collect(o.runDir); err != nil {
		return errors.Join(runErr, err)
	}
	if o.dryRun {
		return runErr
	}

	result := resultCmd()
	result.SetOut(out)
	result.SetArgs([]string{
		"--plan", planAt,
		"--report", filepath.Join(o.runDir, "qualify-e2e.report.json"),
		"--combination", combo.ID,
		"--stream", stream.Name,
		"--started-at", startedAt,
		"--output", filepath.Join(o.runDir, "qualify-result.json"),
	})
	if err := result.ExecuteContext(context.WithoutCancel(ctx)); err != nil {
		return errors.Join(runErr, err)
	}
	fmt.Fprintf(out, "result: %s\n", filepath.Join(o.runDir, "qualify-result.json"))
	return runErr
}

// planLocal plans the way the workflow's plan job does, after fetching
// upstream's tags so the versions are named by the tags the publishers see.
func planLocal(ctx context.Context, out io.Writer, root, remote, planAt string, o localOptions) error {
	fmt.Fprintf(out, "fetching %s's tags\n", remote)
	fetch := exec.CommandContext(ctx, "git", "fetch", "--quiet", "--tags", remote)
	fetch.Dir, fetch.Stdout, fetch.Stderr = root, out, out
	if err := fetch.Run(); err != nil {
		return fmt.Errorf("fetching %s's tags: %w", remote, err)
	}
	plan := planCmd()
	plan.SetOut(out)
	plan.SetArgs([]string{"--subject", o.subject, "--scope", o.scope, "--root", root, "--output", planAt})
	if err := plan.ExecuteContext(ctx); err != nil {
		return fmt.Errorf("planning: %w", err)
	}
	return nil
}

// verifyLocal is qualify verify for the one stream, except that the CDN is
// asked from the site: it refuses a workstation with the 403 it gives for
// something that is not there.
func verifyLocal(ctx context.Context, out io.Writer, combo *qualification.Combination, stream *qualification.Stream, wait time.Duration) error {
	one := &qualification.Plan{Combinations: []qualification.Combination{*combo}}
	one.Combinations[0].Streams = []qualification.Stream{*stream}

	var registry, web []artifact.Location
	for _, l := range locations(one) {
		if strings.HasPrefix(l.URL, "https://") {
			web = append(web, l)
		} else {
			registry = append(registry, l)
		}
	}
	var errs []error
	checker := artifact.Checker{Auth: artifact.EnvAuth(registryCredentials...)}
	if len(registry) > 0 {
		if err := checker.WaitFor(ctx, out, registry, wait, artifact.DefaultPoll); err != nil {
			errs = append(errs, err)
		}
	}
	if len(web) > 0 {
		hostVar, host, login := jumpHost()
		if host == "" {
			return errors.Join(append(errs, errors.New("the CDN answers only the sites; set FORGE_JUMP_HOST to ask it from there"))...)
		}
		fmt.Fprintf(out, "asking the CDN from the site, through $%s\n", hostVar)
		var missing []string
		for _, l := range web {
			code, err := fromSite(ctx, host, login, l.URL)
			if err == nil && strings.HasPrefix(code, "2") {
				fmt.Fprintf(out, "published: %s (%s)\n", l.URL, strings.Join(l.Wanted, ", "))
				continue
			}
			reason := "HTTP " + code
			if err != nil {
				reason = strings.ReplaceAll(err.Error(), host, "$"+hostVar)
			}
			missing = append(missing, fmt.Sprintf("  %s (%s): %s from the site", l.URL, strings.Join(l.Wanted, ", "), reason))
		}
		if len(missing) > 0 {
			errs = append(errs, fmt.Errorf("%d of the CDN artifacts the plan names are not published:\n%s", len(missing), strings.Join(missing, "\n")))
		}
	}
	return errors.Join(errs...)
}

// jumpHost is the first site jump host set in the environment, with the
// variable that named it and the Teleport login for it. Any site reaches the
// CDN, so which one does not matter.
func jumpHost() (hostVar, host, login string) {
	for _, s := range [][2]string{
		{"FORGE_JUMP_HOST", "FORGE_TELEPORT_LOGIN"},
		{"FORGE_SITE_2_JUMP_HOST", "FORGE_SITE_2_TELEPORT_LOGIN"},
		{"DSX_DATAHALL_1_JUMP_HOST", "DSX_DATAHALL_1_TELEPORT_LOGIN"},
	} {
		if h := os.Getenv(s[0]); h != "" {
			return s[0], h, os.Getenv(s[1])
		}
	}
	return "", "", ""
}

// fromSite is the HTTP status the jump host gets for a HEAD of url.
func fromSite(ctx context.Context, host, login, url string) (string, error) {
	if strings.ContainsAny(url, "'\\") {
		return "", fmt.Errorf("refusing to quote %q for a remote shell", url)
	}
	target := host
	if login != "" {
		target = login + "@" + host
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "tsh", "ssh", target,
		"curl -s -o /dev/null -w '%{http_code}' -I --max-time 30 '"+url+"'").Output()
	return strings.TrimSpace(string(b)), err
}

// lane runs the steps of qualify-lane.sh, each with what the earlier ones
// resolved, as the workflow's steps read $GITHUB_ENV.
type lane struct {
	root, envFile string
	env           []string
	out           io.Writer
}

func (l *lane) set(name, value string) error {
	f, err := os.OpenFile(l.envFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s=%s\n", name, value)
	return err
}

// step runs one step. The terminal's interrupt reaches it directly, except
// for the suite: that runs in its own process group, so it is interrupted
// once, from here, and gets to tear the node down. Ginkgo skips its cleanup
// on a second interrupt, and an instance abandoned mid-teardown leaks.
func (l *lane) step(args ...string) error {
	resolved, err := readEnvFile(l.envFile)
	if err != nil {
		return err
	}
	cmd := exec.Command("bash", append([]string{laneScript}, args...)...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = l.root, l.out, l.out
	cmd.Env = append(slices.Clone(l.env), resolved...)
	isolated := args[0] == "run"
	if isolated {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else {
		cmd.Stdin = os.Stdin
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	interrupted := false
	for {
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			if interrupted {
				return fmt.Errorf("%s: interrupted", args[0])
			}
			return nil
		case <-signals:
			if !isolated {
				interrupted = true
				_ = cmd.Process.Signal(os.Interrupt)
				continue
			}
			if interrupted {
				fmt.Fprintf(l.out, "\nstill tearing down; kill -9 %d abandons the node, which then has to be released by hand\n", cmd.Process.Pid)
				continue
			}
			interrupted = true
			fmt.Fprintln(l.out, "\ninterrupted; the suite is tearing the node down")
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
		}
	}
}

// collect moves what the suite wrote into dir, after withhold.
func (l *lane) collect(dir string) error {
	cmd := exec.Command("bash", laneScript, "artifacts")
	cmd.Dir, cmd.Stderr = l.root, l.out
	b, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	for _, path := range strings.Fields(string(b)) {
		if err := os.Rename(path, filepath.Join(dir, filepath.Base(path))); err != nil {
			return err
		}
	}
	return nil
}

// readEnvFile reads the NAME=value lines the steps wrote, later ones last so
// that they win.
func readEnvFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var env []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		if strings.Contains(s.Text(), "=") {
			env = append(env, s.Text())
		}
	}
	return env, s.Err()
}
