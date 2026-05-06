package dump

import "testing"

// Synthetic fixture mirroring the structure of a real `diff all` body —
// small enough to read inline, broad enough to exercise every parser path.
const fixture = `# version
# Betaflight / STM32G47X (G473) 2025.12.2 Mar 24 2026 / 02:48:19 (79065c96b) MSP API: 1.47
# config rev: e378347

board_name TESTBOARD
manufacturer_id ABCD
mcu_id 0123456789abcdef0123456789abcdef
signature

# master
set motor_pwm_protocol = DSHOT300
set vtx_band = 4
set vtx_freq = 5800
set osd_craft_name_pos = 398
set craft_name = TestCraft
set pilot_name = TestPilot

# save configuration
`

func TestParse(t *testing.T) {
	info := Parse(fixture)

	wants := map[string]string{
		"Board":     "TESTBOARD",
		"MCUID":     "0123456789abcdef0123456789abcdef",
		"CraftName": "TestCraft",
		"PilotName": "TestPilot",
	}
	got := map[string]string{
		"Board":     info.Board,
		"MCUID":     info.MCUID,
		"CraftName": info.CraftName,
		"PilotName": info.PilotName,
	}
	for k, want := range wants {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if info.Firmware == "" {
		t.Error("Firmware = \"\", want non-empty")
	}
}

func TestGet(t *testing.T) {
	cases := map[string]string{
		"craft_name":         "TestCraft",
		"pilot_name":         "TestPilot",
		"vtx_band":           "4",
		"vtx_freq":           "5800",
		"motor_pwm_protocol": "DSHOT300",
	}
	for key, want := range cases {
		got, ok := Get(fixture, key)
		if !ok {
			t.Errorf("Get(%q) = (_, false), want (%q, true)", key, want)
			continue
		}
		if got != want {
			t.Errorf("Get(%q) = %q, want %q", key, got, want)
		}
	}

	// Prefix collision: "set vtx" must not match "set vtx_band" etc.
	if _, ok := Get(fixture, "vtx"); ok {
		t.Error("Get(vtx) matched a prefix of another key")
	}
	// Real prefixed key: "set osd_craft_name_pos" should resolve.
	if got, ok := Get(fixture, "osd_craft_name_pos"); !ok || got != "398" {
		t.Errorf("Get(osd_craft_name_pos) = (%q, %v), want (398, true)", got, ok)
	}

	// Missing key.
	if _, ok := Get(fixture, "no_such_key"); ok {
		t.Error("Get(no_such_key) = (_, true), want false")
	}
}
