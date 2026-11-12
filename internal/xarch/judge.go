package xarch

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// This file is the compatibility judge. It is deliberately independent of the
// production audio packages: it re-parses downloaded WAV bytes with the local
// wavio decoder and recomputes signal metrics itself, then applies a tiered
// comparison rather than one blanket tolerance:
//
//   - Structural / editing-meaning differences (container shape, frame count,
//     span placement, digest self-consistency, silence support) are exact. Any
//     mismatch fails the run.
//   - Signal-level differences are accepted only inside the smallest possible
//     bound for PCM16 exchange: one integer LSB per sample, and never when one
//     side is digital silence (a zero code must stay exactly zero). Larger
//     deltas, polarity flips at non-trivial amplitude, or shifted silence are
//     failures because they change the signal's meaning.
//   - Receipt floating-point measurements are compared against the judge's own
//     independently recomputed values, and against each other, with a tight
//     numeric epsilon derived from LSB-scale dB movement -- not a loose
//     catch-all.

// Tolerances used by the judge. They are intentionally derived from the PCM16
// quantizer instead of chosen to make tests pass.
const (
	// maxLSBDelta bounds an accepted per-sample difference to one quantization
	// step: float32 evaluation-order differences between architectures can move
	// a rounding boundary by one code, never more.
	maxLSBDelta = 1
	// nearFullScaleCode marks amplitudes large enough that a sign reversal would
	// be audible and semantically meaningful rather than a rounding artifact.
	nearFullScaleCode = 256 // ~ -42 dBFS
	// metricDBEpsilon bounds dBFS agreement. One LSB applied at a -90 dBFS floor
	// moves a dB figure by far less than this; 1e-3 dB is deliberately tight.
	metricDBEpsilon = 1e-3
	// gainDBEpsilon bounds the normalization gain report, which is float64 math
	// shared through receipt serialization only.
	gainDBEpsilon = 1e-6
)

// SampleFailure records the first samples that violate the signal contract.
type SampleFailure struct {
	Frame     int    `json:"frame"`
	Channel   int    `json:"channel"`
	LeftCode  int16  `json:"left_code"`
	RightCode int16  `json:"right_code"`
	Delta     int    `json:"delta"`
	Reason    string `json:"reason"`
}

// TimelineVerdict is the judge result for one render scenario.
type TimelineVerdict struct {
	TimelineID       string          `json:"timeline_id"`
	Status           string          `json:"status"` // "pass" | "fail"
	FrameCount       int64           `json:"frame_count"`
	Channels         int             `json:"channels"`
	SampleRate       int             `json:"sample_rate"`
	ComparedSamples  int             `json:"compared_samples"`
	DifferingSamples int             `json:"differing_samples"`
	MaxAbsDelta      int             `json:"max_abs_delta"`
	SampleFailures   []SampleFailure `json:"sample_failures,omitempty"`
	StructuralIssues []string        `json:"structural_issues,omitempty"`
	ReceiptIssues    []string        `json:"receipt_issues,omitempty"`
}

// Report is the full judge output for a pair of architecture bundles.
type Report struct {
	SchemaVersion int               `json:"schema_version"`
	SuiteID       string            `json:"suite_id"`
	LeftArch      string            `json:"left_arch"`
	RightArch     string            `json:"right_arch"`
	Status        string            `json:"status"` // "pass" | "fail"
	Verdicts      []TimelineVerdict `json:"verdicts"`
}

// Bundle is one architecture's rendered output set.
type Bundle struct {
	Arch     string
	Outputs  map[string][]byte // timeline ID -> downloaded WAV bytes
	Receipts map[string]RenderReceiptDoc
}

