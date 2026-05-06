package dump

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseAir65(t *testing.T) {
	body := readFixture(t)
	info := Parse(body)

	wants := map[string]string{
		"Board":     "BETAFPVG473",
		"MCUID":     "003700303145500c20393758",
		"CraftName": "Air65",
		"PilotName": "Justin",
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
	body := readFixture(t)
	cases := map[string]string{
		"craft_name":         "Air65",
		"pilot_name":         "Justin",
		"vtx_band":           "4",
		"vtx_freq":           "5800",
		"motor_pwm_protocol": "DSHOT300",
	}
	for key, want := range cases {
		got, ok := Get(body, key)
		if !ok {
			t.Errorf("Get(%q) = (_, false), want (%q, true)", key, want)
			continue
		}
		if got != want {
			t.Errorf("Get(%q) = %q, want %q", key, got, want)
		}
	}

	// Prefix collision: "set vtx" must not match "set vtx_band" etc.
	if _, ok := Get(body, "vtx"); ok {
		t.Error("Get(vtx) matched a prefix of another key")
	}
	// Real prefixed key: "set osd_craft_name_pos" should resolve.
	if got, ok := Get(body, "osd_craft_name_pos"); !ok || got != "398" {
		t.Errorf("Get(osd_craft_name_pos) = (%q, %v), want (398, true)", got, ok)
	}

	// Missing key.
	if _, ok := Get(body, "no_such_key"); ok {
		t.Error("Get(no_such_key) = (_, true), want false")
	}
}

func readFixture(t *testing.T) string {
	t.Helper()
	// Walk up until we find air65.txt at the repo root.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		path := filepath.Join(dir, "air65.txt")
		if b, err := os.ReadFile(path); err == nil {
			return string(b)
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("air65.txt fixture not found")
	return ""
}
