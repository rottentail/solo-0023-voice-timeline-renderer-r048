package xarch

import (
	"testing"
)

// TestFixtureDeterminism guards the core input-fact guarantee: regenerating a
// fixture any number of times must yield byte-identical WAV output, since both
// architectures are required to render from the same inputs.
func TestFixtureDeterminism(t *testing.T) {
	suite := CanonicalSuite()
	first, err := BuildFixture(suite.Sources[0].Tone)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	firstDigest := SHA256Hex(first)
	for attempt := 0; attempt < 5; attempt++ {
		again, err := BuildFixture(suite.Sources[0].Tone)
		if err != nil {
			t.Fatalf("rebuild %d: %v", attempt, err)
		}
		if SHA256Hex(again) != firstDigest {
			t.Fatalf("fixture %s changed between regenerations", suite.Sources[0].ID)
		}
	}
}

// TestAllSuiteFixturesMaterialize verifies every canonical fixture renders and
// round-trips through the judge codec with the expected frame counts.
func TestAllSuiteFixturesMaterialize(t *testing.T) {
	suite := CanonicalSuite()
	for _, source := range suite.Sources {
		wav, err := BuildFixture(source.Tone)
		if err != nil {
			t.Fatalf("build %s: %v", source.ID, err)
		}
		file, err := DecodePCM16(wav)
		if err != nil {
			t.Fatalf("decode %s: %v", source.ID, err)
		}
		if file.FrameCount() != int64(source.Tone.FrameCount()) {
			t.Fatalf("%s frames = %d, want %d", source.ID, file.FrameCount(), source.Tone.FrameCount())
		}
		if file.Channels != source.Tone.Channels || file.SampleRate != source.Tone.SampleRate {
			t.Fatalf("%s header mismatch: %+v", source.ID, file)
		}
	}
}

