// Package fc talks to a Betaflight flight controller over USB CDC ACM.
package fc

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// ErrNoFC means no flight controller was found on any USB serial port.
var ErrNoFC = errors.New("no Betaflight FC found")

// ErrMultipleFCs means more than one matching flight controller was found.
type ErrMultipleFCs struct{ Ports []string }

func (e *ErrMultipleFCs) Error() string {
	return "multiple Betaflight FCs found: " + strings.Join(e.Ports, ", ")
}

// Port describes a detected flight controller.
type Port struct {
	Path    string
	VID     string
	PID     string
	Product string
	Serial  string
}

// FindAll lists every USB serial port that looks like a Betaflight FC.
func FindAll() ([]Port, error) {
	all, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, fmt.Errorf("enumerate serial ports: %w", err)
	}
	var matches []Port
	for _, p := range all {
		if !p.IsUSB {
			continue
		}
		if !looksLikeBetaflightFC(p.Product, p.VID, p.PID) {
			continue
		}
		matches = append(matches, Port{
			Path:    p.Name,
			VID:     strings.ToLower(p.VID),
			PID:     strings.ToLower(p.PID),
			Product: p.Product,
			Serial:  p.SerialNumber,
		})
	}
	return matches, nil
}

// fcMatcher describes one MCU family that ships Betaflight targets. A port
// matches if the Product string contains ProductSubstr (when set, ignoring
// case) OR the VID:PID pair matches (when both are set).
//
// Supporting both signals matters because USB enumeration is platform-flaky:
// macOS's `go.bug.st/serial` returns the Product field for STM32 boards but
// leaves it empty for AT32 boards (only VID/PID survive). Linux and Windows
// generally return both. Keeping both fields lets one matcher cover every
// platform with no per-OS branching.
type fcMatcher struct {
	Family        string // human-readable, used in diagnostics
	ProductSubstr string // case-insensitive substring; "" disables product match
	VID, PID      string // hex (no 0x), case-insensitive; "" disables ID match
}

// fcMatchers enumerates every MCU family bfctl recognises as a Betaflight FC.
// To support a new MCU family, add one row — no other code changes required.
//
// Note: every Betaflight target (so far) uses PID 0x5740, so the VID is the
// distinguishing field.
var fcMatchers = []fcMatcher{
	{Family: "STM32 (ST)", ProductSubstr: "betaflight", VID: "0483", PID: "5740"},
	{Family: "AT32 (Artery)", ProductSubstr: "at32 virtual com port", VID: "2e3c", PID: "5740"},
}

func (m fcMatcher) matches(product, vid, pid string) bool {
	if m.ProductSubstr != "" && product != "" {
		if strings.Contains(strings.ToLower(product), m.ProductSubstr) {
			return true
		}
	}
	if m.VID != "" && m.PID != "" {
		if strings.EqualFold(vid, m.VID) && strings.EqualFold(pid, m.PID) {
			return true
		}
	}
	return false
}

func looksLikeBetaflightFC(product, vid, pid string) bool {
	for _, m := range fcMatchers {
		if m.matches(product, vid, pid) {
			return true
		}
	}
	return false
}

// Resolve returns the port to use. If explicit is non-empty it is returned
// verbatim. Otherwise FindAll runs and must return exactly one match.
func Resolve(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	ports, err := FindAll()
	if err != nil {
		return "", err
	}
	switch len(ports) {
	case 0:
		return "", ErrNoFC
	case 1:
		return ports[0].Path, nil
	default:
		paths := make([]string, len(ports))
		for i, p := range ports {
			paths[i] = p.Path
		}
		return "", &ErrMultipleFCs{Ports: paths}
	}
}

// Session is an open CLI-mode connection to the FC.
type Session struct {
	port serial.Port
	path string
}

