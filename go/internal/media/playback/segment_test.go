package playback

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func box(typ string, size uint32, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(b, size)
	copy(b[4:], typ)
	return append(b, payload...)
}

// Corrupt segment headers must come back as errors, not panics or huge
// allocations: a truncated file from a crash shouldn't take down playback.
func TestReadHeaderRejectsCorruptSizes(t *testing.T) {
	ftyp := box("ftyp", 16, make([]byte, 8))
	for name, data := range map[string][]byte{
		"ftyp size below header": box("ftyp", 4, nil),
		"moov size below header": append(append([]byte{}, ftyp...), box("moov", 4, nil)...),
		"moov size huge":         append(append([]byte{}, ftyp...), box("moov", 0xFFFFFFF0, nil)...),
	} {
		if _, _, err := readHeader(bytes.NewReader(data)); err == nil {
			t.Errorf("%s: readHeader succeeded, want an error", name)
		}
	}
}
