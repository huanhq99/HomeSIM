package remotevoice

import (
	"math"
	"testing"
)

func TestG711KnownVectors(t *testing.T) {
	t.Parallel()
	vectors := []struct {
		linear int16
		muLaw  byte
	}{
		{0, 0xff},
		{1000, 0xce},
		{-1000, 0x4e},
		{32124, 0x80},
		{-32124, 0x00},
	}
	for _, vector := range vectors {
		if got := EncodePCMU([]int16{vector.linear})[0]; got != vector.muLaw {
			t.Errorf("EncodePCMU(%d) = %#02x, want %#02x", vector.linear, got, vector.muLaw)
		}
	}
}

func TestG711RoundTripBoundedError(t *testing.T) {
	t.Parallel()
	input := make([]int16, 0, 511)
	for value := -32000; value <= 32000; value += 127 {
		input = append(input, int16(value))
	}
	decoded := DecodePCMU(EncodePCMU(input))
	if len(decoded) != len(input) {
		t.Fatalf("decoded %d samples, want %d", len(decoded), len(input))
	}
	for i := range input {
		// Mu-law is intentionally lossy. Its maximum quantization step near
		// full scale is 1024, so half a step plus endpoint bias fits in 644.
		if delta := math.Abs(float64(decoded[i]) - float64(input[i])); delta > 644 {
			t.Fatalf("sample %d: input=%d decoded=%d error=%v", i, input[i], decoded[i], delta)
		}
	}
}