// Open connects to the FC and enters CLI mode.
//
// Sequence: open at 115200/8N1, settle, then probe state. The FC may be in
// one of two states:
//
//  1. Fresh boot / MSP mode — we must send a valid MSP frame first to
//     "wake" the USB-side parser. Without that round-trip Betaflight
//     silently ignores `#` (this is the wall the previous pyserial attempt
//     hit). After the MSP exchange, sending `#` enters CLI mode.
//  2. Already in CLI mode — left over from a prior session that closed
//     without exiting (which is the right thing to do, since `exit`
//     reboots the FC). In this state the FC echoes our writes; we detect
//     that and skip the entry sequence.
func Open(path string) (*Session, error) {
	port, err := serial.Open(path, &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// Toggle DTR/RTS — defensive against macOS CDC ACM quirks.
	_ = port.SetDTR(false)
	_ = port.SetRTS(false)
	time.Sleep(100 * time.Millisecond)
	_ = port.SetDTR(true)
	_ = port.SetRTS(true)
	time.Sleep(300 * time.Millisecond)

	s := &Session{port: port, path: path}

	if err := s.drain(150 * time.Millisecond); err != nil {
		_ = port.Close()
		return nil, err
	}

	// Probe with an MSP frame. The reply tells us which state the FC is in:
	//
	//   - reply contains "$M>"          → MSP mode; send `#` to enter CLI.
	//   - reply contains echo of our bytes → already in CLI (left over from
	//     a previous session that closed without exiting). The MSP frame
	//     is harmless garbage typed at the prompt; drain and continue.
	//   - empty                          → link broken / port held elsewhere.
	mspFrame := []byte{'$', 'M', '<', 0x00, 0x01, 0x01}
	if _, err := port.Write(mspFrame); err != nil {
		_ = port.Close()
		return nil, fmt.Errorf("write MSP probe: %w", err)
	}
	reply, _ := s.readUntilSilence(1500*time.Millisecond, 250*time.Millisecond)

	switch {
	case bytes.Contains(reply, []byte{'$', 'M', '>'}):
		// MSP mode. Send `#` to switch to CLI.
		if _, err := port.Write([]byte("#")); err != nil {
			_ = port.Close()
			return nil, fmt.Errorf("write CLI enter: %w", err)
		}
		banner, err := s.readUntilSilence(2*time.Second, 500*time.Millisecond)
		if err != nil {
			_ = port.Close()
			return nil, fmt.Errorf("read CLI banner: %w", err)
		}
		if !strings.Contains(string(banner), "CLI") {
			_ = port.Close()
			return nil, fmt.Errorf("unexpected CLI banner: %q", truncate(banner, 120))
		}

	case bytes.Contains(reply, []byte("$M<")) || bytes.Contains(reply, []byte("# ")):
		// Already in CLI — the FC echoed our MSP frame. Send a newline
		// to flush whatever half-line state the prompt is in, then drain.
		if _, err := port.Write([]byte("\r\n")); err != nil {
			_ = port.Close()
			return nil, fmt.Errorf("write resync: %w", err)
		}
		_, _ = s.readUntilSilence(500*time.Millisecond, 250*time.Millisecond)

	case len(reply) == 0:
		_ = port.Close()
		return nil, errors.New("FC returned no data (is Chrome/Configurator holding the port?)")

	default:
		_ = port.Close()
		return nil, fmt.Errorf("unexpected probe reply %q", truncate(reply, 80))
	}

	return s, nil
}

// Path returns the device path the session was opened on.
func (s *Session) Path() string { return s.path }

// Run sends a CLI command, reads the reply, and returns the body with the
// echoed command and trailing prompt stripped.
//
// On a read error (e.g. the FC reboots mid-reply because the command was
// `save`), Run still returns whatever was received before the error so the
// caller can decide what to do with the partial output. Existing callers
// that already bail on err see no behaviour change.
func (s *Session) Run(cmd string) (string, error) {
	if _, err := s.port.Write([]byte(cmd + "\r\n")); err != nil {
		return "", fmt.Errorf("write %q: %w", cmd, err)
	}
	raw, err := s.readUntilSilence(10*time.Second, 1500*time.Millisecond)
	cleaned := cleanResponse(string(raw), cmd)
	if err != nil {
		return cleaned, err
	}
	return cleaned, nil
}

// Save sends `save`. Betaflight writes the configuration to flash and then
// reboots — which tears the USB CDC ACM device down mid-reply. Read errors
// after the write are expected and ignored. Once Save returns the port is
// effectively invalid; the caller should close it and (if more work is
// needed) wait for the FC to re-enumerate before opening a new session.
func (s *Session) Save() error {
	if _, err := s.port.Write([]byte("save\r\n")); err != nil {
		return fmt.Errorf("write save: %w", err)
	}
	_, _ = s.readUntilSilence(3*time.Second, 500*time.Millisecond)
	return nil
}

// Close releases the serial port. We deliberately do NOT send "exit" — that
// reboots the FC.
func (s *Session) Close() error {
	return s.port.Close()
}

// readUntilSilence reads bytes from the port until the link goes quiet for
// idle, or until total elapses without any data at all.
func (s *Session) readUntilSilence(total, idle time.Duration) ([]byte, error) {
	if err := s.port.SetReadTimeout(150 * time.Millisecond); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(total)
	var (
		out      []byte
		buf      = make([]byte, 4096)
		lastByte time.Time
		seen     bool
	)
	for {
		if time.Now().After(deadline) {
			if !seen {
				return nil, errors.New("timed out waiting for FC response")
			}
			return out, nil
		}
		n, err := s.port.Read(buf)
		if err != nil {
			return out, err
		}
		if n > 0 {
			out = append(out, buf[:n]...)
			lastByte = time.Now()
			seen = true
			continue
		}
		if seen && time.Since(lastByte) >= idle {
			return out, nil
		}
	}
}

// drain reads and discards anything currently buffered on the port. Used at
// open time to flush MSP frames before we try to enter CLI mode.
func (s *Session) drain(window time.Duration) error {
	if err := s.port.SetReadTimeout(50 * time.Millisecond); err != nil {
		return err
	}
	deadline := time.Now().Add(window)
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		if _, err := s.port.Read(buf); err != nil {
			return err
		}
	}
	return nil
}

// cleanResponse strips the echoed command at the start, the trailing "# "
// prompt at the end, and (if present) a trailing "save" recommendation line
// the FC emits after "diff all". Line endings are normalised to \n so output
// matches the file format the web Configurator writes.
func cleanResponse(raw, cmd string) string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	// Drop the echoed command line if present.
	if i := strings.Index(s, cmd); i >= 0 {
		if nl := strings.Index(s[i:], "\n"); nl >= 0 {
			s = s[i+nl+1:]
		}
	}

	// Drop the trailing prompt and trailing whitespace.
	s = strings.TrimRight(s, " \t\r\n")
	s = strings.TrimSuffix(s, "\n#")
	s = strings.TrimRight(s, " \t\r\n")

	// `diff all` ends with "# save configuration\nsave" — the literal
	// "save" line is a hint the FC prints, not part of the dump.
	// Configurator strips it.
	s = strings.TrimSuffix(s, "\nsave")
	return s
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
