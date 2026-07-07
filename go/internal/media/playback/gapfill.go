package playback

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)
// gapFillCacheSubdir is the subfolder, under the configured cache dir, where the
// generated black-frame assets are persisted (one Annex-B .h264 per resolution).
const gapFillCacheSubdir = "gapfill"

// Gap filling: when a download (/get) spans a coverage gap, instead of stopping
// at the gap (which makes the clip look trimmed), we splice in a black
// "SIN GRABACIÓN" H264 frame that occupies the real gap time. The download is
// emitted as avc3 (in-band parameter sets) so the decoder accepts the black
// frame's SPS/PPS alongside the recording's — recordings already carry SPS/PPS
// in every keyframe (the recorder injects them), so both sides are in-band.

// blackFrameCache caches the finished black IDR sample payload per cache key
// (resolution + message hash); blackFrameInflight tracks a generation already in
// progress so concurrent callers for the same key wait on it instead of each
// running ffmpeg. The mutex is only ever held for map bookkeeping — never across
// the ffmpeg run — so a cold 4K generation doesn't block /get for other keys.
var (
	blackFrameMu       sync.Mutex
	blackFrameCache    = map[string][]byte{}
	blackFrameInflight = map[string]*blackFrameCall{}
)

// blackFrameCall is an in-progress generation others can wait on.
type blackFrameCall struct {
	done    chan struct{}
	payload []byte
	err     error
}

// blackFramePayload returns an H264 IDR access unit (SPS+PPS+SEI+IDR) as an
// avcc (length-prefixed) sample payload: a black frame captioned with `message`
// at the given resolution. Lookup order: in-memory cache → on-disk cache
// (<cacheDir>/gapfill/<WxH>-<msghash>.h264, Annex-B) → generate with ffmpeg and
// persist. The message is part of the cache key, so changing it regenerates. A
// tiny asset (a few KB). cacheDir "" skips the disk cache (memory only).
func blackFramePayload(cacheDir, message string, width, height int) ([]byte, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("invalid resolution %dx%d", width, height)
	}
	key := fmt.Sprintf("%dx%d-%08x", width, height, crc32.ChecksumIEEE([]byte(message)))

	blackFrameMu.Lock()
	if p, ok := blackFrameCache[key]; ok {
		blackFrameMu.Unlock()
		return p, nil
	}
	if call, ok := blackFrameInflight[key]; ok {
		// Another request is already generating this key: wait for it (without
		// holding the lock) instead of running ffmpeg a second time.
		blackFrameMu.Unlock()
		<-call.done
		return call.payload, call.err
	}
	call := &blackFrameCall{done: make(chan struct{})}
	blackFrameInflight[key] = call
	blackFrameMu.Unlock()

	// Generate outside the lock so concurrent /get for other keys aren't blocked.
	payload, err := buildBlackFrame(cacheDir, key, message, width, height)

	blackFrameMu.Lock()
	if err == nil {
		blackFrameCache[key] = payload
	}
	delete(blackFrameInflight, key) // on error, drop it so a later request retries
	blackFrameMu.Unlock()

	call.payload, call.err = payload, err
	close(call.done)
	return payload, err
}

// buildBlackFrame resolves the black-frame AVCC payload for one cache key: it
// reads the on-disk cache, else generates it with ffmpeg and persists it. It
// holds no locks (ffmpeg may take seconds on large resolutions).
func buildBlackFrame(cacheDir, key, message string, width, height int) ([]byte, error) {
	var diskPath string
	if cacheDir != "" {
		diskPath = filepath.Join(cacheDir, gapFillCacheSubdir, key+".h264")
	}

	var annexb []byte
	if diskPath != "" {
		if b, err := os.ReadFile(diskPath); err == nil && len(b) > 0 {
			annexb = b
		}
	}
	if annexb == nil {
		b, err := ffmpegBlackFrame(message, width, height)
		if err != nil {
			return nil, err
		}
		annexb = b
		if diskPath != "" {
			if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err == nil {
				if err := os.WriteFile(diskPath, annexb, 0o644); err != nil {
					slog.Warn("gapfill: could not cache black frame", "path", diskPath, "err", err)
				}
			}
		}
	}

	payload := annexBToAVCC(annexb)
	if len(payload) == 0 {
		return nil, fmt.Errorf("black frame produced no NAL units")
	}
	return payload, nil
}

// ffmpegBlackFrame generates a single black IDR frame (Annex-B) at the given
// resolution via ffmpeg, captioned with `message` (centered). High profile
// keeps it broadly decodable. The caption is passed via a temp file
// (drawtext textfile=) so any characters — accents, quotes, colons — work
// without filtergraph escaping. An empty message yields a plain black frame.
func ffmpegBlackFrame(message string, width, height int) ([]byte, error) {
	fontSize := height / 12
	if fontSize < 12 {
		fontSize = 12
	}
	args := []string{
		"-v", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("color=black:s=%dx%d:r=1:d=1", width, height),
	}
	if message != "" {
		tf, err := os.CreateTemp("", "gapmsg-*.txt")
		if err != nil {
			return nil, fmt.Errorf("gapfill temp file: %w", err)
		}
		defer os.Remove(tf.Name())
		if _, err := tf.WriteString(message); err != nil {
			tf.Close()
			return nil, err
		}
		tf.Close()
		args = append(args, "-vf",
			fmt.Sprintf("drawtext=textfile=%s:fontcolor=white:fontsize=%d:x=(w-text_w)/2:y=(h-text_h)/2", tf.Name(), fontSize))
	}
	args = append(args,
		"-c:v", "libx264", "-profile:v", "high", "-pix_fmt", "yuv420p", "-frames:v", "1",
		"-bsf:v", "h264_mp4toannexb", "-f", "h264", "-")

	cmd := exec.Command("ffmpeg", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("generate black frame: %w", err)
	}
	return out.Bytes(), nil
}

// annexBToAVCC converts an Annex-B NAL stream to length-prefixed (4-byte) AVCC,
// dropping access-unit delimiters (type 9), which carry no picture data.
func annexBToAVCC(b []byte) []byte {
	var out []byte
	for _, nal := range splitAnnexB(b) {
		if len(nal) == 0 {
			continue
		}
		if nal[0]&0x1f == 9 { // AUD
			continue
		}
		var lp [4]byte
		binary.BigEndian.PutUint32(lp[:], uint32(len(nal)))
		out = append(out, lp[:]...)
		out = append(out, nal...)
	}
	return out
}

// splitAnnexB splits an Annex-B byte stream on 3- and 4-byte start codes.
func splitAnnexB(b []byte) [][]byte {
	var nals [][]byte
	i := 0
	// find first start code
	start := -1
	for i+3 <= len(b) {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			start = i + 3
			i += 3
			break
		}
		i++
	}
	if start < 0 {
		return nil
	}
	for i+3 <= len(b) {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			end := i
			if end > start && b[end-1] == 0 { // 4-byte start code: trim trailing zero
				end--
			}
			nals = append(nals, b[start:end])
			start = i + 3
			i += 3
			continue
		}
		i++
	}
	nals = append(nals, b[start:])
	return nals
}

// spsDimensions parses an SPS and returns its coded width/height.
func spsDimensions(spsBytes []byte) (int, int, bool) {
	var sps h264.SPS
	if err := sps.Unmarshal(spsBytes); err != nil {
		return 0, 0, false
	}
	return sps.Width(), sps.Height(), true
}
