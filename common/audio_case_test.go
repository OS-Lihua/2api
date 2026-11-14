package common

import (
	"bytes"
	"context"
	"math"

	"encoding/binary"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

// minimalWAV builds a valid 16-bit PCM mono WAV with the given number of sample frames.
func minimalWAV(sampleRate uint32, frames int) []byte {
	const numChannels = 1
	const bitsPerSample = 16
	dataSize := frames * numChannels * (bitsPerSample / 8)
	buf := &bytes.Buffer{}
	buf.WriteString("RIFF")
	binary.Write(buf, binary.LittleEndian, uint32(36+dataSize))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(buf, binary.LittleEndian, uint32(16))
	binary.Write(buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(buf, binary.LittleEndian, uint16(numChannels))
	binary.Write(buf, binary.LittleEndian, sampleRate)
	byteRate := sampleRate * numChannels * (bitsPerSample / 8)
	binary.Write(buf, binary.LittleEndian, byteRate)
	binary.Write(buf, binary.LittleEndian, uint16(numChannels*(bitsPerSample/8)))
	binary.Write(buf, binary.LittleEndian, uint16(bitsPerSample))
	buf.WriteString("data")
	binary.Write(buf, binary.LittleEndian, uint32(dataSize))
	buf.Write(make([]byte, dataSize))
	return buf.Bytes()
}

func TestAudioDurationTokensSaturatesUntrustedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seconds float64
		want    int
		clamped bool
	}{
		{"one second", 1, 17, false},
		{"one minute", 60, 1000, false},
		{"fractional second", 0.1, 17, false},
		{"explicit zero", 0, 0, false},
		{"negative", -1, 0, true},
		{"NaN", math.NaN(), 0, true},
		{"infinite", math.Inf(1), math.MaxInt32, true},
		{"huge finite", math.MaxFloat64, math.MaxInt32, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens, clamp := AudioDurationTokens(tc.seconds)
			assert.Equal(t, tc.want, tokens)
			if tc.clamped {
				require.NotNil(t, clamp)
			} else {
				assert.Nil(t, clamp)
			}
		})
	}
}

func TestGetAudioDurationCaseInsensitiveExt(t *testing.T) {
	wav := minimalWAV(8000, 8000) // 1 second

	for _, ext := range []string{".wav", ".WAV", ".Wav"} {
		d, err := GetAudioDuration(context.Background(), bytes.NewReader(wav), ext)
		if err != nil {
			t.Fatalf("ext %q: unexpected error: %v", ext, err)
		}
		if d < 0.99 || d > 1.01 {
			t.Fatalf("ext %q: expected ~1s duration, got %v", ext, d)
		}
	}
}

func TestGetAudioDurationUppercaseNotUnsupported(t *testing.T) {
	_, err := GetAudioDuration(context.Background(), bytes.NewReader([]byte("not audio")), ".MP3")
	if err != nil && strings.Contains(err.Error(), "unsupported audio format") {
		t.Fatalf("uppercase extension rejected as unsupported: %v", err)
	}
}
