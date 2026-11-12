package xarch

// This file is the single definition of the cross-architecture verification
// suite: which source WAV fixtures exist and which timelines must be rendered.
// The `fixtures` subcommand materializes it under testdata/xarch; every
// architecture-specific runner then reads the same materialized files, so both
// sides are driven by identical input facts.

// SourceSpec pins one registered source fixture and its expected digest.
type SourceSpec struct {
	ID       string     `json:"id"`
	FileName string     `json:"file_name"`
	Tone     ToneConfig `json:"tone"`
	// Digest is the SHA-256 of the materialized WAV bytes. Runners reject any
	// fixture whose on-disk digest differs before talking to the service.
	Digest string `json:"digest"`
}

// SpanSpec is one timeline span, using the same frame semantics as the API.
type SpanSpec struct {
	SourceID         string  `json:"source_id"`
	SourceStartFrame int64   `json:"source_start_frame"`
	SourceEndFrame   int64   `json:"source_end_frame"`
	GainDB           float64 `json:"gain_db"`
	CrossfadeFrames  int64   `json:"crossfade_frames"`
}

// TimelineSpec is one full render scenario.
type TimelineSpec struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	SampleRate     int        `json:"sample_rate"`
	OutputChannels int        `json:"output_channels"`
	TargetPeakDBFS float64    `json:"target_peak_dbfs"`
	Spans          []SpanSpec `json:"spans"`
}

// Suite is the materialized, digest-pinned input contract.
type Suite struct {
	SchemaVersion int            `json:"schema_version"`
	SuiteID       string         `json:"suite_id"`
	Sources       []SourceSpec   `json:"sources"`
	Timelines     []TimelineSpec `json:"timelines"`
}

const suiteSchemaVersion = 1
const suiteID = "voice-timeline-xarch-v1"

// CanonicalSuite returns the built-in suite definition. Tone synthesis is
// integer-only, so the pinned digests are reproducible on any architecture.
func CanonicalSuite() Suite {
	sources := []SourceSpec{
		{
			ID:       "src_mono_48k_a",
			FileName: "src_mono_48k_a.wav",
			Tone: ToneConfig{
				ID:                   "src_mono_48k_a",
				SampleRate:           48000,
				Channels:             1,
				DurationMS:           400,
				FrequencyHz:          220,
				AmplitudeNumerator:   1,
				AmplitudeDenominator: 4,
			},
		},
		{
			ID:       "src_stereo_44k1_b",
			FileName: "src_stereo_44k1_b.wav",
			Tone: ToneConfig{
				ID:                         "src_stereo_44k1_b",
				SampleRate:                 44100,
				Channels:                   2,
				DurationMS:                 350,
				FrequencyHz:                330,
				AmplitudeNumerator:         1,
				AmplitudeDenominator:       5,
				SecondChannelGainNumerator: 4,
				PhaseOffsetQ15:             q15Pi / 3,
			},
		},
		{
			ID:       "src_mono_44k1_c",
			FileName: "src_mono_44k1_c.wav",
			Tone: ToneConfig{
				ID:                   "src_mono_44k1_c",
				SampleRate:           44100,
				Channels:             1,
				DurationMS:           300,
				FrequencyHz:          660,
				AmplitudeNumerator:   1,
				AmplitudeDenominator: 3,
			},
		},
		{
			ID:       "src_stereo_48k_d",
			FileName: "src_stereo_48k_d.wav",
			Tone: ToneConfig{
				ID:                         "src_stereo_48k_d",
				SampleRate:                 48000,
				Channels:                   2,
				DurationMS:                 150,
				FrequencyHz:                880,
				AmplitudeNumerator:         1,
				AmplitudeDenominator:       6,
				SecondChannelGainNumerator: 3,
				PhaseOffsetQ15:             q15Pi / 4,
			},
		},
	}

	timelines := []TimelineSpec{
		{
			// Upsampling mono source into a stereo timeline with a crossfaded
			// native-rate stereo tail: exercises resampling, mono->stereo mapping,
			// equal-power fade and normalization together.
			ID:             "case_resample_stereo",
			Name:           "resampled mono into stereo with crossfade",
			SampleRate:     48000,
			OutputChannels: 2,
			TargetPeakDBFS: -1,
			Spans: []SpanSpec{
				{SourceID: "src_mono_44k1_c", SourceStartFrame: 0, SourceEndFrame: 12000, GainDB: -3, CrossfadeFrames: 0},
				{SourceID: "src_stereo_48k_d", SourceStartFrame: 0, SourceEndFrame: 7200, GainDB: 0, CrossfadeFrames: 2000},
			},
		},
		{
			// Mirrors the production smoke path but on the shared, digest-pinned
			// fixtures: 44.1k stereo into a 48k timeline with positive gain and a
			// long crossfade, which is where float evaluation order differences
			// would most easily accumulate.
			ID:             "case_crossfade_gain",
			Name:           "crossfaded narration with gain adjustment",
			SampleRate:     48000,
			OutputChannels: 2,
			TargetPeakDBFS: -3,
			Spans: []SpanSpec{
				{SourceID: "src_mono_48k_a", SourceStartFrame: 0, SourceEndFrame: 19200, GainDB: -2, CrossfadeFrames: 0},
				{SourceID: "src_stereo_44k1_b", SourceStartFrame: 0, SourceEndFrame: 15435, GainDB: 1, CrossfadeFrames: 4800},
			},
		},
		{
			// Down-mixing timeline at 44.1k with three spans: stereo->mono,
			// interior sub-interval selection, and a downsampled stereo tail. It
			// covers every channel-mapping direction and a non-zero start frame.
			ID:             "case_stereo_to_mono",
			Name:           "three-span stereo to mono downmix",
			SampleRate:     44100,
			OutputChannels: 1,
			TargetPeakDBFS: -2,
			Spans: []SpanSpec{
				{SourceID: "src_stereo_44k1_b", SourceStartFrame: 0, SourceEndFrame: 11025, GainDB: 0, CrossfadeFrames: 0},
				{SourceID: "src_mono_44k1_c", SourceStartFrame: 2000, SourceEndFrame: 13230, GainDB: -1.5, CrossfadeFrames: 2400},
				{SourceID: "src_stereo_48k_d", SourceStartFrame: 0, SourceEndFrame: 7200, GainDB: 0, CrossfadeFrames: 1500},
			},
		},
	}

	return Suite{
		SchemaVersion: suiteSchemaVersion,
		SuiteID:       suiteID,
		Sources:       sources,
		Timelines:     timelines,
	}
}
