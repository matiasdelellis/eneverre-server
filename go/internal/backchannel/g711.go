package backchannel

// G.711 A-law and µ-law encoders (CCITT reference implementations).
// Both take 16-bit linear PCM and produce a single 8-bit companded byte.

var segAEnd = [8]int{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}

func alawSegment(val int) int {
	for i, end := range segAEnd {
		if val <= end {
			return i
		}
	}
	return len(segAEnd)
}

func linearToALaw(pcm int16) byte {
	pcmVal := int(pcm) >> 3
	var mask int
	if pcmVal >= 0 {
		mask = 0xD5
	} else {
		mask = 0x55
		pcmVal = -pcmVal - 1
	}
	seg := alawSegment(pcmVal)
	if seg >= 8 {
		return byte(0x7F ^ mask)
	}
	aval := seg << 4
	if seg < 2 {
		aval |= (pcmVal >> 1) & 0x0F
	} else {
		aval |= (pcmVal >> seg) & 0x0F
	}
	return byte(aval ^ mask)
}

var ulawExpLut = [256]byte{
	0, 0, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 3, 3, 3, 3,
	4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4,
	5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5,
	5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
}

func linearToULaw(pcm int16) byte {
	const bias = 0x84
	const clip = 32635
	pcmVal := int(pcm)
	sign := 0
	if pcmVal < 0 {
		pcmVal = -pcmVal
		sign = 0x80
	}
	if pcmVal > clip {
		pcmVal = clip
	}
	pcmVal += bias
	exponent := int(ulawExpLut[(pcmVal>>7)&0xFF])
	mantissa := (pcmVal >> (exponent + 3)) & 0x0F
	return byte(^(sign | (exponent << 4) | mantissa))
}

func encodeALaw(samples []int16) []byte {
	out := make([]byte, len(samples))
	for i, s := range samples {
		out[i] = linearToALaw(s)
	}
	return out
}

func encodeULaw(samples []int16) []byte {
	out := make([]byte, len(samples))
	for i, s := range samples {
		out[i] = linearToULaw(s)
	}
	return out
}
