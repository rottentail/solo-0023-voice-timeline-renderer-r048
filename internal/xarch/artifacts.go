package xarch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// This file packages a failed comparison into a minimal, self-contained,
// replayable failure case. Nothing is discarded: the exact shared inputs, both
// architecture outputs, both receipts, the judge report, and the commands
// needed to reproduce the run are copied into one timestamp-stable directory.

// FailureCase is the metadata written alongside the copied artifacts.
type FailureCase struct {
	SchemaVersion int      `json:"schema_version"`
	SuiteID       string   `json:"suite_id"`
	LeftArch      string   `json:"left_arch"`
	RightArch     string   `json:"right_arch"`
	FailedCases   []string `json:"failed_cases"`
	ReportPath    string   `json:"report_path"`
	Replay        []string `json:"replay"`
}

// MaterializeSuite renders every fixture with the deterministic integer
// synthesizer, writes it into fixtureDir, fills in each source digest, and
// persists the suite manifest. It is idempotent: re-running overwrites with
// byte-identical content and errors if an existing fixture would change digest.
func MaterializeSuite(suite Suite, fixtureDir string) (Suite, error) {
	if err := os.MkdirAll(fixtureDir, 0o750); err != nil {
		return suite, err
	}
	for index := range suite.Sources {
		source := &suite.Sources[index]
		wav, err := BuildFixture(source.Tone)
		if err != nil {
			return suite, err
		}
		path := filepath.Join(fixtureDir, source.FileName)
		if existing, err := os.ReadFile(path); err == nil {
			if SHA256Hex(existing) != SHA256Hex(wav) {
				return suite, fmt.Errorf("regenerating %s would change its bytes; refusing to overwrite an existing input fact", source.FileName)
			}
		}
		if err := os.WriteFile(path, wav, 0o640); err != nil {
			return suite, err
		}
		source.Digest = SHA256Hex(wav)
	}
	manifestBytes, err := json.MarshalIndent(suite, "", "  ")
	if err != nil {
		return suite, err
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, "suite.json"), append(manifestBytes, '\n'), 0o640); err != nil {
		return suite, err
	}
	return suite, nil
}

// LoadSuite reads a materialized suite manifest.
func LoadSuite(fixtureDir string) (Suite, error) {
	data, err := os.ReadFile(filepath.Join(fixtureDir, "suite.json"))
	if err != nil {
		return Suite{}, err
	}
	var suite Suite
	if err := json.Unmarshal(data, &suite); err != nil {
		return Suite{}, fmt.Errorf("parse suite manifest: %w", err)
	}
	return suite, nil
}

// WriteReport persists the judge report as indented JSON.
func WriteReport(report Report, path string) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o640)
}

// PackageFailureCase copies everything needed to replay a failed comparison
// into caseDir and writes a replay manifest. The directory is intentionally
// self-contained so it can be archived and moved to another machine.
func PackageFailureCase(
	suite Suite,
	report Report,
	fixtureDir, leftBundleDir, rightBundleDir, caseDir string,
	replayCommands []string,
) (FailureCase, error) {
	failureCase := FailureCase{
		SchemaVersion: suite.SchemaVersion,
		SuiteID:       suite.SuiteID,
		LeftArch:      report.LeftArch,
		RightArch:     report.RightArch,
		Replay:        replayCommands,
	}
	for _, verdict := range report.Verdicts {
		if verdict.Status == "fail" {
			failureCase.FailedCases = append(failureCase.FailedCases, verdict.TimelineID)
		}
	}
	if len(failureCase.FailedCases) == 0 {
		return failureCase, nil
	}

	dirs := map[string]string{
		"inputs":       filepath.Join(caseDir, "inputs"),
		"left-bundle":  filepath.Join(caseDir, "left-bundle-"+report.LeftArch),
		"right-bundle": filepath.Join(caseDir, "right-bundle-"+report.RightArch),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return failureCase, err
		}
	}

	// Copy the full input fact set: suite manifest plus every pinned fixture.
	if err := copyFile(filepath.Join(fixtureDir, "suite.json"), filepath.Join(dirs["inputs"], "suite.json")); err != nil {
		return failureCase, err
	}
	for _, source := range suite.Sources {
		if err := copyFile(
			filepath.Join(fixtureDir, source.FileName),
			filepath.Join(dirs["inputs"], source.FileName),
		); err != nil {
			return failureCase, err
		}
	}
	for _, timeline := range suite.Timelines {
		names := []string{timeline.ID + ".wav", timeline.ID + ".receipt.json"}
		for _, name := range names {
			if err := copyFile(filepath.Join(leftBundleDir, name), filepath.Join(dirs["left-bundle"], name)); err != nil {
				return failureCase, err
			}
			if err := copyFile(filepath.Join(rightBundleDir, name), filepath.Join(dirs["right-bundle"], name)); err != nil {
				return failureCase, err
			}
		}
	}

	reportPath := filepath.Join(caseDir, "judge-report.json")
	if err := WriteReport(report, reportPath); err != nil {
		return failureCase, err
	}
	failureCase.ReportPath = "judge-report.json"

	manifestBytes, err := json.MarshalIndent(failureCase, "", "  ")
	if err != nil {
		return failureCase, err
	}
	if err := os.WriteFile(filepath.Join(caseDir, "failure-case.json"), append(manifestBytes, '\n'), 0o640); err != nil {
		return failureCase, err
	}
	return failureCase, nil
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o640)
}
