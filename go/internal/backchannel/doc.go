// Package backchannel sends real-time audio to an RTSP camera's ONVIF Profile T
// two-way-audio backchannel. It is a library port of the standalone web2rtsp
// proof of concept: the single-session globals, HTTP server, WebSocket handling
// and flag parsing are gone — a Session is opened per camera by the caller.
//
// The pipeline is: caller feeds native-rate mono S16LE PCM via FeedPCM →
// anti-alias low-pass + linear resample to 8 kHz → G.711 (A-law/µ-law) encode →
// 160-sample RTP frames every 20 ms → RTSP interleaved ($-framing, channel 0)
// over the same TCP connection as the RTSP control messages.
//
// Everything is hand-implemented with the standard library (no external RTSP or
// G.711 dependency). Trace logging goes through slog at debug level, so run with
// ENEVERRE_LOG_LEVEL=debug to see the RTSP exchange.
package backchannel
