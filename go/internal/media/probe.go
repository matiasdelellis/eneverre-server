package media

import (
	"fmt"
	"strings"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
)

// ProbeResult reports what a DESCRIBE against an RTSP source revealed. Width and
// Height are best-effort (parsed from the H264/H265 parameter sets when the
// camera advertises them); they are 0 when the resolution could not be
// determined, which is not an error.
type ProbeResult struct {
	Codecs []string `json:"codecs"`
	Width  int      `json:"width"`
	Height int      `json:"height"`
}

// ProbeSource opens an RTSP connection to url, performs a DESCRIBE, and reports
// the offered codecs and (best-effort) video resolution. It is used by the
// camera-create wizard's "test connection" step to verify reachability and
// credentials before a camera is saved. transport is "auto" | "tcp" | "udp"
// (empty means auto). The whole probe is bounded by timeout so an unreachable
// or silent host fails fast instead of hanging the request.
func ProbeSource(url, transport string, timeout time.Duration) (ProbeResult, error) {
	if strings.TrimSpace(url) == "" {
		return ProbeResult{}, fmt.Errorf("empty source URL")
	}
	if timeout <= 0 {
		timeout = 8 * time.Second
	}

	u, err := base.ParseURL(url)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("invalid RTSP URL: %w", err)
	}

	c := &gortsplib.Client{
		Scheme:        u.Scheme,
		Host:          u.Host,
		ReadTimeout:   timeout,
		WriteTimeout:  timeout,
		OnPacketsLost: func(uint64) {},
		OnDecodeError: func(error) {},
	}
	switch strings.ToLower(strings.TrimSpace(transport)) {
	case "", "auto":
		// leave Protocol nil (UDP with TCP fallback)
	case "tcp":
		p := gortsplib.ProtocolTCP
		c.Protocol = &p
	case "udp":
		p := gortsplib.ProtocolUDP
		c.Protocol = &p
	default:
		return ProbeResult{}, fmt.Errorf("invalid transport %q (want auto|tcp|udp)", transport)
	}

	// Bound the whole probe: gortsplib's per-op timeouts cover a live-but-slow
	// server, but a black-holed host can still stall the initial dial, so run in
	// a goroutine and give up after the deadline regardless.
	type outcome struct {
		res ProbeResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		if err := c.Start(); err != nil {
			done <- outcome{err: fmt.Errorf("connect: %w", err)}
			return
		}
		defer c.Close()
		desc, _, err := c.Describe(u)
		if err != nil {
			done <- outcome{err: fmt.Errorf("describe: %w", err)}
			return
		}
		var res ProbeResult
		for _, m := range desc.Medias {
			for _, f := range m.Formats {
				res.Codecs = append(res.Codecs, f.Codec())
				if w, h, ok := videoSize(f); ok && res.Width == 0 {
					res.Width, res.Height = w, h
				}
			}
		}
		done <- outcome{res: res}
	}()

	select {
	case o := <-done:
		return o.res, o.err
	case <-time.After(timeout + 2*time.Second):
		c.Close()
		return ProbeResult{}, fmt.Errorf("timed out after %s", timeout)
	}
}

// videoSize extracts the coded resolution from an H264/H265 format's parameter
// sets when present. Returns ok=false for audio formats or when the camera did
// not send parameter sets in the SDP (common — the client would learn them from
// the stream instead).
func videoSize(f format.Format) (int, int, bool) {
	switch v := f.(type) {
	case *format.H264:
		if len(v.SPS) == 0 {
			return 0, 0, false
		}
		var sps h264.SPS
		if err := sps.Unmarshal(v.SPS); err != nil {
			return 0, 0, false
		}
		return sps.Width(), sps.Height(), true
	case *format.H265:
		if len(v.SPS) == 0 {
			return 0, 0, false
		}
		var sps h265.SPS
		if err := sps.Unmarshal(v.SPS); err != nil {
			return 0, 0, false
		}
		return sps.Width(), sps.Height(), true
	default:
		return 0, 0, false
	}
}
