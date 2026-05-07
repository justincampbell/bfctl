package fc

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"go.bug.st/serial"

	"github.com/justincampbell/bfctl/internal/msp"
)

// MSPSession is an open USB CDC ACM port used purely for MSP queries. Unlike
// Session it never sends `#` and never enters CLI mode — every Query is a
// raw MSP v1 round-trip.
//
// Once a port is in CLI mode (after any prior `bfctl set` / `bfctl exec` /
// `bfctl cli` that ended without rebooting the FC), MSP queries don't work
// until the FC reboots back into MSP mode. Power-cycle or run a save-causing
// command first if Query consistently times out.
type MSPSession struct {
	port serial.Port
	path string
}

// OpenMSP opens the port, waits for the FC's USB CDC ACM stack to settle,
// and returns a session ready for MSP queries.
func OpenMSP(path string) (*MSPSession, error) {
	port, err := serial.Open(path, &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	_ = port.SetDTR(false)
	_ = port.SetRTS(false)
	time.Sleep(100 * time.Millisecond)
	_ = port.SetDTR(true)
	_ = port.SetRTS(true)
	time.Sleep(300 * time.Millisecond)
	if err := port.SetReadTimeout(50 * time.Millisecond); err != nil {
		_ = port.Close()
		return nil, fmt.Errorf("set read timeout: %w", err)
	}
	if err := drainPort(port, 150*time.Millisecond); err != nil {
		_ = port.Close()
		return nil, err
	}
	return &MSPSession{port: port, path: path}, nil
}

// Path returns the device path the session was opened on.
func (s *MSPSession) Path() string { return s.path }

// Close releases the port. Safe to call multiple times.
func (s *MSPSession) Close() error { return s.port.Close() }

// ErrMSPClosedPort is returned when a Query encounters a write error that
// looks like the port was closed mid-flight (FC rebooted, USB unplugged).
var ErrMSPClosedPort = errors.New("MSP: port closed mid-query")

// ErrMSPInCLIMode is returned when the FC echoed our request bytes back
// instead of replying — a clear signal it's in CLI mode from a prior
// session that didn't reboot. MSP queries don't work in this state; the
// user needs to power-cycle the FC (or run `bfctl exec exit` to reboot it).
var ErrMSPInCLIMode = errors.New("MSP: FC is in CLI mode (reboot required: unplug/replug, or `bfctl exec exit`)")

// Query sends one MSP v1 request and reads exactly one response. timeout
// bounds the entire round-trip.
//
// If the FC is in CLI mode (left over from a prior bfctl session that didn't
// reboot), it echoes our request bytes back rather than replying. We detect
// that — direction byte `<` instead of `>`/`!` — and surface ErrMSPInCLIMode.
func (s *MSPSession) Query(code uint8, payload []byte, timeout time.Duration) (msp.Response, error) {
	frame := msp.Request(code, payload)
	if _, err := s.port.Write(frame); err != nil {
		return msp.Response{}, fmt.Errorf("%w: %v", ErrMSPClosedPort, err)
	}
	resp, err := msp.ReadResponse(s.port, timeout)
	if err != nil {
		// "bad direction byte 0x3c" means the FC echoed `$M<…` back at us
		// because the CLI parser is reading our bytes as text. Translate
		// that into an actionable error.
		if errors.Is(err, msp.ErrCorrupt) && bytes.Contains([]byte(err.Error()), []byte("0x3c")) {
			return resp, ErrMSPInCLIMode
		}
		return resp, err
	}
	if resp.Code != code {
		return resp, fmt.Errorf("MSP: reply code %d does not match request %d", resp.Code, code)
	}
	return resp, nil
}

// drainPort reads and discards anything currently buffered on the port.
// Used at session open to flush any stale frames from prior sessions.
//
// Shared between Session and MSPSession; lives at file scope to avoid a
// shared-base-struct refactor.
func drainPort(p serial.Port, window time.Duration) error {
	if err := p.SetReadTimeout(50 * time.Millisecond); err != nil {
		return err
	}
	deadline := time.Now().Add(window)
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		if _, err := p.Read(buf); err != nil {
			return err
		}
	}
	return nil
}
