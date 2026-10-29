// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
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
	"github.com/dsx-ai-factory/nvke/release/internal/resolve"
)

// e2eDir is the suite's package, relative to the repository root.
const e2eDir = "controller/test/e2e"

// leftInE2EDir are what the suite writes into its own directory rather than
// where it is told. A run moves them into its work directory, so one already
// there belongs to an earlier run and would be filed under this one.
var leftInE2EDir = []string{"tenant-diagnostics.json", "nvkectl-diagnostics.tar.gz", "site-cluster-state.txt"}

// stageLength is roughly how long each stage runs, site inputs included. A
// Teleport login that ends sooner is worth renewing first.
var stageLength = map[string]time.Duration{
	"provision": 45 * time.Minute,
	"bootstrap": 90 * time.Minute,
	"full":      3 * time.Hour,
}

type localOptions struct {
	plan, scope, version, resolution string
	combination, stream              string
	stage, registry, ngcOrg          string
	nodeUser, remote, controllerName string
	workDir, siteInputs              string
	keepSite, dryRun                 bool
	reuseSiteInputs, skipVerify      bool
	wait                             time.Duration
	suiteArgs                        []string
}

func localCmd() *cobra.Command {
	var o localOptions
	cmd := &cobra.Command{
		Use:   "local",
		Short: "Run one stream on the hardware lane from this workstation",
		Long: `Run one stream on the hardware lane from this workstation.

Does what one leg of the qualify-e2e workflow does, with the credentials in
this shell instead of the workflow's: plans, checks every artifact the plan
names, builds the site inputs, writes the e2e config, and runs the site
controller suite with the workflow's flags and timeouts. Run it from the root
of the checkout under test.

Without --plan it plans for itself, from --scope and --version, describing
each commit at the length a fresh clone of --remote does. That is the clone
the publishers tag from; a clone with more objects, forks fetched for one,
describes the same commit with a longer name that names no artifact.

Before any machine is taken it checks what the run needs and names every gap
at once:

  FORGE_API_KEY            the Forge provider's credential
  NGC_API_KEY              for --registry nvcr
  every ${NAME} the stream's environments/<provider>.yaml requires
  docker                   running, with buildx, and able to read the images
                           the run pulls: for nvcr.io, an NGC key made in the
                           org they are published under
  tsh                      logged in, unless TELEPORT_IDENTITY_FILE or
                           JUMP_HOST_PASSWORD is set; every jump host set is
                           reached as its login, which is one of the Logins
                           tsh status lists, not the Teleport user
  go, git, make, and oras when nvkectl is published

The CDN that holds the OS images refuses networks it does not allow, so those
locations are asked from the site, through the first jump host reached. The
registry is asked from here.

Everything the run writes goes to --work-dir, outside the checkout: the plan,
the e2e config, the site inputs, the log, the JUnit and JSON reports, the
suite's diagnostics, the resolved inputs and the qualification result. Jump
hosts and the SSA issuer are withheld from the text files there, as the
workflow withholds them from its artifacts. The result is recorded, never
published.

Interrupt with one Ctrl-C. Ginkgo then runs its cleanup, which is what
releases the node; a second interrupt skips that cleanup and leaks it.

--dry-run makes every check, prints the commands it would run, and takes no
machine.`,
		Example: `  qualify local --scope l40s --stage provision --dry-run
  qualify local --scope l40s --stage provision
  qualify local --scope golden-path --stream forge-gb200-arm64-ubuntu-24.04-kube-1.36 --stage full
  qualify local --plan plan.json --stage full --suite-arg=-ginkgo.focus=tenant`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLocal(cmd.Context(), cmd.OutOrStdout(), o)
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.plan, "plan", "", "a plan to run from, instead of planning here")
	f.StringVar(&o.scope, "scope", "golden-path", "the scope to plan, a scope from release/policy.yaml")
	f.StringVar(&o.version, "version", "", "the nvke version to plan (default: the highest)")
	f.StringVar(&o.resolution, "resolution", resolve.VersionStrategyCommitDescribe, "how the plan resolves components: commitDescribe or gitTag")
	f.StringVar(&o.combination, "combination", "", "the combination to run (required if the plan has more than one)")
	f.StringVar(&o.stream, "stream", "", "the stream to run (required if the combination has more than one)")
	f.StringVar(&o.stage, "stage", "", "how far the site run goes: provision, bootstrap or full (required)")
	f.StringVar(&o.registry, "registry", "nvcr", "where the site images are pulled from: nvcr or ghcr")
	f.StringVar(&o.ngcOrg, "ngc-org", os.Getenv("QUALIFY_E2E_NGC_ORG"), "the NGC org the NVCR images are published under")
	f.StringVar(&o.nodeUser, "node-user", firstEnv("QUALIFY_E2E_NODE_USER", "NODE_USER"), "the account the OS image bakes; empty is the OSS images'")
	f.StringVar(&o.controllerName, "controller-name", "", "the namespace and Service the deploy path installs the controller under (default: read from the controller's deploy path)")
	f.StringVar(&o.remote, "remote", "origin", "the upstream remote: tags and a missing controller commit come from it, and its size sets the describe length")
	f.StringVar(&o.workDir, "work-dir", "", "where the run writes everything (default: a new directory under the user cache)")
	f.StringVar(&o.siteInputs, "site-inputs", "", "where the site inputs are built (default: site-inputs in the work directory)")
	f.BoolVar(&o.reuseSiteInputs, "reuse-site-inputs", false, "use the archives already in --site-inputs instead of building them")
	f.BoolVar(&o.keepSite, "keep-site", false, "keep the site node when the run fails; it must then be deleted by hand")
	f.BoolVar(&o.dryRun, "dry-run", false, "make every check and print what would run, and take no machine")
	f.BoolVar(&o.skipVerify, "skip-verify", false, "do not check the plan's artifacts, for re-running a plan that has already passed the check")
	f.DurationVar(&o.wait, "wait", 0, "how long to wait for a registry artifact the plan names that is not published yet")
	f.StringArrayVar(&o.suiteArgs, "suite-arg", nil, "an extra argument for the suite, after the workflow's own (repeatable)")
	_ = cmd.MarkFlagRequired("stage")
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
	abbrev, err := remoteAbbrev(ctx, root, o.remote)
	if err != nil {
		return err
	}

	planPath := o.plan
	if planPath == "" {
		step("Planning %s", o.scope)
		if planPath, err = planHere(ctx, out, root, abbrev, o); err != nil {
			return err
		}
		defer os.Remove(planPath)
	}
	plan, err := qualification.LoadPlan(planPath)
	if err != nil {
		return fmt.Errorf("the plan: %w", err)
	}
	inputs, err := lane.FromPlan(plan, o.combination, o.stream)
	if err != nil {
		return err
	}

	workDir, err := makeWorkDir(o.workDir, inputs.Stream, startedAt)
	if err != nil {
		return err
	}
	keptPlan := filepath.Join(workDir, "plan.json")
	if err := copyFile(planPath, keptPlan); err != nil {
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
	fmt.Fprintf(out, "\nnvke %s: controller %s, nvkectl %s\n", plan.Version, r.ControllerVersion, r.Inputs.CLI)
	fmt.Fprintf(out, "stream %s at stage %s, from %s\n", r.Inputs.Stream, r.Stage, root)
	fmt.Fprintf(out, "work directory %s\n", workDir)

	env := suiteEnv(os.Environ())

	step("Checking what the run needs")
	c := preflight(ctx, root, r, env, abbrev, o)
	for _, w := range c.warnings {
		fmt.Fprintln(out, "  warning:", w)
	}
	if len(c.problems) > 0 {
		for _, p := range c.problems {
			fmt.Fprintln(out, "  -", p)
		}
		return fmt.Errorf("%d problem(s) to fix before a machine is taken", len(c.problems))
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
		if err := verifyLocal(ctx, out, env, plan, c.site, o.wait); err != nil {
			return err
		}
	}

	treeRef := "HEAD"
	if !r.ControllerBuild {
		step("Making the controller's tree available")
		if err := ensureControllerTree(ctx, out, root, o.remote, r, plan.Commit); err != nil {
			return err
		}
		treeRef = controllerTreeRef(r, plan.Commit)
	}
	name, from, err := controllerName(ctx, root, treeRef, env, o.controllerName)
	if err != nil {
		return err
	}
	if name != "" {
		env = append(env, lane.ControllerNameEnv(name)...)
		fmt.Fprintf(out, "controller installed as %s (%s)\n", name, from)
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

	makeArgs := append([]string{"-C", filepath.Join(root, e2eDir), "site-inputs"}, r.SiteInputsVars()...)
	goArgs := r.GoTestArgs(reports, o.suiteArgs)
	if o.dryRun {
		step("Dry run: no machine is taken. It would run")
		if !o.reuseSiteInputs {
			fmt.Fprintf(out, "  %smake %s\n", envPrefix(r.SiteInputsEnv()), strings.Join(makeArgs, " "))
		}
		var suiteVars []string
		if name != "" {
			suiteVars = lane.ControllerNameEnv(name)
		}
		fmt.Fprintf(out, "  (cd %s && %sgo %s)\n", filepath.Join(root, e2eDir), envPrefix(suiteVars), strings.Join(goArgs, " "))
		fmt.Fprintf(out, "\nplan and config: %s\n", workDir)
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
	resultPath := filepath.Join(workDir, "qualify-result.json")
	result := resultCmd()
	result.SetOut(out)
	result.SetArgs(append([]string{
		"--plan", keptPlan,
		"--report", reports.JSON,
		"--combination", r.Inputs.Combination,
		"--stream", r.Inputs.Stream,
		"--started-at", startedAt.Format(time.RFC3339),
		"--output", resultPath,
	}, localIdentity(context.WithoutCancel(ctx), root, o.remote)...))
	if err := result.ExecuteContext(context.WithoutCancel(ctx)); err != nil {
		fmt.Fprintln(out, "  recording the result failed:", err)
	}
	// Ginkgo and qualify result write 0644; the reports carry remote output.
	for _, path := range []string{reports.JUnit, reports.JSON, resultPath} {
		if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(out, "  could not make %s private: %v\n", path, err)
		}
	}

	fmt.Fprintf(out, "\neverything this run wrote is in %s\n", workDir)
	if suiteErr != nil {
		fmt.Fprintln(out, "the suite failed. If it was killed before its teardown, its Forge instances may still exist:")
		fmt.Fprintln(out, "the site node is labelled purpose=nke-e2e-site-controller.")
		return fmt.Errorf("the e2e suite: %w", suiteErr)
	}
	return nil
}

// planHere runs `qualify plan` for the checkout, describing at abbrev, after
// fetching the remote's tags so the highest version is the remote's.
func planHere(ctx context.Context, out io.Writer, root string, abbrev int, o localOptions) (string, error) {
	if err := runCommand(out, root, nil, "git", "fetch", "--quiet", "--tags", o.remote); err != nil {
		return "", fmt.Errorf("fetching %s's tags: %w", o.remote, err)
	}
	fmt.Fprintf(out, "describing commits at %d characters, as a fresh clone of %s does\n", abbrev, o.remote)

	f, err := os.CreateTemp("", "qualify-plan-*.json")
	if err != nil {
		return "", err
	}
	f.Close()
	args := []string{"--subject", "nvke", "--scope", o.scope, "--resolution", o.resolution, "--root", root, "--output", f.Name()}
	if o.version != "" {
		args = append(args, "--version", o.version)
	}

	restore := withGitConfig("core.abbrev", strconv.Itoa(abbrev))
	defer restore()
	plan := planCmd()
	plan.SetOut(out)
	plan.SetErr(out)
	plan.SetArgs(args)
	if err := plan.ExecuteContext(ctx); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("planning: %w", err)
	}
	return f.Name(), nil
}

// remoteAbbrev is the describe length a fresh clone of remote picks, from
// the objects its branches and the tags reach.
func remoteAbbrev(ctx context.Context, root, remote string) (int, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-list", "--objects", "--remotes="+remote, "--tags")
	cmd.Dir = root
	b, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("counting %s's objects: %w", remote, err)
	}
	count := bytes.Count(b, []byte("\n"))
	if count == 0 {
		return 0, fmt.Errorf("no objects reachable from %s; is it the upstream remote, and fetched?", remote)
	}
	return lane.DescribeAbbrev(count), nil
}

