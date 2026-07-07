// Package mtxi defines the "mtxi" MP4 box: per-segment metadata written into
// the fMP4 init (ftyp+moov) UserData, used to concatenate segments precisely
// during playback. It is a straight port of MediaMTX's recordstore box.
package mtxi

import (
	amp4 "github.com/abema/go-mp4"
)

func boxType() amp4.BoxType { return amp4.StrToBoxType("mtxi") }

func init() { //nolint:gochecknoinits
	amp4.AddBoxDef(&Box{}, 0)
}

// Box is a MediaMTX-compatible segment info box.
type Box struct {
	amp4.FullBox  `mp4:"0,extend"`
	StreamID      [16]byte `mp4:"1,size=8"`
	SegmentNumber uint64   `mp4:"2,size=64"`
	DTS           int64    `mp4:"3,size=64"`
	NTP           int64    `mp4:"4,size=64"`
}

// GetType implements amp4.IBox.
func (*Box) GetType() amp4.BoxType {
	return boxType()
}

// Find returns the mtxi box from a list of UserData boxes, or nil.
func Find(userData []amp4.IBox) *Box {
	for _, b := range userData {
		if i, ok := b.(*Box); ok {
			return i
		}
	}
	return nil
}
