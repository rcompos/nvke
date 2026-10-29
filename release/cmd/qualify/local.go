// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/release/internal/artifact"
	"github.com/dsx-ai-factory/nvke/release/internal/lane"
	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

// e2eDir is the suite's package, relative to the repository root.
const e2eDir = "controller/test/e2e"

// leftInE2EDir are what the suite writes into its own directory rather than
// where it is told. A run moves them into its work directory, so one already
// there belongs to an earlier run and would be filed under this one.
var leftInE2EDir = []string{"tenant-diagnostics.json", "nvkectl-diagnostics.tar.gz", "site-cluster-state.txt"}

type localOptions struct {
	plan, combination, stream string
	stage, registry, ngcOrg   string
	nodeUser, remote          string
	workDir, siteInputs       string
	keepSite, dryRun          bool
	reuseSiteInputs           bool
	skipVerify                bool
	wait                      time.Duration
	suiteArgs                 []string
}

func localCmd() *cobra.Command {
	var o localOptions
	cmd := &cobra.Command{
		Use:   "local",
		Short: "Run one stream of a plan on the hardware lane from this workstation",
		Long: `Run one stream of a plan on the hardware lane from this workstation.

Does what one leg of the qualify-e2e workflow does, with the credentials in
this shell instead of the workflow's: checks every artifact the plan names,
builds the site inputs, writes the e2e config, and runs the site controller
suite with the workflow's flags and timeouts. Run it from the root of the
checkout under test.

Before any machine is taken it checks what the run needs and names every gap
at once:

  FORGE_API_KEY            the Forge provider's credential
  NGC_API_KEY              for --registry nvcr, with a docker login to nvcr.io
  every ${NAME} the stream's environments/<provider>.yaml requires
  tsh logged in            unless TELEPORT_IDENTITY_FILE or JUMP_HOST_PASSWORD is set
  go, git, make, docker with buildx, and oras when nvkectl is published

Everything the run writes goes to --work-dir, outside the checkout: the e2e
config, the site inputs, the log, the JUnit and JSON reports, the suite's
diagnostics, the resolved inputs and the qualification result. Jump hosts and
the SSA issuer are withheld from the text files there, as the workflow
withholds them from its artifacts. The result is recorded, never published.

Interrupt with one Ctrl-C. Ginkgo then runs its cleanup, which is what
releases the node; a second interrupt skips that cleanup and leaks it.

--dry-run stops after the checks and the config, printing the commands it
would run, and takes no machine.`,
		Example: `  go run ./release/cmd/qualify plan --subject nvke --output plan.json
  go run ./release/cmd/qualify local --plan plan.json --stream forge-gb200-arm64-ubuntu-24.04-kube-1.36 --stage provision --dry-run
  go run ./release/cmd/qualify local --plan plan.json --stream forge-gb200-arm64-ubuntu-24.04-kube-1.36 --stage full --suite-arg=-ginkgo.focus=tenant`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLocal(cmd.Context(), cmd.OutOrStdout(), o)
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.plan, "plan", "", "the plan to run from (required)")
	f.StringVar(&o.combination, "combination", "", "the combination to run (required if the plan has more than one)")
	f.StringVar(&o.stream, "stream", "", "the stream to run (required if the combination has more than one)")
	f.StringVar(&o.stage, "stage", "", "how far the site run goes: provision, bootstrap or full (required)")
	f.StringVar(&o.registry, "registry", "nvcr", "where the site images are pulled from: nvcr or ghcr")
	f.StringVar(&o.ngcOrg, "ngc-org", os.Getenv("QUALIFY_E2E_NGC_ORG"), "the NGC org the NVCR images are published under")
	f.StringVar(&o.nodeUser, "node-user", firstEnv("QUALIFY_E2E_NODE_USER", "NODE_USER"), "the account the OS image bakes; empty is the OSS images'")
	f.StringVar(&o.remote, "remote", "origin", "the remote a missing controller commit or tag is fetched from")
	f.StringVar(&o.workDir, "work-dir", "", "where the run writes everything (default: a new directory under the user cache)")
	f.StringVar(&o.siteInputs, "site-inputs", "", "where the site inputs are built (default: site-inputs in the work directory)")
	f.BoolVar(&o.reuseSiteInputs, "reuse-site-inputs", false, "use the archives already in --site-inputs instead of building them")
	f.BoolVar(&o.keepSite, "keep-site", false, "keep the site node when the run fails; it must then be deleted by hand")
	f.BoolVar(&o.dryRun, "dry-run", false, "check and print what would run, and take no machine")
	f.BoolVar(&o.skipVerify, "skip-verify", false, "do not check the plan's artifacts, for re-running a plan that has already passed the check")
	f.DurationVar(&o.wait, "wait", 0, "how long to wait for an artifact the plan names that is not published yet")
	f.StringArrayVar(&o.suiteArgs, "suite-arg", nil, "an extra argument for the suite, after the workflow's own (repeatable)")
	for _, name := range []string{"plan", "stage"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

func runLocal(ctx context.Context, out io.Writer, o localOptions) error {
	startedAt := time.Now().UTC()
	step := func(format string, args ...any) { fmt.Fprintf(out, "\n==> "+format+"\n", args...) }

	root, err := gitOutput(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("run from inside the checkout under test: %w", err)
	}
	if _, err := os.Stat(filepath.Join(root, e2eDir, "suite_test.go")); err != nil {
		return fmt.Errorf("%s has no %s; run from the root of an nvke checkout", root, e2eDir)
	}
	head, err := gitOutput(ctx, root, "rev-parse", "--short=12", "HEAD")
	if err != nil {
		return err
	}

	plan, err := qualification.LoadPlan(o.plan)
	if err != nil {
		return fmt.Errorf("--plan: %w", err)
	}
	inputs, err := lane.FromPlan(plan, o.combination, o.stream)
	if err != nil {
		return err
	}

	workDir, err := makeWorkDir(o.workDir, inputs.Stream, startedAt)
	if err != nil {
		return err
	}
	siteInputs := o.siteInputs
	if siteInputs == "" {
		siteInputs = filepath.Join(workDir, "site-inputs")
	}
	if siteInputs, err = filepath.Abs(siteInputs); err != nil {
		return err
	}

	r, err := lane.Resolve(inputs, lane.Options{
		Stage:      o.stage,
		Registry:   o.registry,
		NGCOrg:     o.ngcOrg,
		NodeUser:   o.nodeUser,
		KeepSite:   o.keepSite,
		Head:       head,
		RepoRoot:   root,
		SiteInputs: siteInputs,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stream %s at stage %s, from %s\n", r.Inputs.Stream, r.Stage, root)
	fmt.Fprintf(out, "work directory %s\n", workDir)

	env := suiteEnv(os.Environ())

	step("Checking what the run needs")
	if problems := preflight(ctx, root, r, env, o); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(out, "  -", p)
		}
		return fmt.Errorf("%d problem(s) to fix before a machine is taken", len(problems))
	}
	fmt.Fprintln(out, "  ok")

	if o.registry == "ghcr" && !r.ControllerBuild {
		step("Resolving the controller image name")
		name, err := ghcrControllerRepository(ctx, r)
		if err != nil {
			return err
		}
		r.SetControllerRepository(name)
	}
	fmt.Fprintf(out, "controller image %s\n", r.ControllerImage())

	if o.skipVerify {
		fmt.Fprintln(out, "\nnot checking the plan's artifacts (--skip-verify)")
	} else {
		step("Checking every artifact the plan names")
		checker := artifact.Checker{Auth: artifact.EnvAuth(registryCredentials...)}
		if err := checker.WaitFor(ctx, out, locations(plan), o.wait, artifact.DefaultPoll); err != nil {
			return err
		}
	}

	if !r.ControllerBuild {
		step("Making the controller's tree available")
		if err := ensureControllerTree(ctx, out, root, o.remote, r, plan.Commit); err != nil {
			return err
		}
	}

	reports := lane.Reports{
		Config: filepath.Join(workDir, "e2e-config.yaml"),
		JUnit:  filepath.Join(workDir, "qualify-e2e.junit.xml"),
		JSON:   filepath.Join(workDir, "qualify-e2e.report.json"),
	}
	if err := os.WriteFile(reports.Config, []byte(r.E2EConfig()), 0o600); err != nil {
		return err
	}
	if err := writeResolved(filepath.Join(workDir, "resolved-inputs.json"), o, r, root, head, startedAt); err != nil {
		return err
	}
	if err := copyFile(o.plan, filepath.Join(workDir, "plan.json")); err != nil {
		return err
	}

	makeArgs := append([]string{"-C", filepath.Join(root, e2eDir), "site-inputs"}, r.SiteInputsVars()...)
	goArgs := r.GoTestArgs(reports, o.suiteArgs)
	if o.dryRun {
		step("Dry run: no machine is taken. It would run")
		if !o.reuseSiteInputs {
			fmt.Fprintf(out, "  %smake %s\n", envPrefix(r.SiteInputsEnv()), strings.Join(makeArgs, " "))
		}
		fmt.Fprintf(out, "  (cd %s && go %s)\n", filepath.Join(root, e2eDir), strings.Join(goArgs, " "))
		fmt.Fprintf(out, "\nconfig: %s\n", reports.Config)
		return nil
	}

	if o.reuseSiteInputs {
		step("Reusing the site inputs in %s", siteInputs)
		if err := haveSiteInputs(r); err != nil {
			return err
		}
	} else {
		step("Building the site inputs")
		if err := runCommand(out, "", append(os.Environ(), r.SiteInputsEnv()...), "make", makeArgs...); err != nil {
			return fmt.Errorf("site-inputs: %w", err)
		}
	}

	if r.KeepSite {
		fmt.Fprintln(out, "\nwarning: --keep-site is set; the site node, and a failed tenant's nodes, survive a failure and must be deleted by hand")
	}
	stopAwake := keepAwake(out)
	defer stopAwake()

	step("Running the e2e suite")
	logPath := filepath.Join(workDir, "qualify-e2e.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	suiteErr := runCommand(io.MultiWriter(out, logFile), filepath.Join(root, e2eDir), env, "go", goArgs...)
	logFile.Close()

	step("Collecting what the run wrote")
	kept := collectDiagnostics(out, filepath.Join(root, e2eDir), workDir)
	text := []string{logPath, reports.JUnit, reports.JSON}
	for _, name := range kept {
		if !strings.HasSuffix(name, ".tar.gz") {
			text = append(text, filepath.Join(workDir, name))
		}
	}
	if err := lane.Withhold(text, lane.Withheld(env)); err != nil {
		return fmt.Errorf("withholding credentials from the run's files: %w", err)
	}

	step("Recording the qualification result")
	result := resultCmd()
	result.SetOut(out)
	result.SetArgs([]string{
		"--plan", o.plan,
		"--report", reports.JSON,
		"--combination", r.Inputs.Combination,
		"--stream", r.Inputs.Stream,
		"--started-at", startedAt.Format(time.RFC3339),
		"--output", filepath.Join(workDir, "qualify-result.json"),
	})
	if err := result.ExecuteContext(context.WithoutCancel(ctx)); err != nil {
		fmt.Fprintln(out, "  recording the result failed:", err)
	}

	fmt.Fprintf(out, "\neverything this run wrote is in %s\n", workDir)
	if suiteErr != nil {
		fmt.Fprintln(out, "the suite failed. If it was killed before its teardown, its Forge instances may still exist:")
		fmt.Fprintln(out, "the site node is labelled purpose=nke-e2e-site-controller.")
		return fmt.Errorf("the e2e suite: %w", suiteErr)
	}
	return nil
}

// preflight is every check that costs seconds and would otherwise fail an
// hour into a run, all of them, so one pass names every gap.
func preflight(ctx context.Context, root string, r lane.Run, env []string, o localOptions) []string {
	var problems []string
	addf := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	tools := []string{"go", "git", "make", "docker"}
	if r.CLIRef != "" {
		tools = append(tools, "oras")
	}
	teleport := firstEnvIn(env, "JUMP_HOST_PASSWORD") == "" && firstEnvIn(env, "TELEPORT_IDENTITY_FILE") == ""
	if teleport && !o.dryRun {
		tools = append(tools, "tsh")
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			addf("%s is not on PATH", tool)
		}
	}

	if r.Inputs.Provider == "forge" {
		if names := lane.Missing(env, []string{"FORGE_API_KEY"}); len(names) > 0 {
			addf("FORGE_API_KEY is not set")
		}
	}
	if o.registry == "nvcr" {
		if names := lane.Missing(env, []string{"NGC_API_KEY"}); len(names) > 0 {
			addf("NGC_API_KEY is not set; the plan's NVCR artifacts cannot be checked")
		}
	}
	envFile := filepath.Join(root, e2eDir, "environments", r.Environment+".yaml")
	if b, err := os.ReadFile(envFile); err != nil {
		addf("reading %s: %v", envFile, err)
	} else if names := lane.Missing(env, lane.RequiredEnv(b)); len(names) > 0 {
		addf("environments/%s.yaml needs %s", r.Environment, strings.Join(names, ", "))
	}

	for _, name := range leftInE2EDir {
		if _, err := os.Stat(filepath.Join(root, e2eDir, name)); err == nil {
			addf("%s/%s is left from an earlier run; move it out of the way", e2eDir, name)
		}
	}

	if !o.dryRun {
		if err := quiet(ctx, "docker", "info"); err != nil {
			addf("docker is not running: %v", err)
		} else if err := quiet(ctx, "docker", "buildx", "version"); err != nil {
			addf("docker buildx is not installed: %v", err)
		} else if !o.reuseSiteInputs {
			// A docker login, not the NGC_API_KEY verify uses: site-inputs pulls
			// with docker's own credentials.
			if err := quiet(ctx, "docker", "manifest", "inspect", r.ProvisionedImage()); err != nil {
				addf("docker cannot read %s; log in to %s first", r.ProvisionedImage(), strings.SplitN(r.Images, "/", 2)[0])
			}
		}
		if teleport {
			if err := quiet(ctx, "tsh", "status"); err != nil {
				addf("tsh has no valid login; run tsh login --proxy=$TELEPORT_PROXY")
			}
		}
	}
	return problems
}

// suiteEnv is the shell's environment with TELEPORT_PROXY taken from
// FORGE_TELEPORT_PROXY when only the latter is set. tsh reads TELEPORT_PROXY,
// and the dialer uses the logged-in profile when TELEPORT_IDENTITY_FILE is
// unset, which is how a person's run differs from the workflow's bot.
func suiteEnv(environ []string) []string {
	env := append([]string(nil), environ...)
	if firstEnvIn(env, "TELEPORT_PROXY") == "" {
		if proxy := firstEnvIn(env, "FORGE_TELEPORT_PROXY"); proxy != "" {
			env = append(env, "TELEPORT_PROXY="+proxy)
		}
	}
	return env
}

// ghcrControllerRepository finds which name GHCR keeps a published controller
// under: releases from before the rename are nke-controller.
func ghcrControllerRepository(ctx context.Context, r lane.Run) (string, error) {
	for _, name := range []string{lane.DefaultControllerRepository, "nke-controller"} {
		image := r.Images + "/" + name + ":" + r.ControllerVersion
		if quiet(ctx, "docker", "manifest", "inspect", image) == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("%s is on GHCR as neither nvke-controller nor nke-controller", r.ControllerVersion)
}

// ensureControllerTree makes the tree the deploy path is exported from
// present in the checkout: the commit a described version was taken at, or
// the controller's release tag.
func ensureControllerTree(ctx context.Context, out io.Writer, root, remote string, r lane.Run, commit string) error {
	if r.Inputs.ControllerVersionStrategy == "commitDescribe" {
		if commit == "" {
			return fmt.Errorf("the controller was described at a commit, but the plan records none")
		}
		if _, err := gitOutput(ctx, root, "cat-file", "-e", commit+"^{commit}"); err == nil {
			fmt.Fprintf(out, "  have %s\n", commit)
			return nil
		}
		return runCommand(out, root, nil, "git", "fetch", "--no-tags", remote, commit)
	}
	tag := "refs/tags/controller/" + r.ControllerVersion
	if _, err := gitOutput(ctx, root, "rev-parse", "-q", "--verify", tag); err == nil {
		fmt.Fprintf(out, "  have %s\n", tag)
		return nil
	}
	if err := runCommand(out, root, nil, "git", "fetch", "--no-tags", remote, tag+":"+tag); err != nil {
		return fmt.Errorf("no %s on %s; the deploy path is exported from the controller's own commit: %w", tag, remote, err)
	}
	return nil
}

func haveSiteInputs(r lane.Run) error {
	want := []string{"controller.tar", "capi-provisioned.tar", "profiles.tar"}
	var missing []string
	for _, name := range want {
		if _, err := os.Stat(filepath.Join(r.SiteInputs, name)); err != nil {
			missing = append(missing, name)
		}
	}
	if _, err := os.Stat(r.NVKECTL); err != nil {
		missing = append(missing, r.NVKECTL)
	}
	if len(missing) > 0 {
		return fmt.Errorf("--reuse-site-inputs: %s has no %s", r.SiteInputs, strings.Join(missing, ", "))
	}
	return nil
}

// collectDiagnostics moves what the suite left in its directory into the
// work directory, and returns the names it moved.
func collectDiagnostics(out io.Writer, from, to string) []string {
	var moved []string
	for _, name := range leftInE2EDir {
		src := filepath.Join(from, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Rename(src, filepath.Join(to, name)); err != nil {
			fmt.Fprintf(out, "  could not move %s: %v\n", src, err)
			continue
		}
		moved = append(moved, name)
	}
	return moved
}

func makeWorkDir(dir, stream string, at time.Time) (string, error) {
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("no user cache directory, so --work-dir is required: %w", err)
		}
		dir = filepath.Join(cache, "nvke-qualify", stream+"-"+at.Format("20060102T150405Z"))
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	// 0700: the reports carry remote command output.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, os.Chmod(dir, 0o700)
}

// writeResolved records what the run resolved and from what, so a run can say
// what it qualified after its terminal is gone. It holds no credential.
func writeResolved(path string, o localOptions, r lane.Run, root, head string, at time.Time) error {
	record := struct {
		StartedAt string   `json:"startedAt"`
		Checkout  string   `json:"checkout"`
		Head      string   `json:"head"`
		Plan      string   `json:"plan"`
		Registry  string   `json:"registry"`
		SuiteArgs []string `json:"suiteArgs,omitempty"`
		Run       lane.Run `json:"run"`
	}{at.Format(time.RFC3339), root, head, o.plan, o.registry, o.suiteArgs, r}
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// keepAwake holds off sleep on macOS for as long as this process lives: a
// sleeping laptop drops the relay and the run with it.
func keepAwake(out io.Writer) func() {
	if runtime.GOOS != "darwin" {
		return func() {}
	}
	path, err := exec.LookPath("caffeinate")
	if err != nil {
		return func() {}
	}
	cmd := exec.Command(path, "-ims", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(out, "  caffeinate did not start; keep the machine awake:", err)
		return func() {}
	}
	fmt.Fprintln(out, "  caffeinate is holding off sleep; closing the lid on battery still sleeps")
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// runCommand runs a command with its output on out. It is not tied to a
// context: an interrupt reaches the child from the terminal, and Ginkgo needs
// to see exactly one to run its cleanup.
func runCommand(out io.Writer, dir string, env []string, name string, args ...string) error {
	fmt.Fprintf(out, "$ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

func quiet(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		if line := firstLine(string(b)); line != "" {
			return fmt.Errorf("%w: %s", err, line)
		}
		return err
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", args[0], firstLine(string(exit.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(b)), nil
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o600)
}

func envPrefix(env []string) string {
	if len(env) == 0 {
		return ""
	}
	return strings.Join(env, " ") + " "
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func firstEnv(names ...string) string {
	return firstEnvIn(os.Environ(), names...)
}

// firstEnvIn is the value of the first of names set non-empty in environ.
func firstEnvIn(environ []string, names ...string) string {
	for _, name := range names {
		prefix := name + "="
		for i := len(environ) - 1; i >= 0; i-- {
			if v, ok := strings.CutPrefix(environ[i], prefix); ok && v != "" {
				return v
			}
		}
	}
	return ""
}