// withGitConfig sets one git config value for every git this process runs,
// through git's own environment, and returns what undoes it.
func withGitConfig(key, value string) func() {
	prev, had := os.LookupEnv("GIT_CONFIG_COUNT")
	n, _ := strconv.Atoi(prev)
	keyVar, valueVar := fmt.Sprintf("GIT_CONFIG_KEY_%d", n), fmt.Sprintf("GIT_CONFIG_VALUE_%d", n)
	os.Setenv(keyVar, key)
	os.Setenv(valueVar, value)
	os.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(n+1))
	return func() {
		os.Unsetenv(keyVar)
		os.Unsetenv(valueVar)
		if had {
			os.Setenv("GIT_CONFIG_COUNT", prev)
		} else {
			os.Unsetenv("GIT_CONFIG_COUNT")
		}
	}
}

// site is a jump host the run reached, and the login it reached it as.
type site struct {
	hostVar, host, login string
}

func (s site) target() string {
	if s.login == "" {
		return s.host
	}
	return s.login + "@" + s.host
}

type checks struct {
	problems, warnings []string
	// site is the first jump host reached, which the CDN checks go through.
	site *site
}

// preflight is every check that costs seconds and would otherwise fail an
// hour into a run, all of them, so one pass names every gap.
func preflight(ctx context.Context, root string, r lane.Run, env []string, abbrev int, o localOptions) checks {
	var c checks
	problem := func(format string, args ...any) { c.problems = append(c.problems, fmt.Sprintf(format, args...)) }

	teleport := firstEnvIn(env, "JUMP_HOST_PASSWORD") == "" && firstEnvIn(env, "TELEPORT_IDENTITY_FILE") == ""
	tools := []string{"go", "git", "make", "docker"}
	if r.CLIRef != "" {
		tools = append(tools, "oras")
	}
	if teleport {
		tools = append(tools, "tsh")
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			problem("%s is not on PATH", tool)
		}
	}

	if r.Inputs.Provider == "forge" && len(lane.Missing(env, []string{"FORGE_API_KEY"})) > 0 {
		problem("FORGE_API_KEY is not set")
	}
	if o.registry == "nvcr" && len(lane.Missing(env, []string{"NGC_API_KEY"})) > 0 {
		problem("NGC_API_KEY is not set; the plan's NVCR artifacts cannot be checked")
	}
	envFile, err := os.ReadFile(filepath.Join(root, e2eDir, "environments", r.Environment+".yaml"))
	if err != nil {
		problem("reading environments/%s.yaml: %v", r.Environment, err)
	} else if names := lane.Missing(env, lane.RequiredEnv(envFile)); len(names) > 0 {
		problem("environments/%s.yaml needs %s", r.Environment, strings.Join(names, ", "))
	}

	for _, name := range leftInE2EDir {
		if _, err := os.Stat(filepath.Join(root, e2eDir, name)); err == nil {
			problem("%s/%s is left from an earlier run; move it out of the way", e2eDir, name)
		}
	}

	if o.plan != "" && r.Inputs.ControllerVersionStrategy == resolve.VersionStrategyCommitDescribe {
		if hash, ok := lane.DescribeHash(r.ControllerVersion); ok && len(hash) != abbrev {
			problem("the plan names controller %s, a %d-character hash, but a fresh clone of %s describes at %d, which is what the publishers tag; plan here instead (drop --plan)",
				r.ControllerVersion, len(hash), o.remote, abbrev)
		}
	}

	if err := quiet(ctx, nil, "docker", "info"); err != nil {
		problem("docker is not running: %v", err)
	} else {
		if err := quiet(ctx, nil, "docker", "buildx", "version"); err != nil {
			problem("docker buildx is not installed: %v", err)
		}
		if !o.reuseSiteInputs {
			images := []string{r.ProvisionedImage()}
			if !r.ControllerBuild && o.registry == "nvcr" {
				images = append([]string{r.ControllerImage()}, images...)
			}
			for _, image := range images {
				if msg := dockerCannotRead(ctx, image); msg != "" {
					problem("%s", msg)
				}
			}
		}
	}

	if teleport && envFile != nil {
		c.checkTeleport(ctx, env, envFile, r.Stage)
	}
	return c
}

