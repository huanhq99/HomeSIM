package remotevoice

const (
	SampleRate   = 8000
	FrameMillis  = 20
	FrameSamples = SampleRate * FrameMillis / 1000
	FrameBytes   = FrameSamples * 2
)

// EncodePCMU converts signed linear PCM to G.711 mu-law (ITU-T G.711).
func EncodePCMU(pcm []int16) []byte {
	out := make([]byte, len(pcm))
	for i, sample := range pcm {
		out[i] = encodeSample(sample)
	}
	return out
}

// DecodePCMU converts G.711 mu-law to signed linear PCM.
func DecodePCMU(pcmu []byte) []int16 {
	out := make([]int16, len(pcmu))
	for i, sample := range pcmu {
		out[i] = decodeSample(sample)
	}
	return out
}

func encodeSample(sample int16) byte {
	const (
		bias = 0x84
		clip = 32635
	)

	pcm := int(sample)
	mask := byte(0xff)
	if pcm < 0 {
		pcm = -pcm
		mask = 0x7f
	}
	if pcm > clip {
		pcm = clip
	}
	pcm += bias

	segment := 7
	for bound, candidate := 0x100, 0; candidate < 8; candidate, bound = candidate+1, bound<<1 {
		if pcm < bound {
			segment = candidate
			break
		}
	}
	mantissa := (pcm >> (segment + 3)) & 0x0f
	return byte((segment<<4)|mantissa) ^ mask
}

func decodeSample(sample byte) int16 {
	u := ^sample
	t := ((int(u) & 0x0f) << 3) + 0x84
	t <<= (u & 0x70) >> 4
	if u&0x80 != 0 {
		return int16(0x84 - t)
	}
	return int16(t - 0x84)
}
