package thingino

import (
	"encoding/json"
	"testing"
)

// TestHeartbeatDecoding pins the tolerant decoding of the slow-heartbeat
// payload: booleans spelled as bool/number/string, numbers as number/string,
// and odd fields that must not fail the whole payload.
func TestHeartbeatDecoding(t *testing.T) {
	raw := []byte(`{
		"privacy_enabled": "1",
		"daynight_mode": "night",
		"daynight_enabled": true,
		"daynight_brightness": "80",
		"motion_enabled": 0,
		"mic_enabled": false,
		"spk_enabled": "true",
		"rec_ch0": 1,
		"rec_ch1": "0",
		"ircut_state": "false",
		"ir850_state": true,
		"ir940_state": 1,
		"white_state": "true",
		"unknown_future_field": {"nested": [1, 2, 3]}
	}`)
	var hb Heartbeat
	if err := json.Unmarshal(raw, &hb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !hb.PrivacyEnabled {
		t.Error("privacy_enabled = false, want true (string \"1\")")
	}
	if hb.DaynightMode != "night" || !hb.DaynightEnabled {
		t.Errorf("daynight = %q/%v, want night/true", hb.DaynightMode, hb.DaynightEnabled)
	}
	if hb.DaynightBrightness != 80 {
		t.Errorf("daynight_brightness = %d, want 80 (string \"80\")", hb.DaynightBrightness)
	}
	if hb.MotionEnabled {
		t.Error("motion_enabled = true, want false (number 0)")
	}
	if !hb.SpkEnabled || hb.MicEnabled {
		t.Errorf("spk/mic = %v/%v, want true/false", hb.SpkEnabled, hb.MicEnabled)
	}
	if !hb.RecCh0 || hb.RecCh1 {
		t.Errorf("rec_ch0/ch1 = %v/%v, want true/false", hb.RecCh0, hb.RecCh1)
	}
	if hb.IRCutState || !hb.IR850State || !hb.IR940State || !hb.WhiteState {
		t.Errorf("illuminators = cut:%v ir850:%v ir940:%v white:%v, want false/true/true/true",
			hb.IRCutState, hb.IR850State, hb.IR940State, hb.WhiteState)
	}
}

func TestNumUnmarshal(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`42`, 42},
		{`"42"`, 42},
		{`""`, 0},
		{`null`, 0},
		{`"junk"`, 0},
	}
	for _, c := range cases {
		var n Num
		if err := json.Unmarshal([]byte(c.in), &n); err != nil {
			t.Errorf("Num(%s): %v", c.in, err)
			continue
		}
		if int(n) != c.want {
			t.Errorf("Num(%s) = %d, want %d", c.in, n, c.want)
		}
	}
}