// RenderReceiptDoc mirrors only the receipt fields the judge examines. It is
// declared locally (rather than importing domain) to keep the judge decoupled
// from production type evolution.
type RenderReceiptDoc struct {
	SchemaVersion int    `json:"schema_version"`
	RenderID      string `json:"render_id"`
	TimelineID    string `json:"timeline_id"`
	Output        struct {
		Digest        string  `json:"digest"`
		ByteCount     int64   `json:"byte_count"`
		SampleRate    int     `json:"sample_rate"`
		Channels      int     `json:"channels"`
		BitsPerSample int     `json:"bits_per_sample"`
		FrameCount    int64   `json:"frame_count"`
		DurationMS    int64   `json:"duration_ms"`
		PeakDBFS      float64 `json:"peak_dbfs"`
		RMSDBFS       float64 `json:"rms_dbfs"`
	} `json:"output"`
	Normalization struct {
		RequestedPeakDBFS float64 `json:"requested_peak_dbfs"`
		BeforePeakDBFS    float64 `json:"before_peak_dbfs"`
		AppliedGainDB     float64 `json:"applied_gain_db"`
		OutputPeakDBFS    float64 `json:"output_peak_dbfs"`
	} `json:"normalization"`
	Spans []struct {
		Index            int     `json:"index"`
		SourceID         string  `json:"source_id"`
		SourceDigest     string  `json:"source_digest"`
		SourceStartFrame int64   `json:"source_start_frame"`
		SourceEndFrame   int64   `json:"source_end_frame"`
		OutputStartFrame int64   `json:"output_start_frame"`
		OutputEndFrame   int64   `json:"output_end_frame"`
		OutputFrames     int64   `json:"output_frames"`
		CrossfadeFrames  int64   `json:"crossfade_frames"`
		GainDB           float64 `json:"gain_db"`
		ChannelAction    string  `json:"channel_action"`
	} `json:"spans"`
	Warnings []string `json:"warnings"`
}

// LoadReceipt reads and parses a receipt JSON document from disk.
func LoadReceipt(path string) (RenderReceiptDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RenderReceiptDoc{}, err
	}
	var doc RenderReceiptDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return RenderReceiptDoc{}, fmt.Errorf("parse receipt %s: %w", path, err)
	}
	return doc, nil
}

// IndependentMetrics holds judge-side recomputed signal measurements in raw
// integer terms plus their dBFS conversion, so the receipt cannot grade itself.
type IndependentMetrics struct {
	PeakCode int
	RMS      float64 // in PCM codes, float64 only for the aggregate
	PeakDBFS float64
	RMSDBFS  float64
}

const silenceFloorDBFS = -120.0

// MeasurePCM recomputes peak/RMS directly from integer PCM codes. The RMS
// accumulation uses integer squares in int64; the final sqrt is the only
// transcendental and operates on identical integer inputs on both sides.
func MeasurePCM(samples []int16) IndependentMetrics {
	if len(samples) == 0 {
		return IndependentMetrics{PeakDBFS: silenceFloorDBFS, RMSDBFS: silenceFloorDBFS}
	}
	peak := 0
	var sumSquares int64
	for _, sample := range samples {
		absolute := int(sample)
		if absolute < 0 {
			absolute = -absolute
		}
		if absolute > peak {
			peak = absolute
		}
		sumSquares += int64(absolute) * int64(absolute)
	}
	rms := math.Sqrt(float64(sumSquares) / float64(len(samples)))
	return IndependentMetrics{
		PeakCode: peak,
		RMS:      rms,
		PeakDBFS: codeToDBFS(float64(peak)),
		RMSDBFS:  codeToDBFS(rms),
	}
}

func codeToDBFS(amplitude float64) float64 {
	if amplitude <= 0 {
		return silenceFloorDBFS
	}
	db := 20 * math.Log10(amplitude/32768.0)
	if db < silenceFloorDBFS {
		return silenceFloorDBFS
	}
	return db
}

// CompareBundles judges two architecture bundles against each other and checks
// every receipt against the independently decoded and measured output.
func CompareBundles(suite Suite, left, right Bundle) Report {
	report := Report{
		SchemaVersion: suite.SchemaVersion,
		SuiteID:       suite.SuiteID,
		LeftArch:      left.Arch,
		RightArch:     right.Arch,
		Status:        "pass",
	}

	for _, timeline := range suite.Timelines {
		verdict := compareTimeline(timeline, left, right)
		if verdict.Status != "pass" {
			report.Status = "fail"
		}
		report.Verdicts = append(report.Verdicts, verdict)
	}
	return report
}