// dockerCannotRead says why docker cannot read image's manifest, or "" when
// it can. site-inputs pulls with docker's own login, not NGC_API_KEY.
func dockerCannotRead(ctx context.Context, image string) string {
	err := quiet(ctx, nil, "docker", "manifest", "inspect", image)
	switch {
	case err == nil:
		return ""
	case strings.Contains(err.Error(), "denied") || strings.Contains(err.Error(), "unauthorized"):
		return fmt.Sprintf("docker may not read %s. Log in with a key that can: for nvcr.io, an NGC key made in the org the images are published under", image)
	case strings.Contains(err.Error(), "no such manifest") || strings.Contains(err.Error(), "not found"):
		return fmt.Sprintf("%s does not exist; check the version the plan names", image)
	default:
		return fmt.Sprintf("docker cannot read %s: %v", image, err)
	}
}

// checkTeleport checks the login and reaches every jump host that is set, as
// its login. The first one reached is kept for the CDN checks.
func (c *checks) checkTeleport(ctx context.Context, env []string, envFile []byte, stage string) {
	status, err := tshStatus(ctx, env)
	if err != nil {
		c.problems = append(c.problems, "tsh has no valid login; run tsh login --proxy=$TELEPORT_PROXY")
		return
	}
	if left := time.Until(status.Active.ValidUntil); left < stageLength[stage] {
		c.warnings = append(c.warnings, fmt.Sprintf("your Teleport login ends in %s, before a %s run usually does. A tsh login during the run is picked up; to start fresh: tsh logout && tsh login --proxy=$TELEPORT_PROXY",
			left.Round(time.Minute), stage))
	}

	set := 0
	for _, jh := range lane.JumpHosts(envFile) {
		host := firstEnvIn(env, jh.HostVar)
		if host == "" {
			continue
		}
		set++
		login := firstEnvIn(env, jh.LoginVar)
		if login != "" && !contains(status.Active.Logins, login) {
			c.problems = append(c.problems, fmt.Sprintf("%s is %q, which is not one of your Teleport logins (%s). It takes the account on the jump host, not your Teleport user",
				jh.LoginVar, login, strings.Join(status.Active.Logins, ", ")))
			continue
		}
		s := site{hostVar: jh.HostVar, host: host, login: login}
		if err := quiet(ctx, env, "tsh", "ssh", s.target(), "true"); err != nil {
			c.problems = append(c.problems, fmt.Sprintf("$%s is not reachable as %q: %s",
				jh.HostVar, login, strings.ReplaceAll(err.Error(), host, "$"+jh.HostVar)))
			continue
		}
		if c.site == nil {
			c.site = &s
		}
	}
	if set == 0 {
		var names []string
		for _, jh := range lane.JumpHosts(envFile) {
			names = append(names, jh.HostVar)
		}
		c.problems = append(c.problems, fmt.Sprintf("no jump host is set; the stream's site reads one of %s", strings.Join(names, ", ")))
	}
}