// TestWAVRoundTrip uses only judge code to prove the independent codec parses
// what it writes without involving any production package.
func TestWAVRoundTrip(t *testing.T) {
	samples := []int16{0, 1, -1, 32767, -32768, 12345, -12345}
	encoded, err := EncodePCM16(48000, 1, samples)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodePCM16(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.SampleRate != 48000 || decoded.Channels != 1 {
		t.Fatalf("decoded header mismatch: %+v", decoded)
	}
	if len(decoded.Samples) != len(samples) {
		t.Fatalf("sample count %d != %d", len(decoded.Samples), len(samples))
	}
	for index, want := range samples {
		if decoded.Samples[index] != want {
			t.Fatalf("sample %d = %d, want %d", index, decoded.Samples[index], want)
		}
	}
}

// TestMeasurePCMIndependence checks integer-domain peak/RMS against hand
// computed values, ensuring the judge does not inherit production float metrics.
func TestMeasurePCMIndependence(t *testing.T) {
	// Peak of [3, 4] is 4; RMS is sqrt((9+16)/2) = sqrt(12.5) ~= 3.5355 codes.
	metrics := MeasurePCM([]int16{3, 4})
	if metrics.PeakCode != 4 {
		t.Fatalf("peak code = %d, want 4", metrics.PeakCode)
	}
	if metrics.RMS < 3.535 || metrics.RMS > 3.536 {
		t.Fatalf("rms = %v, want sqrt(12.5) ~= 3.5355", metrics.RMS)
	}
	silence := MeasurePCM([]int16{0, 0})
	if silence.PeakDBFS != silenceFloorDBFS || silence.RMSDBFS != silenceFloorDBFS {
		t.Fatalf("silence metrics = %+v, want floor %v", silence, silenceFloorDBFS)
	}
}

// syntheticBundles builds two mono bundles from fully controlled integer sample
// sets so each judge rule can be exercised deterministically.
func syntheticBundles(t *testing.T, leftSamples, rightSamples []int16) (Bundle, Bundle) {
	t.Helper()
	timeline := CanonicalSuite().Timelines[0]
	timeline.OutputChannels = 1
	leftWAV, err := EncodePCM16(timeline.SampleRate, 1, leftSamples)
	if err != nil {
		t.Fatalf("encode left: %v", err)
	}
	rightWAV, err := EncodePCM16(timeline.SampleRate, 1, rightSamples)
	if err != nil {
		t.Fatalf("encode right: %v", err)
	}
	left := Bundle{Arch: "arm64", Outputs: map[string][]byte{timeline.ID: leftWAV},
		Receipts: map[string]RenderReceiptDoc{timeline.ID: receiptFor(t, leftWAV, timeline)}}
	right := Bundle{Arch: "amd64", Outputs: map[string][]byte{timeline.ID: rightWAV},
		Receipts: map[string]RenderReceiptDoc{timeline.ID: receiptFor(t, rightWAV, timeline)}}
	return left, right
}

// receiptFor builds a self-consistent receipt document from judge-side decoded
// output, mirroring what the real service receipt must satisfy.
func receiptFor(t *testing.T, wav []byte, timeline TimelineSpec) RenderReceiptDoc {
	t.Helper()
	file, err := DecodePCM16(wav)
	if err != nil {
		t.Fatalf("decode for receipt: %v", err)
	}
	metrics := MeasurePCM(file.Samples)
	var doc RenderReceiptDoc
	doc.SchemaVersion = 1
	doc.Output.Digest = SHA256Hex(wav)
	doc.Output.ByteCount = int64(len(wav))
	doc.Output.SampleRate = file.SampleRate
	doc.Output.Channels = file.Channels
	doc.Output.BitsPerSample = 16
	doc.Output.FrameCount = file.FrameCount()
	doc.Output.PeakDBFS = metrics.PeakDBFS
	doc.Output.RMSDBFS = metrics.RMSDBFS
	doc.Normalization.OutputPeakDBFS = metrics.PeakDBFS
	doc.Normalization.AppliedGainDB = 0
	doc.Normalization.BeforePeakDBFS = metrics.PeakDBFS
	return doc
}

// TestJudgeAcceptsOneLSB confirms ordinary floating-point rounding differences
// (one quantization step, non-zero on both sides) are accepted, not failed.
func TestJudgeAcceptsOneLSB(t *testing.T) {
	left := []int16{1000, 2000, 3000, 4000}
	right := []int16{1000, 2001, 3000, 4000} // single +1 LSB, both non-zero
	bundleLeft, bundleRight := syntheticBundles(t, left, right)
	report := CompareBundles(CanonicalSuite(), bundleLeft, bundleRight)
	verdict := report.Verdicts[0]
	if verdict.Status != "pass" {
		t.Fatalf("one-LSB difference rejected: failures=%v structural=%v receipts=%v",
			verdict.SampleFailures, verdict.StructuralIssues, verdict.ReceiptIssues)
	}
	if verdict.MaxAbsDelta != 1 || verdict.DifferingSamples != 1 {
		t.Fatalf("unexpected diff counts: %+v", verdict)
	}
}

// TestJudgeRejectsExcessDelta ensures a difference larger than one LSB fails,
// so a blanket loose tolerance can never mask a real signal change.
func TestJudgeRejectsExcessDelta(t *testing.T) {
	left := []int16{1000, 2000, 3000, 4000}
	right := []int16{1000, 2005, 3000, 4000} // +5 LSB
	bundleLeft, bundleRight := syntheticBundles(t, left, right)
	report := CompareBundles(CanonicalSuite(), bundleLeft, bundleRight)
	if report.Status != "fail" {
		t.Fatalf("5-LSB drift was accepted")
	}
	if report.Verdicts[0].MaxAbsDelta != 5 {
		t.Fatalf("max delta = %d, want 5", report.Verdicts[0].MaxAbsDelta)
	}
}

// TestJudgeRejectsSilenceDrift proves a zero PCM code must remain exactly zero
// even though the numeric delta is only one LSB.
func TestJudgeRejectsSilenceDrift(t *testing.T) {
	left := []int16{0, 100, 200, 300}
	right := []int16{1, 100, 200, 300} // silence 0 -> 1
	bundleLeft, bundleRight := syntheticBundles(t, left, right)
	report := CompareBundles(CanonicalSuite(), bundleLeft, bundleRight)
	if report.Status != "fail" {
		t.Fatalf("silence drift 0->1 was accepted")
	}
	failures := report.Verdicts[0].SampleFailures
	if len(failures) != 1 || failures[0].Reason == "" {
		t.Fatalf("expected one explicit silence failure, got %+v", failures)
	}
}

// TestJudgeRejectsStructuralChange guards the exact-match tier: a changed frame
// count must fail regardless of how small the per-sample deltas look.
func TestJudgeRejectsStructuralChange(t *testing.T) {
	left := make([]int16, 512)
	for index := range left {
		left[index] = int16(index % 1000)
	}
	right := append(append([]int16(nil), left...), 0) // one extra frame's worth
	bundleLeft, bundleRight := syntheticBundles(t, left, right)
	report := CompareBundles(CanonicalSuite(), bundleLeft, bundleRight)
	if report.Status != "fail" {
		t.Fatalf("structural frame-count change was accepted")
	}
	if len(report.Verdicts[0].StructuralIssues) == 0 {
		t.Fatalf("expected a structural issue, got %+v", report.Verdicts[0])
	}
}

// TestJudgeRejectsPolarityFlip ensures a sign reversal at audible amplitude is
// treated as a meaning-changing signal difference.
func TestJudgeRejectsPolarityFlip(t *testing.T) {
	left := []int16{2000, 2000, -2000, -2000}
	right := []int16{2000, 2000, 2000, -2000} // -2000 -> +2000 polarity flip
	bundleLeft, bundleRight := syntheticBundles(t, left, right)
	report := CompareBundles(CanonicalSuite(), bundleLeft, bundleRight)
	if report.Status != "fail" {
		t.Fatalf("non-trivial polarity flip was accepted")
	}
	found := false
	for _, failure := range report.Verdicts[0].SampleFailures {
		if failure.LeftCode == -2000 && failure.RightCode == 2000 {
			found = true
		}
	}
	if !found {
		t.Fatalf("polarity flip not recorded: %+v", report.Verdicts[0].SampleFailures)
	}
}

// TestJudgeRejectsInconsistentReceipt verifies a receipt whose digest no longer
// matches its output is caught independently on each side.
func TestJudgeRejectsInconsistentReceipt(t *testing.T) {
	left := []int16{1000, 2000, 3000}
	bundleLeft, bundleRight := syntheticBundles(t, left, left)
	// Tamper with the right-side receipt digest without changing the WAV.
	timeline := CanonicalSuite().Timelines[0]
	receipt := bundleRight.Receipts[timeline.ID]
	receipt.Output.Digest = "0000000000000000000000000000000000000000000000000000000000000000"
	bundleRight.Receipts[timeline.ID] = receipt
	report := CompareBundles(CanonicalSuite(), bundleLeft, bundleRight)
	if report.Status != "fail" {
		t.Fatalf("tampered receipt digest was accepted")
	}
}