func compareTimeline(timeline TimelineSpec, left, right Bundle) TimelineVerdict {
	verdict := TimelineVerdict{TimelineID: timeline.ID, Status: "pass"}

	leftWAV, leftOK := left.Outputs[timeline.ID]
	rightWAV, rightOK := right.Outputs[timeline.ID]
	if !leftOK || !rightOK {
		verdict.Status = "fail"
		verdict.StructuralIssues = append(verdict.StructuralIssues,
			fmt.Sprintf("missing rendered output: %s present=%v, %s present=%v", left.Arch, leftOK, right.Arch, rightOK))
		return verdict
	}

	leftFile, leftErr := DecodePCM16(leftWAV)
	rightFile, rightErr := DecodePCM16(rightWAV)
	if leftErr != nil || rightErr != nil {
		verdict.Status = "fail"
		verdict.StructuralIssues = append(verdict.StructuralIssues,
			fmt.Sprintf("independent WAV decode failed: %s err=%v, %s err=%v", left.Arch, leftErr, right.Arch, rightErr))
		return verdict
	}

	// Tier 1: structural / container meaning must match exactly.
	if leftFile.SampleRate != rightFile.SampleRate ||
		leftFile.Channels != rightFile.Channels ||
		len(leftFile.Samples) != len(rightFile.Samples) {
		verdict.Status = "fail"
		verdict.StructuralIssues = append(verdict.StructuralIssues, fmt.Sprintf(
			"container shape differs: %s rate=%d ch=%d samples=%d vs %s rate=%d ch=%d samples=%d",
			left.Arch, leftFile.SampleRate, leftFile.Channels, len(leftFile.Samples),
			right.Arch, rightFile.SampleRate, rightFile.Channels, len(rightFile.Samples)))
	}
	if len(leftWAV) != len(rightWAV) {
		verdict.Status = "fail"
		verdict.StructuralIssues = append(verdict.StructuralIssues, fmt.Sprintf(
			"encoded WAV byte length differs: %s=%d %s=%d", left.Arch, len(leftWAV), right.Arch, len(rightWAV)))
	}
	verdict.FrameCount = leftFile.FrameCount()
	verdict.Channels = leftFile.Channels
	verdict.SampleRate = leftFile.SampleRate

	// Tier 2: sample-level comparison with the bounded LSB rule and explicit
	// protection against meaning-changing edits (silence drift, polarity flips).
	verdict.ComparedSamples = len(leftFile.Samples)
	maxDelta := 0
	differing := 0
	for index, leftCode := range leftFile.Samples {
		rightCode := rightFile.Samples[index]
		if leftCode == rightCode {
			continue
		}
		differing++
		delta := int(rightCode) - int(leftCode)
		absolute := delta
		if absolute < 0 {
			absolute = -absolute
		}
		if absolute > maxDelta {
			maxDelta = absolute
		}
		reason := classifySampleDifference(leftCode, rightCode, absolute)
		if reason != "" && len(verdict.SampleFailures) < 10 {
			verdict.SampleFailures = append(verdict.SampleFailures, SampleFailure{
				Frame:     index / leftFile.Channels,
				Channel:   index % leftFile.Channels,
				LeftCode:  leftCode,
				RightCode: rightCode,
				Delta:     delta,
				Reason:    reason,
			})
		}
	}
	verdict.DifferingSamples = differing
	verdict.MaxAbsDelta = maxDelta
	if len(verdict.SampleFailures) > 0 {
		verdict.Status = "fail"
	}

	// Tier 3: receipts must be self-consistent with the independently decoded
	// output and mutually identical on every structural field.
	leftReceipt, hasLeftReceipt := left.Receipts[timeline.ID]
	rightReceipt, hasRightReceipt := right.Receipts[timeline.ID]
	if !hasLeftReceipt || !hasRightReceipt {
		verdict.Status = "fail"
		verdict.ReceiptIssues = append(verdict.ReceiptIssues,
			fmt.Sprintf("missing receipt: %s present=%v, %s present=%v", left.Arch, hasLeftReceipt, right.Arch, hasRightReceipt))
		return verdict
	}
	verdict.ReceiptIssues = append(verdict.ReceiptIssues, judgeReceipt(left.Arch, leftReceipt, leftWAV, leftFile)...)
	verdict.ReceiptIssues = append(verdict.ReceiptIssues, judgeReceipt(right.Arch, rightReceipt, rightWAV, rightFile)...)
	verdict.ReceiptIssues = append(verdict.ReceiptIssues, compareReceipts(left.Arch, right.Arch, leftReceipt, rightReceipt)...)
	if len(verdict.ReceiptIssues) > 0 {
		verdict.Status = "fail"
	}
	return verdict
}

