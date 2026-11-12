package xarch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
)

// This file builds the registered source fixtures with deterministic,
// architecture-independent integer arithmetic. No float operation is involved:
// phase accumulation and waveform synthesis run in fixed point, and the final
// values are integer PCM16 codes. Regenerating the fixtures on any machine
// therefore produces byte-identical WAV files, which the suite manifest pins
// with SHA-256 digests.

const (
	q15One = 1 << 15
	// q15Pi is the Q15 encoding of pi: round(pi * 32768) = 102944. Declaring
	// the rounded integer literal keeps the entire signal path float-free.
	q15Pi     int32 = 102944
	q15HalfPi       = q15Pi / 2
	// Fixed-point Taylor coefficients, also pre-rounded integer Q15 literals.
	inv42Q15 int32 = 780  // round(32768 / 42)
	inv20Q15 int32 = 1638 // round(32768 / 20)
	inv6Q15  int32 = 5461 // round(32768 / 6)
)

// mulQ15 multiplies two Q15 signed values and returns the Q15 product.
func mulQ15(a, b int32) int32 {
	return int32((int64(a) * int64(b)) >> 15)
}

// sinQ15 evaluates sine for an angle expressed in radians in Q15. The input is
// folded into the first quadrant using trigonometric symmetry, then evaluated
// with a fixed-point Horner form of the Taylor series. All arithmetic is
// integer, so the result is identical on every architecture.
func sinQ15(angleQ15 int32) int16 {
	// Reduce into [0, 2*pi).
	twoPi := int32(2) * q15Pi
	a := angleQ15 % twoPi
	if a < 0 {
		a += twoPi
	}
	negate := false
	switch {
	case a > q15Pi:
		a -= q15Pi
		negate = true
	}
	if a > q15HalfPi {
		a = q15Pi - a
	}
	x := a
	x2 := mulQ15(x, x)
	// sin(x) ~= x * (1 - x^2/6 * (1 - x^2/20 * (1 - x^2/42)))
	term := q15One - mulQ15(mulQ15(x2, inv42Q15), q15One)
	term = q15One - mulQ15(mulQ15(x2, inv20Q15), term)
	term = q15One - mulQ15(mulQ15(x2, inv6Q15), term)
	result := mulQ15(x, term)
	if negate {
		result = -result
	}
	return int16(result)
}

// ToneConfig describes one deterministic PCM16 tone fixture.
type ToneConfig struct {
	ID         string
	FileName   string
	SampleRate int
	Channels   int
	DurationMS int
	// FrequencyHz drives a Q32 phase accumulator: step = 2^32 * f / rate.
	FrequencyHz int
	// Amplitude is a fraction of full scale, expressed as numerator/denominator.
	AmplitudeNumerator   int
	AmplitudeDenominator int
	// SecondChannelGainNumerator scales the stereo right channel relative to the
	// left (ignored for mono). Denominator is AmplitudeDenominator.
	SecondChannelGainNumerator int
	// PhaseOffsetQ15 shifts the right-channel phase for stereo fixtures.
	PhaseOffsetQ15 int32
}

// FrameCount reports how many frames the fixture contains.
func (c ToneConfig) FrameCount() int {
	return c.SampleRate * c.DurationMS / 1000
}

// RenderTone synthesizes the fixture as raw signed PCM16 codes using a Q32
// phase accumulator. It never allocates floats on the signal path.
func (c ToneConfig) RenderTone() ([]int16, error) {
	if c.Channels != 1 && c.Channels != 2 {
		return nil, errors.New("fixture channel count must be one or two")
	}
	if c.AmplitudeDenominator <= 0 || c.AmplitudeNumerator < 0 || c.AmplitudeNumerator > c.AmplitudeDenominator {
		return nil, errors.New("fixture amplitude fraction is invalid")
	}
	frames := c.FrameCount()
	if frames < 1 {
		return nil, errors.New("fixture has no frames")
	}
	phaseStep := uint32(uint64(1<<32) * uint64(c.FrequencyHz) / uint64(c.SampleRate))
	samples := make([]int16, frames*c.Channels)
	var phase uint32
	for frame := 0; frame < frames; frame++ {
		// Map the Q32 phase fraction to a Q15 radian angle: angle = 2*pi * phase/2^32.
		angle := int32((int64(phase) * int64(q15Pi) * 2) >> 32)
		left := scaleQ15(sinQ15(angle), c.AmplitudeNumerator, c.AmplitudeDenominator)
		if c.Channels == 1 {
			samples[frame] = left
		} else {
			rightAngle := angle + c.PhaseOffsetQ15
			right := scaleQ15(sinQ15(rightAngle), c.AmplitudeNumerator*c.SecondChannelGainNumerator, c.AmplitudeDenominator)
			samples[frame*2] = left
			samples[frame*2+1] = right
		}
		phase += phaseStep
	}
	return samples, nil
}

func scaleQ15(value int16, numerator, denominator int) int16 {
	scaled := int64(value) * int64(numerator) / int64(denominator)
	if scaled > math.MaxInt16 {
		return math.MaxInt16
	}
	if scaled < math.MinInt16 {
		return math.MinInt16
	}
	return int16(scaled)
}

// BuildFixture renders and encodes one fixture and returns the WAV bytes.
func BuildFixture(config ToneConfig) ([]byte, error) {
	samples, err := config.RenderTone()
	if err != nil {
		return nil, fmt.Errorf("render fixture %s: %w", config.ID, err)
	}
	wav, err := EncodePCM16(config.SampleRate, config.Channels, samples)
	if err != nil {
		return nil, fmt.Errorf("encode fixture %s: %w", config.ID, err)
	}
	return wav, nil
}

// SHA256Hex returns the lowercase hex SHA-256 digest of data.
func SHA256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