type tshStatusOutput struct {
	Active struct {
		ValidUntil time.Time `json:"valid_until"`
		Logins     []string  `json:"logins"`
	} `json:"active"`
}

func tshStatus(ctx context.Context, env []string) (tshStatusOutput, error) {
	var status tshStatusOutput
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tsh", "status", "--format=json")
	cmd.Env = env
	b, err := cmd.Output()
	if err != nil {
		return status, err
	}
	if err := json.Unmarshal(b, &status); err != nil {
		return status, err
	}
	if time.Until(status.Active.ValidUntil) <= 0 {
		return status, errors.New("the login has expired")
	}
	return status, nil
}

// verifyLocal checks every location the plan names: the registry from here,
// and the CDN from the site when a jump host was reached, since the CDN
// refuses networks it does not allow and the node is what fetches from it.
func verifyLocal(ctx context.Context, out io.Writer, env []string, plan *qualification.Plan, s *site, wait time.Duration) error {
	var registry, web []artifact.Location
	for _, l := range locations(plan) {
		if strings.HasPrefix(l.URL, "https://") && s != nil {
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
		fmt.Fprintf(out, "asking the CDN from the site, through $%s:\n", s.hostVar)
		var missing []string
		for _, l := range web {
			code, err := fromSite(ctx, env, *s, l.URL)
			if err == nil && strings.HasPrefix(code, "2") {
				fmt.Fprintf(out, "published: %s (%s)\n", l.URL, strings.Join(l.Wanted, ", "))
				continue
			}
			reason := "HTTP " + code
			if err != nil {
				reason = strings.ReplaceAll(err.Error(), s.host, "$"+s.hostVar)
			}
			missing = append(missing, fmt.Sprintf("  %s (%s): %s from the site", l.URL, strings.Join(l.Wanted, ", "), reason))
		}
		if len(missing) > 0 {
			errs = append(errs, fmt.Errorf("%d of the CDN artifacts the plan names are not published:\n%s", len(missing), strings.Join(missing, "\n")))
		}
	}
	return errors.Join(errs...)
}

// fromSite is the HTTP status the jump host gets for a HEAD of url.
func fromSite(ctx context.Context, env []string, s site, url string) (string, error) {
	if strings.ContainsAny(url, "'\\") {
		return "", fmt.Errorf("refusing to quote %q for a remote shell", url)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tsh", "ssh", s.target(), "curl -s -o /dev/null -w '%{http_code}' -I --max-time 30 '"+url+"'")
	cmd.Env = env
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err
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
		if quiet(ctx, nil, "docker", "manifest", "inspect", image) == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("%s is on GHCR as neither nvke-controller nor nke-controller", r.ControllerVersion)
}

// ensureControllerTree makes the tree the deploy path is exported from
// present in the checkout: the commit a described version was taken at, or
// the controller's release tag.
func ensureControllerTree(ctx context.Context, out io.Writer, root, remote string, r lane.Run, commit string) error {
	if r.Inputs.ControllerVersionStrategy == resolve.VersionStrategyCommitDescribe {
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

// controllerTreeRef is the tree the deploy path is exported from.
func controllerTreeRef(r lane.Run, commit string) string {
	if r.Inputs.ControllerVersionStrategy == resolve.VersionStrategyCommitDescribe {
		return commit
	}
	return "refs/tags/controller/" + r.ControllerVersion
}

// controllerName is the name the suite should look for the controller under,
// and where it came from: --controller-name, else the shell's own setting,
// which is left alone, else the deploy path's release at ref. The suite fails
// before any machine is taken when it and the deploy path disagree, and the
// name changed between releases, so a published controller older than the
// checkout's suite needs its own.
func controllerName(ctx context.Context, root, ref string, env []string, flag string) (name, from string, err error) {
	if flag != "" {
		return flag, "--controller-name", nil
	}
	if firstEnvIn(env, lane.ControllerNameVars...) != "" {
		return "", "", nil
	}
	b, err := gitOutput(ctx, root, "show", ref+":"+lane.ControllerReleasePath)
	if err != nil {
		return "", "", fmt.Errorf("reading the controller's deploy path at %s: %w (pass --controller-name)", ref, err)
	}
	name, err = lane.ReleaseNamespace([]byte(b))
	if err != nil {
		return "", "", fmt.Errorf("%s at %s: %w (pass --controller-name)", lane.ControllerReleasePath, ref, err)
	}
	return name, "from " + lane.ControllerReleasePath + " at the controller's commit", nil
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
		Plan      string   `json:"plan,omitempty"`
		Scope     string   `json:"scope,omitempty"`
		Registry  string   `json:"registry"`
		SuiteArgs []string `json:"suiteArgs,omitempty"`
		Run       lane.Run `json:"run"`
	}{at.Format(time.RFC3339), root, head, o.plan, "", o.registry, o.suiteArgs, r}
	if o.plan == "" {
		record.Scope = o.scope
	}
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// localIdentity is the qualify result flags that say which checkout ran, as
// GitHub Actions' environment says for the workflow: the repository the
// branch tracks, else remote; the commit; and who ran it. A tree with
// uncommitted changes did not run that commit alone, and the workflow says so.
func localIdentity(ctx context.Context, root, remote string) []string {
	var args []string
	if upstream, err := gitOutput(ctx, root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil {
		remote, _, _ = strings.Cut(upstream, "/")
	}
	if url, err := gitOutput(ctx, root, "remote", "get-url", remote); err == nil {
		if repo := repositoryFromURL(url); repo != "" {
			args = append(args, "--repository", repo)
		}
	}
	if sha, err := gitOutput(ctx, root, "rev-parse", "HEAD"); err == nil {
		args = append(args, "--sha", sha)
	}
	workflow := "qualify local"
	if dirty, err := gitOutput(ctx, root, "status", "--porcelain", "--untracked-files=no"); err == nil && dirty != "" {
		workflow += ", with uncommitted changes"
	}
	args = append(args, "--workflow", workflow)
	if actor, err := gitOutput(ctx, root, "config", "user.name"); err == nil && actor != "" {
		args = append(args, "--actor", actor)
	}
	return args
}

// repositoryFromURL is the owner/name a git remote URL names, in either the
// https or the scp-like ssh form, or "" for one it cannot read.
func repositoryFromURL(url string) string {
	url = strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")
	if _, path, ok := strings.Cut(url, "://"); ok {
		_, url, _ = strings.Cut(path, "/")
	} else if _, path, ok := strings.Cut(url, ":"); ok {
		url = path
	}
	if strings.Count(url, "/") != 1 || strings.HasPrefix(url, "/") || strings.HasSuffix(url, "/") {
		return ""
	}
	return url
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

// quiet runs a short check, in env when it is not nil, and reports its first
// line of output on failure.
func quiet(ctx context.Context, env []string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	b, err := cmd.CombinedOutput()
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

// firstEnvIn is the value of the first of names set non-empty in environ,
// taking the last assignment of each as a shell would.
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