// classifySampleDifference returns "" when the difference is an acceptable
// one-LSB numeric variation, otherwise a stable reason describing the changed
// signal meaning.
func classifySampleDifference(leftCode, rightCode int16, absoluteDelta int) string {
	switch {
	case leftCode == 0 || rightCode == 0:
		return "digital silence on one side differs: zero PCM code must remain exactly zero"
	case absoluteDelta > maxLSBDelta:
		return fmt.Sprintf("sample delta %d LSB exceeds the accepted one-LSB numeric bound", absoluteDelta)
	case oppositeSignNonTrivial(leftCode, rightCode):
		return "non-trivial amplitude polarity flipped between architectures"
	default:
		return ""
	}
}

func oppositeSignNonTrivial(a, b int16) bool {
	if (a >= 0) == (b >= 0) {
		return false
	}
	absolute := int(a)
	if absolute < 0 {
		absolute = -absolute
	}
	return absolute >= nearFullScaleCode
}

// judgeReceipt verifies one receipt against its own output using only judge
// code: digest, structural counts, and independently recomputed metrics.
func judgeReceipt(arch string, receipt RenderReceiptDoc, wav []byte, file *PCM16File) []string {
	var issues []string
	expectedDigest := SHA256Hex(wav)
	if receipt.Output.Digest != expectedDigest {
		issues = append(issues, fmt.Sprintf("%s receipt output digest %s does not match downloaded bytes %s",
			arch, receipt.Output.Digest, expectedDigest))
	}
	if receipt.Output.ByteCount != int64(len(wav)) {
		issues = append(issues, fmt.Sprintf("%s receipt byte_count %d != downloaded %d",
			arch, receipt.Output.ByteCount, len(wav)))
	}
	if receipt.Output.FrameCount != file.FrameCount() {
		issues = append(issues, fmt.Sprintf("%s receipt frame_count %d != decoded %d",
			arch, receipt.Output.FrameCount, file.FrameCount()))
	}
	if receipt.Output.SampleRate != file.SampleRate || receipt.Output.Channels != file.Channels ||
		receipt.Output.BitsPerSample != 16 {
		issues = append(issues, fmt.Sprintf("%s receipt container fields disagree with decoded WAV", arch))
	}
	metrics := MeasurePCM(file.Samples)
	if !floatWithin(receipt.Output.PeakDBFS, metrics.PeakDBFS, metricDBEpsilon) {
		issues = append(issues, fmt.Sprintf("%s receipt peak_dbfs %.6f disagrees with independent measure %.6f",
			arch, receipt.Output.PeakDBFS, metrics.PeakDBFS))
	}
	if !floatWithin(receipt.Output.RMSDBFS, metrics.RMSDBFS, metricDBEpsilon) {
		issues = append(issues, fmt.Sprintf("%s receipt rms_dbfs %.6f disagrees with independent measure %.6f",
			arch, receipt.Output.RMSDBFS, metrics.RMSDBFS))
	}
	if !floatWithin(receipt.Normalization.OutputPeakDBFS, metrics.PeakDBFS, metricDBEpsilon) {
		issues = append(issues, fmt.Sprintf("%s normalization output_peak_dbfs %.6f disagrees with independent measure %.6f",
			arch, receipt.Normalization.OutputPeakDBFS, metrics.PeakDBFS))
	}
	return issues
}

