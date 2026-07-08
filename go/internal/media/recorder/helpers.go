package recorder

import "time"

func multiplyAndDivide(v, m, d int64) int64 {
	secs := v / d
	dec := v % d
	return secs*m + dec*m/d
}

func timestampToDuration(t int64, clockRate int) time.Duration {
	return time.Duration(multiplyAndDivide(t, int64(time.Second), int64(clockRate)))
}
