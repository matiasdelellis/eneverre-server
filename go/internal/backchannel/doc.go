// Package backchannel sends real-time audio to an RTSP camera's ONVIF Profile T
// two-way-audio backchannel. A Session is opened per camera by the caller.
//
// The pipeline is: caller feeds native-rate mono S16LE PCM via FeedPCM →
// anti-alias low-pass + linear resample to 8 kHz → G.711 (A-law/µ-law) encode →
// 160-sample RTP frames every 20 ms. Cameras that advertise an MPEG4-GENERIC or
// an Opus track instead take raw access units via FeedAU or raw 20 ms packets
// via FeedOpus, forwarded unclocked as they arrive.
//
// RTSP and RTP are gortsplib's: the client handles DESCRIBE/SETUP/PLAY with the
// ONVIF Require header, digest auth, interleaved framing, keepalives and the
// periodic RTCP sender reports, and its per-format encoders do the
// packetization. What stays here is the part cameras disagree on — which track
// and codec to talk on (sdp.go), the resampler and the G.711 tables.
//
// Trace logging goes through slog at debug level, so run with
// ENEVERRE_LOG_LEVEL=debug to see the RTSP exchange.
//
// The implementation is split across g711.go, aac.go, resample.go, sdp.go and
// session.go.
package backchannel
