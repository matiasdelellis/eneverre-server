# Two-way audio (push-to-talk) — client protocol

This document describes the `talk` endpoint that streams a client's microphone to
a camera's ONVIF Profile T backchannel, and how to implement it in the Android
client. The server side lives in `go/internal/server/handlers_talk.go` and
`go/internal/backchannel/`.

## Overview

```
[Android client] ──WebSocket (PCM S16LE)──► [Eneverre] ──RTP/G.711──► [Camera backchannel]
```

The client captures mono PCM and streams it over a WebSocket. Eneverre resamples
to 8 kHz, encodes G.711 (A-law/µ-law, chosen from the camera's SDP) and relays it
to the camera over the RTSP backchannel. The client only sends audio; it does not
receive camera audio on this socket (to *hear* the camera, use the normal live
stream — HLS/WebRTC via MediaMTX).

## Endpoint

```
GET wss://<host>/api/camera/{camera_id}/talk        (ws:// for plain HTTP)
```

- One active session **per camera**. A second concurrent client is rejected.
- `{camera_id}` is the camera `id` from `GET /api/cameras`.

## Capability discovery

Only cameras whose INI defines a `backchannel` URL support this. Discover it from
the camera list:

```
GET /api/cameras   →   [ { "id": "galeria", "capabilities": { "talk": true, ... }, ... } ]
```

Show the push-to-talk button only when `capabilities.talk == true`.

## Authentication

The access token (the same Bearer token used for the REST API) authenticates the
WebSocket, validated **before** the upgrade. Three ways, in the order the server
checks them:

1. **`Sec-WebSocket-Protocol` subprotocol carrier** — the client offers
   `["eneverre-talk", "<token>"]`; the server echoes only `eneverre-talk`. This
   is what the **browser** uses, because browsers cannot set an `Authorization`
   header on a WebSocket upgrade. It keeps the token out of the URL and out of
   reverse-proxy access logs.
2. **`?token=<token>` query param** — simple, but the token ends up in the URL
   and in proxy logs. Avoid it in production.
3. **`Authorization: Bearer <token>` header** — standard; not usable from a
   browser WebSocket, but **fully usable from Android** (OkHttp can set it).

> **Android should use option 3 (Bearer header).** It is the simplest and keeps
> the token out of the URL/logs, with no subprotocol handling needed.

An invalid/missing token → HTTP `401` and the WebSocket never opens.

## Message flow

```
Client                                  Server
  │                                        │
  │──── (WS upgrade, auth) ───────────────►│  101 Switching Protocols
  │                                        │  (server dials the camera, ~1 s)
  │──── JSON {"sampleRate": N} ───────────►│  (1) Handshake
  │                                        │
  │◄─── JSON {"status":"ready"} ───────────│  (2) Backchannel is live → speak now
  │                                        │
  │──── binary S16LE PCM ─────────────────►│  (3) Audio (repeated)
  │──── binary S16LE PCM ─────────────────►│
  │              ...                       │
  │◄─── ping ──────────────────────────────│  (4) Keepalive every 25 s
  │──── pong ─────────────────────────────►│      (auto-handled by OkHttp)
  │                                        │
  │──── Close ────────────────────────────►│  (5) Release (TEARDOWN to camera)
```

1. **Handshake** — immediately after the socket opens, send a **text** message
   with the capture sample rate: `{"sampleRate": 16000}`. Must be ≥ 8000; mono
   only. Prefer a low rate — see [Bandwidth](#bandwidth).
2. **Ready** — the server sends exactly one **text** message `{"status":"ready"}`
   once the RTSP backchannel is live (the dial takes ~1 s). Use it to switch the
   UI from "connecting" to "talking" so the user does not clip the first second
   of speech. (Audio sent before `ready` is buffered, not lost, but the camera
   isn't playing it yet.)
3. **Audio** — send **binary** messages of raw mono **S16LE** PCM at the sample
   rate you announced. Any chunk size; the server frames it into 20 ms packets.
4. **Keepalive** — the server pings every 25 s and drops the session if no pong
   or audio arrives within 60 s (reclaims the camera slot from dead clients).
   OkHttp answers pings with pongs automatically — nothing to do.
5. **Close** — closing the WebSocket ends the session; the server sends TEARDOWN
   to the camera and frees the per-camera slot.

## Audio format

| Stage | Format |
|---|---|
| Client → server | **PCM S16LE, mono**, any sample rate **≥ 8000 Hz** (16000 recommended — see below) |
| Server internal | anti-alias low-pass → linear resample to **8000 Hz** |
| Server → camera | **G.711** A-law (PCMA) or µ-law (PCMU) at 8 kHz, auto-selected from the camera SDP |

Android's `AudioRecord` with `ENCODING_PCM_16BIT` already produces little-endian
16-bit PCM, so its bytes can be sent verbatim — no conversion needed.

## Bandwidth

The client→server leg is **uncompressed** PCM, so its bit rate is just
`sampleRate × 16 bits`:

| Capture rate | Uplink | Notes |
|---|---|---|
| 48000 Hz | ~768 kbps | wasteful — see below |
| 16000 Hz | ~256 kbps | **recommended** |
| 8000 Hz | ~128 kbps | minimum; matches the sink exactly |

The server resamples everything to **8 kHz** and encodes **G.711 (64 kbps)** for
the camera anyway, so capturing at 48 kHz sends ~12× more data than the result
needs **with no audio-quality gain** — the camera plays 8 kHz telephony audio
regardless. Capture at a low rate:

- **16000 Hz** is the sweet spot: ~⅓ the bandwidth of 48 kHz, broad device
  support, and it gives the server's anti-alias filter clean input. No audible
  loss vs 48 kHz for this use case.
- **8000 Hz** is the absolute minimum and matches the final sink one-to-one;
  use it on constrained cellular uplinks if `AudioRecord` supports it on the
  device.

Set `SAMPLE_RATE` accordingly in the examples below and announce the same value
in the handshake. (Push-to-talk is bursty — bandwidth is only spent while the
button is held — but on a weak mobile uplink the lower rate still matters.)

## Errors

Before the upgrade (HTTP status; surfaces in OkHttp's `onFailure` with the
`Response`):

| Status | Meaning |
|---|---|
| `401` | Missing/invalid token |
| `404` | Camera has no two-way audio (`capabilities.talk == false`) or unknown id |
| `409` | Another client already has an active talk session for this camera |

If the camera backchannel fails to come up, the server accepts the upgrade and
then closes the socket with a close reason `RTSP error: ...` (surfaces in
`onClosing`/`onClosed`).

## Android implementation (Kotlin + OkHttp)

### Dependencies

```kotlin
implementation("com.squareup.okhttp3:okhttp:4.12.0")
```

### Permissions (AndroidManifest.xml)

```xml
<uses-permission android:name="android.permission.RECORD_AUDIO" />
<uses-permission android:name="android.permission.INTERNET" />
```

Request `RECORD_AUDIO` at runtime (API 23+) before starting.

### Push-to-talk client

```kotlin
import android.media.AudioFormat
import android.media.AudioRecord
import android.media.MediaRecorder
import okhttp3.*
import okio.ByteString.Companion.toByteString
import kotlin.concurrent.thread

/**
 * Streams the mic to wss://<host>/api/camera/<camId>/talk while active.
 * @param baseWsUrl e.g. "wss://nvr.delellis.com.ar"
 * @param token     the Bearer access token used for the REST API
 */
class TalkClient(
    private val baseWsUrl: String,
    private val camId: String,
    private val token: String,
    private val onReady: () -> Unit = {},
    private val onEnd: (reason: String?) -> Unit = {},
) {
    private companion object {
        const val SAMPLE_RATE = 16000   // low rate → less uplink; see "Bandwidth"
    }

    private val client = OkHttpClient.Builder()
        // Optional: also detect a dead server from the client side.
        .pingInterval(25, java.util.concurrent.TimeUnit.SECONDS)
        .build()

    private var ws: WebSocket? = null
    private var recorder: AudioRecord? = null
    @Volatile private var running = false

    fun start() {
        val request = Request.Builder()
            .url("$baseWsUrl/api/camera/$camId/talk")
            // Android can set the Authorization header directly — no token in URL.
            .header("Authorization", "Bearer $token")
            .build()

        ws = client.newWebSocket(request, object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                // 1. Handshake with the capture rate.
                webSocket.send("""{"sampleRate": $SAMPLE_RATE}""")
                startRecording(webSocket)
            }

            override fun onMessage(webSocket: WebSocket, text: String) {
                // 2. Server signals the camera backchannel is live.
                if (text.contains("\"ready\"")) onReady()
            }

            override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                stop(); onEnd(reason)
            }

            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                stop(); onEnd(reason)
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                // response?.code is 401 / 404 / 409 for auth / capability / busy.
                stop(); onEnd(response?.let { "HTTP ${it.code}" } ?: t.message)
            }
        })
    }

    @Suppress("MissingPermission")
    private fun startRecording(webSocket: WebSocket) {
        val minBuf = AudioRecord.getMinBufferSize(
            SAMPLE_RATE, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT
        )
        recorder = AudioRecord(
            MediaRecorder.AudioSource.MIC,
            SAMPLE_RATE, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT,
            minBuf
        ).also { it.startRecording() }
        running = true

        thread(name = "talk-mic") {
            val buf = ByteArray(minBuf)
            while (running) {
                val n = recorder?.read(buf, 0, buf.size) ?: -1
                if (n > 0) {
                    // AudioRecord gives little-endian S16 — send the bytes as-is.
                    webSocket.send(buf.toByteString(0, n))
                }
            }
        }
    }

    /** Idempotent; safe to call from any callback or on button release. */
    fun stop() {
        running = false
        recorder?.apply { runCatching { stop() }; release() }
        recorder = null
        ws?.close(1000, "user released")
        ws = null
    }
}
```

### Hooking up the button (hold-to-talk)

```kotlin
val talk = TalkClient(
    baseWsUrl = "wss://nvr.delellis.com.ar",
    camId = camera.id,
    token = session.accessToken,
    onReady = { runOnUiThread { button.text = "🔴 Talking — go ahead" } },
    onEnd = { runOnUiThread { button.text = "🎤 Hold to talk" } },
)

button.setOnTouchListener { _, event ->
    when (event.action) {
        MotionEvent.ACTION_DOWN -> {
            button.text = "⏳ Connecting…"
            talk.start(); true
        }
        MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL -> {
            talk.stop(); true
        }
        else -> false
    }
}
```

### Java equivalent

Same behaviour as the Kotlin client above.

```java
import android.media.AudioFormat;
import android.media.AudioRecord;
import android.media.MediaRecorder;
import androidx.annotation.Nullable;
import java.util.concurrent.TimeUnit;
import okhttp3.*;
import okio.ByteString;

/** Streams the mic to wss://<host>/api/camera/<camId>/talk while active. */
public class TalkClient {

    /** UI callbacks (invoked on OkHttp threads — marshal to the UI thread). */
    public interface Listener {
        void onReady();
        void onEnd(@Nullable String reason);
    }

    private static final int SAMPLE_RATE = 16000;   // low rate → less uplink; see "Bandwidth"

    private final String baseWsUrl;   // e.g. "wss://nvr.delellis.com.ar"
    private final String camId;
    private final String token;       // Bearer access token used for the REST API
    private final Listener listener;

    private final OkHttpClient client = new OkHttpClient.Builder()
            // Optional: also detect a dead server from the client side.
            .pingInterval(25, TimeUnit.SECONDS)
            .build();

    private WebSocket ws;
    private AudioRecord recorder;
    private volatile boolean running;

    public TalkClient(String baseWsUrl, String camId, String token, Listener listener) {
        this.baseWsUrl = baseWsUrl;
        this.camId = camId;
        this.token = token;
        this.listener = listener;
    }

    public void start() {
        Request request = new Request.Builder()
                .url(baseWsUrl + "/api/camera/" + camId + "/talk")
                // Android can set the Authorization header directly — no token in URL.
                .header("Authorization", "Bearer " + token)
                .build();

        ws = client.newWebSocket(request, new WebSocketListener() {
            @Override
            public void onOpen(WebSocket webSocket, Response response) {
                // 1. Handshake with the capture rate.
                webSocket.send("{\"sampleRate\": " + SAMPLE_RATE + "}");
                startRecording(webSocket);
            }

            @Override
            public void onMessage(WebSocket webSocket, String text) {
                // 2. Server signals the camera backchannel is live.
                if (text.contains("\"ready\"")) listener.onReady();
            }

            @Override
            public void onClosing(WebSocket webSocket, int code, String reason) {
                stop();
                listener.onEnd(reason);
            }

            @Override
            public void onClosed(WebSocket webSocket, int code, String reason) {
                stop();
                listener.onEnd(reason);
            }

            @Override
            public void onFailure(WebSocket webSocket, Throwable t, @Nullable Response response) {
                // response.code() is 401 / 404 / 409 for auth / capability / busy.
                stop();
                listener.onEnd(response != null ? "HTTP " + response.code() : t.getMessage());
            }
        });
    }

    @SuppressWarnings("MissingPermission")
    private void startRecording(final WebSocket webSocket) {
        int minBuf = AudioRecord.getMinBufferSize(
                SAMPLE_RATE, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT);
        recorder = new AudioRecord(
                MediaRecorder.AudioSource.MIC,
                SAMPLE_RATE, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT,
                minBuf);
        recorder.startRecording();
        running = true;

        final int bufSize = minBuf;
        new Thread(() -> {
            byte[] buf = new byte[bufSize];
            while (running) {
                int n = recorder != null ? recorder.read(buf, 0, buf.length) : -1;
                if (n > 0) {
                    // AudioRecord gives little-endian S16 — send the bytes as-is.
                    webSocket.send(ByteString.of(buf, 0, n));
                }
            }
        }, "talk-mic").start();
    }

    /** Idempotent; safe to call from any callback or on button release. */
    public void stop() {
        running = false;
        if (recorder != null) {
            try { recorder.stop(); } catch (IllegalStateException ignored) {}
            recorder.release();
            recorder = null;
        }
        if (ws != null) {
            ws.close(1000, "user released");
            ws = null;
        }
    }
}
```

Button hookup (hold-to-talk):

```java
TalkClient talk = new TalkClient(
        "wss://nvr.delellis.com.ar",
        camera.getId(),
        session.getAccessToken(),
        new TalkClient.Listener() {
            @Override public void onReady() {
                runOnUiThread(() -> button.setText("🔴 Talking — go ahead"));
            }
            @Override public void onEnd(@Nullable String reason) {
                runOnUiThread(() -> button.setText("🎤 Hold to talk"));
            }
        });

button.setOnTouchListener((v, event) -> {
    switch (event.getAction()) {
        case MotionEvent.ACTION_DOWN:
            button.setText("⏳ Connecting…");
            talk.start();
            return true;
        case MotionEvent.ACTION_UP:
        case MotionEvent.ACTION_CANCEL:
            talk.stop();
            return true;
        default:
            return false;
    }
});
```

### Notes

- **Wait for `ready` before speaking.** The `onReady` callback marks the moment
  the camera is actually playing your audio; speaking earlier clips the start.
- **Show the button only when `capabilities.talk` is true** (from `/api/cameras`).
- **One session per camera** — expect `409` if someone else is already talking.
- **Half-duplex** — this socket is send-only. Keep the live view playing to hear
  the camera.
- OkHttp responds to the server's pings automatically; the optional
  `pingInterval` above just lets the client also notice a dead server faster.
