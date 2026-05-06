package fc

import "testing"

func TestLooksLikeBetaflightFC(t *testing.T) {
	cases := []struct {
		name             string
		product, vid, pid string
		want             bool
	}{
		// STM32 targets — product string populated (Linux, Windows, and
		// macOS in the typical case).
		{"STM32 product+ids", "Betaflight STM32G473", "0483", "5740", true},
		{"STM32 product lowercase", "betaflight stm32f745", "0483", "5740", true},
		// STM32 product missing — fall through to VID/PID.
		{"STM32 ids only", "", "0483", "5740", true},

		// AT32 targets — same two scenarios.
		{"AT32 product+ids", "AT32 Virtual Com Port", "2E3C", "5740", true},
		// macOS reproduction: go.bug.st/serial returns Product="" for the
		// LionBee3, so the VID:PID fallback is what catches it.
		{"AT32 ids only (macOS)", "", "2E3C", "5740", true},

		// Negative cases.
		{"unrelated USB serial", "USB Serial Device", "1a86", "7523", false},
		{"empty everything", "", "", "", false},
		{"matching VID, wrong PID", "", "0483", "0001", false},
	}
	for _, c := range cases {
		got := looksLikeBetaflightFC(c.product, c.vid, c.pid)
		if got != c.want {
			t.Errorf("%s: looksLikeBetaflightFC(%q, %q, %q) = %v, want %v",
				c.name, c.product, c.vid, c.pid, got, c.want)
		}
	}
}
