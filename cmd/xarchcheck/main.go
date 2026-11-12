// Command xarchcheck implements repeatable cross-architecture audio
// compatibility verification for the voice-timeline renderer.
//
// Subcommands:
//
//	fixtures  Materialize the deterministic, digest-pinned input suite.
//	run       Render the suite through a real service binary for one arch.
//	judge     Compare two architecture bundles with the independent judge.
//	verify    Orchestrate a full two-architecture run (used by the shell driver).
//
// The judge never imports a production audio package, so a change in production
// rendering behavior cannot make the comparison silently pass.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"voice-timeline-renderer/internal/xarch"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "fixtures":
		err = fixturesCommand(os.Args[2:])
	case "run":
		err = runCommand(os.Args[2:])
	case "judge":
		err = judgeCommand(os.Args[2:])
	case "verify":
		err = verifyCommand(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "xarchcheck %s failed: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: xarchcheck <subcommand> [flags]

  fixtures -fixture-dir <dir>
      Materialize the deterministic source WAV fixtures and suite manifest.

  run -arch <name> -binary <path> [-qemu <path>] -fixture-dir <dir>
      -work-dir <dir> -bundle-dir <dir>
      Start one architecture's service binary and render the full suite.

  judge -fixture-dir <dir> -left-arch <name> -left-bundle <dir>
      -right-arch <name> -right-bundle <dir> -report <path>
      [-failure-case-dir <dir>]
      Compare two bundles using the independent judge.

  verify -fixture-dir <dir> -work-dir <dir> -result-dir <dir>
      -left-arch <name> -left-binary <path> [-left-qemu <path>]
      -right-arch <name> -right-binary <path> [-right-qemu <path>]
      Run both architectures, judge them, and package failures.
`)
}

func fixturesCommand(args []string) error {
	flags := flag.NewFlagSet("fixtures", flag.ContinueOnError)
	fixtureDir := flags.String("fixture-dir", "", "directory for materialized WAV fixtures and suite.json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *fixtureDir == "" {
		return fmt.Errorf("-fixture-dir is required")
	}
	suite, err := xarch.MaterializeSuite(xarch.CanonicalSuite(), *fixtureDir)
	if err != nil {
		return err
	}
	for _, source := range suite.Sources {
		fmt.Printf("fixture %s frames=%d digest=%s\n", source.FileName, source.Tone.FrameCount(), source.Digest)
	}
	fmt.Printf("suite %s materialized in %s with %d sources and %d timelines\n",
		suite.SuiteID, *fixtureDir, len(suite.Sources), len(suite.Timelines))
	return nil
}

func runCommand(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	arch := flags.String("arch", "", "architecture label for this run")
	binaryPath := flags.String("binary", "", "path to the voiceservice binary")
	qemu := flags.String("qemu", "", "optional qemu user emulator binary")
	fixtureDir := flags.String("fixture-dir", "", "materialized fixture directory")
	workDir := flags.String("work-dir", "", "scratch directory for service data")
	bundleDir := flags.String("bundle-dir", "", "directory to write this arch's bundle")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *arch == "" || *binaryPath == "" || *fixtureDir == "" || *workDir == "" || *bundleDir == "" {
		return fmt.Errorf("-arch, -binary, -fixture-dir, -work-dir and -bundle-dir are required")
	}
	suite, err := xarch.LoadSuite(*fixtureDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	bundle, err := xarch.RunArchitecture(ctx, suite, xarch.RunnerConfig{
		Arch:       *arch,
		BinaryPath: *binaryPath,
		QEMUBin:    *qemu,
		WorkDir:    *workDir,
		BundleDir:  *bundleDir,
		FixtureDir: *fixtureDir,
	})
	if err != nil {
		return err
	}
	fmt.Printf("arch %s rendered %d timelines into %s\n", bundle.Arch, len(bundle.Outputs), *bundleDir)
	return nil
}

func judgeCommand(args []string) error {
	flags := flag.NewFlagSet("judge", flag.ContinueOnError)
	fixtureDir := flags.String("fixture-dir", "", "materialized fixture directory")
	leftArch := flags.String("left-arch", "", "left architecture label")
	leftBundleDir := flags.String("left-bundle", "", "left bundle directory")
	rightArch := flags.String("right-arch", "", "right architecture label")
	rightBundleDir := flags.String("right-bundle", "", "right bundle directory")
	reportPath := flags.String("report", "", "path to write the judge report JSON")
	failureCaseDir := flags.String("failure-case-dir", "", "optional directory for a replayable failure case")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *fixtureDir == "" || *leftArch == "" || *leftBundleDir == "" || *rightArch == "" || *rightBundleDir == "" || *reportPath == "" {
		return fmt.Errorf("-fixture-dir, both arch labels, both bundles, and -report are required")
	}
	suite, err := xarch.LoadSuite(*fixtureDir)
	if err != nil {
		return err
	}
	left, err := loadBundle(*leftArch, *leftBundleDir, suite)
	if err != nil {
		return err
	}
	right, err := loadBundle(*rightArch, *rightBundleDir, suite)
	if err != nil {
		return err
	}
	report := xarch.CompareBundles(suite, left, right)
	if err := xarch.WriteReport(report, *reportPath); err != nil {
		return err
	}
	printReportSummary(report)
	if report.Status != "pass" && *failureCaseDir != "" {
		replay := []string{
			"xarchcheck run -arch " + left.Arch + " -binary <" + left.Arch + "-voiceservice> -fixture-dir inputs",
			"xarchcheck run -arch " + right.Arch + " -binary <" + right.Arch + "-voiceservice> -fixture-dir inputs",
			"xarchcheck judge -fixture-dir inputs -left-arch " + left.Arch + " -right-arch " + right.Arch,
		}
		failureCase, err := xarch.PackageFailureCase(suite, report, *fixtureDir,
			*leftBundleDir, *rightBundleDir, *failureCaseDir, replay)
		if err != nil {
			return err
		}
		fmt.Printf("replayable failure case packaged in %s (cases: %v)\n", *failureCaseDir, failureCase.FailedCases)
	}
	if report.Status != "pass" {
		return fmt.Errorf("cross-architecture compatibility check FAILED; see %s", *reportPath)
	}
	return nil
}

func verifyCommand(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	fixtureDir := flags.String("fixture-dir", "", "materialized fixture directory")
	workDir := flags.String("work-dir", "", "scratch directory")
	resultDir := flags.String("result-dir", "", "directory for bundles, report and failures")
	leftArch := flags.String("left-arch", "", "left architecture label")
	leftBinary := flags.String("left-binary", "", "left service binary")
	leftQEMU := flags.String("left-qemu", "", "optional qemu for the left binary")
	rightArch := flags.String("right-arch", "", "right architecture label")
	rightBinary := flags.String("right-binary", "", "right service binary")
	rightQEMU := flags.String("right-qemu", "", "optional qemu for the right binary")
	if err := flags.Parse(args); err != nil {
		return err
	}
	required := map[string]string{
		"-fixture-dir": *fixtureDir, "-work-dir": *workDir, "-result-dir": *resultDir,
		"-left-arch": *leftArch, "-left-binary": *leftBinary,
		"-right-arch": *rightArch, "-right-binary": *rightBinary,
	}
	for name, value := range required {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	suite, err := xarch.LoadSuite(*fixtureDir)
	if err != nil {
		return err
	}
	leftBundleDir := filepath.Join(*resultDir, "bundle-"+*leftArch)
	rightBundleDir := filepath.Join(*resultDir, "bundle-"+*rightArch)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	left, err := xarch.RunArchitecture(ctx, suite, xarch.RunnerConfig{
		Arch: *leftArch, BinaryPath: *leftBinary, QEMUBin: *leftQEMU,
		WorkDir: *workDir, BundleDir: leftBundleDir, FixtureDir: *fixtureDir,
	})
	if err != nil {
		return fmt.Errorf("left (%s) run failed: %w", *leftArch, err)
	}
	right, err := xarch.RunArchitecture(ctx, suite, xarch.RunnerConfig{
		Arch: *rightArch, BinaryPath: *rightBinary, QEMUBin: *rightQEMU,
		WorkDir: *workDir, BundleDir: rightBundleDir, FixtureDir: *fixtureDir,
	})
	if err != nil {
		return fmt.Errorf("right (%s) run failed: %w", *rightArch, err)
	}

	report := xarch.CompareBundles(suite, left, right)
	reportPath := filepath.Join(*resultDir, "judge-report.json")
	if err := xarch.WriteReport(report, reportPath); err != nil {
		return err
	}
	printReportSummary(report)
	if report.Status != "pass" {
		failureCaseDir := filepath.Join(*resultDir, "failure-case")
		replay := []string{
			"scripts/xarch-verify.sh (full replay from source)",
			"xarchcheck judge -fixture-dir <case>/inputs -left-arch " + left.Arch +
				" -left-bundle <case>/left-bundle-" + left.Arch +
				" -right-arch " + right.Arch + " -right-bundle <case>/right-bundle-" + right.Arch,
		}
		if _, err := xarch.PackageFailureCase(suite, report, *fixtureDir,
			leftBundleDir, rightBundleDir, failureCaseDir, replay); err != nil {
			return err
		}
		fmt.Printf("replayable failure case packaged in %s\n", failureCaseDir)
		return fmt.Errorf("cross-architecture compatibility check FAILED; see %s", reportPath)
	}
	fmt.Println("cross-architecture compatibility verification PASSED")
	return nil
}

func loadBundle(arch, dir string, suite xarch.Suite) (xarch.Bundle, error) {
	bundle := xarch.Bundle{
		Arch:     arch,
		Outputs:  make(map[string][]byte),
		Receipts: make(map[string]xarch.RenderReceiptDoc),
	}
	for _, timeline := range suite.Timelines {
		wav, err := os.ReadFile(filepath.Join(dir, timeline.ID+".wav"))
		if err != nil {
			return bundle, fmt.Errorf("load %s output: %w", arch, err)
		}
		receipt, err := xarch.LoadReceipt(filepath.Join(dir, timeline.ID+".receipt.json"))
		if err != nil {
			return bundle, err
		}
		bundle.Outputs[timeline.ID] = wav
		bundle.Receipts[timeline.ID] = receipt
	}
	return bundle, nil
}

func printReportSummary(report xarch.Report) {
	data, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(data))
}
