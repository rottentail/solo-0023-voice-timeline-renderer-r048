// Package wavio is a deliberately minimal, standalone RIFF/WAVE PCM16 codec
// used ONLY by the cross-architecture verification harness.
//
// It must not import any production package (audiocodec, render, ...): the
// compatibility judge is required to be independent of the production audio
// algorithms whose cross-architecture behavior is under test. Everything here
// is integer arithmetic over the on-disk PCM16 representation plus direct
// bytes.Buffer/encoding/binary use, so it has no floating-point behavior that
// could diverge between amd64 and arm64.
package xarch

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	pcm16HeaderSize = 44
	pcm16FormatCode = 1
	pcm16Bits       = 16
)

// PCM16File is the decoded view of a 16-bit integer PCM WAVE file. Samples are
// kept as their raw signed integer codes: no float conversion happens here.
type PCM16File struct {
	SampleRate int
	Channels   int
	// Samples holds interleaved signed 16-bit codes, length FrameCount*Channels.
	Samples []int16
}

// FrameCount returns the number of interleaved frames.
func (f *PCM16File) FrameCount() int64 {
	if f.Channels <= 0 {
		return 0
	}
	return int64(len(f.Samples) / f.Channels)
}

// EncodePCM16 writes a canonical 44-byte-header RIFF/WAVE file containing the
// supplied integer PCM codes. The output bytes depend only on the inputs.
func EncodePCM16(sampleRate, channels int, samples []int16) ([]byte, error) {
	if sampleRate < 8000 || sampleRate > 192000 {
		return nil, errors.New("sample rate is outside the accepted range")
	}
	if channels != 1 && channels != 2 {
		return nil, errors.New("channel count must be one or two")
	}
	if len(samples) == 0 || len(samples)%channels != 0 {
		return nil, errors.New("samples are empty or not frame-aligned")
	}
	dataBytes := uint64(len(samples)) * 2
	if dataBytes > uint64(^uint32(0))-36 {
		return nil, errors.New("encoded PCM payload is too large")
	}
	blockAlign := channels * 2
	totalBytes := pcm16HeaderSize + int(dataBytes)

	buf := bytes.NewBuffer(make([]byte, 0, totalBytes))
	write := func(value any) {
		_ = binary.Write(buf, binary.LittleEndian, value)
	}
	write([4]byte{'R', 'I', 'F', 'F'})
	write(uint32(totalBytes - 8))
	write([4]byte{'W', 'A', 'V', 'E'})
	write([4]byte{'f', 'm', 't', ' '})
	write(uint32(16))
	write(uint16(pcm16FormatCode))
	write(uint16(channels))
	write(uint32(sampleRate))
	write(uint32(sampleRate * blockAlign))
	write(uint16(blockAlign))
	write(uint16(pcm16Bits))
	write([4]byte{'d', 'a', 't', 'a'})
	write(uint32(dataBytes))
	for _, sample := range samples {
		write(sample)
	}
	return buf.Bytes(), nil
}

// DecodePCM16 parses the subset of RIFF/WAVE that the service itself emits and
// accepts. It performs strict structural checks so the judge can fail loudly on
// a container whose meaning changed, instead of silently comparing samples from
// differently shaped files.
func DecodePCM16(data []byte) (*PCM16File, error) {
	if len(data) < pcm16HeaderSize {
		return nil, errors.New("file is shorter than a RIFF/WAVE header")
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errors.New("file is not a RIFF/WAVE payload")
	}
	riffSize := binary.LittleEndian.Uint32(data[4:8])
	if uint64(riffSize)+8 != uint64(len(data)) {
		return nil, fmt.Errorf("RIFF size %d does not match payload length %d", riffSize, len(data)-8)
	}

	var (
		formatChunk []byte
		dataChunk   []byte
	)
	offset := 12
	for offset+8 <= len(data) {
		chunkID := string(data[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		payloadStart := offset + 8
		payloadEnd := payloadStart + chunkSize
		if chunkSize < 0 || payloadEnd < payloadStart || payloadEnd > len(data) {
			return nil, fmt.Errorf("chunk %q exceeds the payload", chunkID)
		}
		switch chunkID {
		case "fmt ":
			formatChunk = data[payloadStart:payloadEnd]
		case "data":
			dataChunk = data[payloadStart:payloadEnd]
		}
		offset = payloadEnd
		if chunkSize%2 == 1 {
			offset++
		}
	}
	if len(formatChunk) < 16 {
		return nil, errors.New("fmt chunk is missing or too short")
	}
	if dataChunk == nil {
		return nil, errors.New("data chunk is missing")
	}

	audioFormat := binary.LittleEndian.Uint16(formatChunk[0:2])
	channels := int(binary.LittleEndian.Uint16(formatChunk[2:4]))
	sampleRate := int(binary.LittleEndian.Uint32(formatChunk[4:8]))
	byteRate := binary.LittleEndian.Uint32(formatChunk[8:12])
	blockAlign := int(binary.LittleEndian.Uint16(formatChunk[12:14]))
	bitsPerSample := int(binary.LittleEndian.Uint16(formatChunk[14:16]))

	if audioFormat != pcm16FormatCode {
		return nil, fmt.Errorf("unsupported WAVE format code %d", audioFormat)
	}
	if bitsPerSample != pcm16Bits {
		return nil, fmt.Errorf("unsupported PCM width %d", bitsPerSample)
	}
	if channels != 1 && channels != 2 {
		return nil, fmt.Errorf("unsupported channel count %d", channels)
	}
	if sampleRate < 8000 || sampleRate > 192000 {
		return nil, fmt.Errorf("sample rate %d is outside the accepted range", sampleRate)
	}
	if blockAlign != channels*2 {
		return nil, errors.New("block alignment does not match channel count and PCM width")
	}
	if byteRate != uint32(sampleRate*blockAlign) {
		return nil, errors.New("byte rate does not match sample rate and block alignment")
	}
	if len(dataChunk) == 0 || len(dataChunk)%blockAlign != 0 {
		return nil, errors.New("sample data is empty or not frame-aligned")
	}

	samples := make([]int16, len(dataChunk)/2)
	for index := range samples {
		samples[index] = int16(binary.LittleEndian.Uint16(dataChunk[index*2 : index*2+2]))
	}
	return &PCM16File{SampleRate: sampleRate, Channels: channels, Samples: samples}, nil
}
