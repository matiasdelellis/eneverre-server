import { token } from "../api.js";

// WebSocket subprotocol the server expects; the access token is offered as a
// second subprotocol value so it never appears in the URL (and thus never in
// reverse-proxy access logs). Mirrors talkSubprotocol in handlers_talk.go.
const TALK_SUBPROTOCOL = "eneverre-talk";

// Keep capturing this long after stop() before tearing down, so the tail of
// speech is delivered: the ScriptProcessor only emits full blocks, so the
// in-progress block (plus anything still queued on the socket) would otherwise
// be dropped mid-word.
const TAIL_GRACE_MS = 250;

// createTalkClient builds a push-to-talk client for one camera, driving an
// already-acquired mic MediaStream. The caller owns that stream's lifecycle (it
// is armed/released by the topbar control in views/talk.js), so this client
// never calls getUserMedia and never stops the stream's tracks — separating the
// slow mic-permission step from the connection means start() only pays the
// WebSocket + RTSP dial. start() opens the /talk WebSocket and streams mono
// S16LE PCM at the browser's native sample rate (the server resamples to 8 kHz
// and relays G.711 to the camera's ONVIF backchannel); stop() closes the socket
// but leaves the mic running. onReady fires once the server signals the camera
// backchannel is live (UI: connecting → talking); onEnd fires once when the
// session ends for any reason (server close, error, stop).
export function createTalkClient(camId, { stream, onReady, onEnd } = {}) {
  let ws = null;
  let ctx = null;
  let source = null;
  let processor = null;
  let active = false;
  let graceTimer = null;

  function cleanup() {
    if (graceTimer) { clearTimeout(graceTimer); graceTimer = null; }
    if (!active && !ws && !ctx) return;
    active = false;
    try { if (processor) { processor.onaudioprocess = null; processor.disconnect(); } } catch {}
    try { if (source) source.disconnect(); } catch {}
    try { if (ctx) ctx.close(); } catch {}
    try { if (ws && ws.readyState <= WebSocket.OPEN) ws.close(1000, "stop"); } catch {}
    processor = source = ctx = ws = null;
    // The mic `stream` is owned by the caller (kept live while talk is armed),
    // so its tracks are deliberately left running here.
    if (typeof onEnd === "function") onEnd();
  }

  function start() {
    // A quick re-press cancels a pending stop grace and keeps the session alive.
    if (graceTimer) { clearTimeout(graceTimer); graceTimer = null; }
    if (active || !stream) return;
    active = true;
    ctx = new (window.AudioContext || window.webkitAudioContext)();

    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${proto}//${location.host}/api/camera/${encodeURIComponent(camId)}/talk`;
    const t = token();
    const protocols = t ? [TALK_SUBPROTOCOL, t] : [TALK_SUBPROTOCOL];

    ws = new WebSocket(url, protocols);
    ws.binaryType = "arraybuffer";
    ws.onopen = () => {
      ws.send(JSON.stringify({ sampleRate: ctx.sampleRate }));
      source = ctx.createMediaStreamSource(stream);
      processor = ctx.createScriptProcessor(4096, 1, 1);
      processor.onaudioprocess = (e) => {
        if (!ws || ws.readyState !== WebSocket.OPEN) return;
        const input = e.inputBuffer.getChannelData(0);
        const pcm = new Int16Array(input.length);
        for (let i = 0; i < input.length; i++) {
          const s = Math.max(-1, Math.min(1, input[i]));
          pcm[i] = s < 0 ? s * 0x8000 : s * 0x7fff;
        }
        ws.send(pcm.buffer);
      };
      // Route through a muted gain node so the mic never plays back locally
      // (avoids echo) while still driving the ScriptProcessor.
      const mute = ctx.createGain();
      mute.gain.value = 0;
      source.connect(processor);
      processor.connect(mute);
      mute.connect(ctx.destination);
    };
    ws.onmessage = (ev) => {
      // The server sends one text message {"status":"ready"} once the camera
      // backchannel is live; everything else on this socket is ignorable.
      if (typeof ev.data !== "string") return;
      try {
        if (JSON.parse(ev.data).status === "ready" && typeof onReady === "function") onReady();
      } catch {}
    };
    ws.onclose = cleanup;
    ws.onerror = cleanup;
  }

  // stop keeps capturing for a short grace window before tearing down, so the
  // tail of speech (the in-progress capture block, plus anything still queued on
  // the socket) is delivered instead of being clipped.
  function stop() {
    if (!active) { cleanup(); return; }
    if (graceTimer) return;
    graceTimer = setTimeout(() => { graceTimer = null; cleanup(); }, TAIL_GRACE_MS);
  }

  return { start, stop, isActive: () => active };
}