// compareReceipts enforces exact equality on editing-meaning fields and tight
// numeric agreement on measurement fields between the two architectures.
func compareReceipts(leftArch, rightArch string, left, right RenderReceiptDoc) []string {
	var issues []string
	if left.SchemaVersion != right.SchemaVersion {
		issues = append(issues, fmt.Sprintf("receipt schema_version differs: %s=%d %s=%d",
			leftArch, left.SchemaVersion, rightArch, right.SchemaVersion))
	}
	if left.Output.FrameCount != right.Output.FrameCount ||
		left.Output.SampleRate != right.Output.SampleRate ||
		left.Output.Channels != right.Output.Channels ||
		left.Output.BitsPerSample != right.Output.BitsPerSample ||
		left.Output.ByteCount != right.Output.ByteCount ||
		left.Output.DurationMS != right.Output.DurationMS {
		issues = append(issues, "receipt structural output fields differ between architectures")
	}
	if len(left.Spans) != len(right.Spans) {
		issues = append(issues, fmt.Sprintf("receipt span count differs: %s=%d %s=%d",
			leftArch, len(left.Spans), rightArch, len(right.Spans)))
	} else {
		for index := range left.Spans {
			l, r := left.Spans[index], right.Spans[index]
			if l.SourceID != r.SourceID || l.SourceDigest != r.SourceDigest ||
				l.SourceStartFrame != r.SourceStartFrame || l.SourceEndFrame != r.SourceEndFrame ||
				l.OutputStartFrame != r.OutputStartFrame || l.OutputEndFrame != r.OutputEndFrame ||
				l.OutputFrames != r.OutputFrames || l.CrossfadeFrames != r.CrossfadeFrames ||
				l.ChannelAction != r.ChannelAction || l.GainDB != r.GainDB {
				issues = append(issues, fmt.Sprintf("receipt span %d placement/gain differs between architectures", index))
			}
		}
	}
	if !floatWithin(left.Normalization.AppliedGainDB, right.Normalization.AppliedGainDB, gainDBEpsilon) {
		issues = append(issues, fmt.Sprintf("normalization applied_gain_db differs: %s=%.9f %s=%.9f",
			leftArch, left.Normalization.AppliedGainDB, rightArch, right.Normalization.AppliedGainDB))
	}
	if !floatWithin(left.Normalization.BeforePeakDBFS, right.Normalization.BeforePeakDBFS, metricDBEpsilon) {
		issues = append(issues, fmt.Sprintf("normalization before_peak_dbfs differs: %s=%.6f %s=%.6f",
			leftArch, left.Normalization.BeforePeakDBFS, rightArch, right.Normalization.BeforePeakDBFS))
	}
	if !floatWithin(left.Output.RMSDBFS, right.Output.RMSDBFS, metricDBEpsilon) {
		issues = append(issues, fmt.Sprintf("receipt rms_dbfs differs: %s=%.6f %s=%.6f",
			leftArch, left.Output.RMSDBFS, rightArch, right.Output.RMSDBFS))
	}
	if !floatWithin(left.Output.PeakDBFS, right.Output.PeakDBFS, metricDBEpsilon) {
		issues = append(issues, fmt.Sprintf("receipt peak_dbfs differs: %s=%.6f %s=%.6f",
			leftArch, left.Output.PeakDBFS, rightArch, right.Output.PeakDBFS))
	}
	if !equalStrings(left.Warnings, right.Warnings) {
		issues = append(issues, "receipt warnings differ between architectures")
	}
	return issues
}

func floatWithin(a, b, epsilon float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff <= epsilon
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
