package backchannel

// lowPassForDecimation applies a moving-average anti-alias filter to the input
// before decimating. A simple boxcar (rectangular) window is enough for voice
// because the stopband needs only moderate rejection — G.711 at 8 kHz barely
// reproduces 3.4 kHz, so a sharp cutoff is unnecessary. The output length
// matches the input; decimate separately.
func lowPassForDecimation(samples []int16, fromRate, toRate int) []int16 {
	if toRate >= fromRate || len(samples) == 0 {
		return samples
	}
	win := fromRate / toRate
	if win < 2 {
		return samples
	}
	out := make([]int16, len(samples))
	var acc int32
	for i := range samples {
		acc += int32(samples[i])
		if i >= win {
			acc -= int32(samples[i-win])
		}
		n := int32(win)
		if i+1 < win {
			n = int32(i + 1)
		}
		out[i] = int16(acc / n)
	}
	return out
}

// resampleLinear resamples (up or down) a slice of int16 samples from fromRate
// to toRate using linear interpolation. When the sample count is zero it returns
// nil; when fromRate == toRate it returns the original slice (no copy).
func resampleLinear(samples []int16, fromRate, toRate int) []int16 {
	if fromRate == toRate || len(samples) == 0 {
		return samples
	}
	ratio := float64(toRate) / float64(fromRate)
	outLen := int(float64(len(samples)) * ratio)
	if outLen < 1 {
		outLen = 1
	}
	out := make([]int16, outLen)
	for i := 0; i < outLen; i++ {
		fpos := float64(i) / ratio
		idx := int(fpos)
		frac := fpos - float64(idx)
		if idx >= len(samples)-1 {
			out[i] = samples[len(samples)-1]
		} else {
			out[i] = int16(float64(samples[idx])*(1-frac) + float64(samples[idx+1])*frac)
		}
	}
	return out
}
