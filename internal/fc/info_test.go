package fc

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"go.bug.st/serial"

	"github.com/justincampbell/bfctl/internal/msp"
)

func TestReadPString(t *testing.T) {
	cases := []struct {
		name      string
		payload   []byte
		startOff  int
		wantStr   string
		wantOff   int
	}{
		{"empty payload", []byte{}, 0, "", 0},
		{"len-only zero", []byte{0x00}, 0, "", 1},
		{"simple string", []byte{0x05, 'h', 'e', 'l', 'l', 'o'}, 0, "hello", 6},
		{"truncated", []byte{0x05, 'h', 'e', 'l'}, 0, "", 4},
		{"after offset", []byte{0xff, 0x03, 'a', 'b', 'c'}, 1, "abc", 5},
	}
	for _, c := range cases {
		gotStr, gotOff := readPString(c.payload, c.startOff)
		if gotStr != c.wantStr || gotOff != c.wantOff {
			t.Errorf("%s: readPString(%v, %d) = (%q, %d), want (%q, %d)",
				c.name, c.payload, c.startOff, gotStr, gotOff, c.wantStr, c.wantOff)
		}
	}
}

func TestFormatMCUID(t *testing.T) {
	// Three little-endian uint32s: 0x00842032, 0x00000409, 0x0917190d.
	// Wire bytes (LE): 32 20 84 00 | 09 04 00 00 | 0d 19 17 09
	// CLI `mcu_id` line for these IDs is `00842032 00000409 0917190d`
	// concatenated with no spaces.
	payload := []byte{0x32, 0x20, 0x84, 0x00, 0x09, 0x04, 0x00, 0x00, 0x0d, 0x19, 0x17, 0x09}
	want := "00842032000004090917190d"
	if got := formatMCUID(payload); got != want {
		t.Errorf("formatMCUID = %q, want %q", got, want)
	}

	// Short payloads return "".
	if got := formatMCUID(payload[:11]); got != "" {
		t.Errorf("formatMCUID(short) = %q, want \"\"", got)
	}
}

func TestDecodeAPIVersion(t *testing.T) {
	// Real reply: proto=0, api=1.47.
	resp := msp.Response{Direction: msp.DirResp, Code: msp.CmdAPIVersion, Payload: []byte{0x00, 0x01, 0x2f}}
	if got := decodeAPIVersion(resp); got != "1.47" {
		t.Errorf("decodeAPIVersion = %q, want %q", got, "1.47")
	}

	// Error response.
	bad := msp.Response{Direction: msp.DirErr, Payload: []byte{0x00, 0x01, 0x2f}}
	if got := decodeAPIVersion(bad); got != "" {
		t.Errorf("decodeAPIVersion(err) = %q, want \"\"", got)
	}

	// Truncated payload.
	short := msp.Response{Direction: msp.DirResp, Payload: []byte{0x00, 0x01}}
	if got := decodeAPIVersion(short); got != "" {
		t.Errorf("decodeAPIVersion(short) = %q, want \"\"", got)
	}
}

func TestComposeFirmware(t *testing.T) {
	got := composeFirmware("BTFL", "4.5.0", "AT32F435G", "LIONBEE_V1",
		"Apr  8 2026 / 10:20:28 (272fa72)", "1.47")
	want := "Betaflight / AT32F435G (LIONBEE_V1) 4.5.0 Apr  8 2026 / 10:20:28 (272fa72) MSP API: 1.47"
	if got != want {
		t.Errorf("composeFirmware =\n  %q\nwant\n  %q", got, want)
	}

	// All empty inputs → empty output.
	if got := composeFirmware("", "", "", "", "", ""); got != "" {
		t.Errorf("composeFirmware(all empty) = %q, want \"\"", got)
	}

	// Unknown variant passes through.
	got = composeFirmware("XXXX", "1.0.0", "", "", "", "")
	if got != "XXXX 1.0.0" {
		t.Errorf("composeFirmware(unknown) = %q, want %q", got, "XXXX 1.0.0")
	}
}

// fakePort is an in-memory serial.Port that records every byte written and
// replies with frames produced by a caller-supplied handler. Lets us drive
// MSPSession.Query without a real FC.
type fakePort struct {
	mu       sync.Mutex
	written  bytes.Buffer
	pending  bytes.Buffer
	handler  func(req []byte) []byte
	closed   bool
	reqState reqParser
}

// reqParser walks a stream of bytes looking for MSP v1 request frames.
// Whenever it sees a complete `$M< size code …` request it hands it off.
type reqParser struct {
	buf []byte
}

func (p *reqParser) feed(b byte) (frame []byte, ok bool) {
	p.buf = append(p.buf, b)
	// Need at least preamble (2) + dir (1) + size (1) + code (1) + cksum (1) = 6 bytes.
	if len(p.buf) < 6 {
		return nil, false
	}
	// Hunt for `$M<`.
	i := bytes.Index(p.buf, []byte{'$', 'M', '<'})
	if i < 0 {
		// Drop bytes that can't possibly start a frame; keep the last
		// two so a `$M` straddling a feed boundary still resolves.
		if len(p.buf) > 2 {
			p.buf = p.buf[len(p.buf)-2:]
		}
		return nil, false
	}
	// Discard anything before the preamble.
	if i > 0 {
		p.buf = p.buf[i:]
	}
	if len(p.buf) < 6 {
		return nil, false
	}
	size := int(p.buf[3])
	need := 5 + size + 1 // header + payload + checksum
	if len(p.buf) < need {
		return nil, false
	}
	frame = make([]byte, need)
	copy(frame, p.buf[:need])
	p.buf = p.buf[need:]
	return frame, true
}

func (f *fakePort) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	f.written.Write(p)
	for _, b := range p {
		if frame, ok := f.reqState.feed(b); ok {
			if reply := f.handler(frame); reply != nil {
				f.pending.Write(reply)
			}
		}
	}
	return len(p), nil
}

func (f *fakePort) Read(p []byte) (int, error) {
	// Tiny sleep so spin-loops in MSPSession reads don't burn CPU. Keeps
	// the test snappy without making timeouts flaky.
	time.Sleep(time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	if f.pending.Len() == 0 {
		return 0, nil
	}
	return f.pending.Read(p)
}

func (f *fakePort) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// All other serial.Port methods are no-ops for our purposes.
func (f *fakePort) SetMode(*serial.Mode) error                  { return nil }
func (f *fakePort) Drain() error                                { return nil }
func (f *fakePort) ResetInputBuffer() error                     { return nil }
func (f *fakePort) ResetOutputBuffer() error                    { return nil }
func (f *fakePort) SetDTR(bool) error                           { return nil }
func (f *fakePort) SetRTS(bool) error                           { return nil }
func (f *fakePort) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	return &serial.ModemStatusBits{}, nil
}
func (f *fakePort) SetReadTimeout(time.Duration) error { return nil }
func (f *fakePort) Break(time.Duration) error          { return nil }

// mspReply builds a v1 standard response frame with the given code/payload.
func mspReply(code uint8, payload []byte) []byte {
	out := []byte{'$', 'M', msp.DirResp, byte(len(payload)), code}
	out = append(out, payload...)
	cksum := byte(len(payload)) ^ code
	for _, b := range payload {
		cksum ^= b
	}
	out = append(out, cksum)
	return out
}

// TestQueryInfoIsMSPOnly is the regression for the bug fix: running info
// must not put the FC into CLI mode. We assert that by inspecting every
// byte the session wrote and confirming the `#` byte never appears.
func TestQueryInfoIsMSPOnly(t *testing.T) {
	port := &fakePort{}
	port.handler = func(req []byte) []byte {
		// Request layout: $ M < size code [payload...] cksum
		if len(req) < 6 {
			return nil
		}
		code := req[4]
		switch code {
		case msp.CmdAPIVersion:
			return mspReply(code, []byte{0x00, 0x01, 0x2f})
		case msp.CmdFCVariant:
			return mspReply(code, []byte("BTFL"))
		case msp.CmdFCVersion:
			return mspReply(code, []byte{4, 5, 0})
		case msp.CmdBoardInfo:
			// boardIdentifier(4) + hwRev(2) + fcType(1) + targetCap(1)
			// + Pstring targetName + Pstring boardName + …
			payload := []byte{'L', 'B', 'V', '1', 0, 0, 0, 0}
			payload = append(payload, byte(len("AT32F435G")))
			payload = append(payload, []byte("AT32F435G")...)
			payload = append(payload, byte(len("LIONBEE_V1")))
			payload = append(payload, []byte("LIONBEE_V1")...)
			return mspReply(code, payload)
		case msp.CmdBuildInfo:
			date := []byte("Apr  8 2026")        // 11
			tm := []byte("10:20:28")              // 8
			git := []byte("272fa72")               // 7
			payload := append(append(date, tm...), git...)
			return mspReply(code, payload)
		case msp.CmdName:
			return mspReply(code, []byte("LionBee3"))
		case msp.CmdUID:
			return mspReply(code, []byte{
				0x32, 0x20, 0x84, 0x00,
				0x09, 0x04, 0x00, 0x00,
				0x0d, 0x19, 0x17, 0x09,
			})
		}
		// Unknown code: return an error frame so Query short-circuits.
		errFrame := []byte{'$', 'M', msp.DirErr, 0, code, code}
		return errFrame
	}

	sess := &MSPSession{port: port, path: "/dev/fake"}
	defer func() { _ = sess.Close() }()

	// Drive the same sequence QueryInfo does, but inline so we don't need
	// to refactor QueryInfo to take a port. (QueryInfo's job is mostly
	// composition of these helpers, which are tested independently.)
	const queryTimeout = 500 * time.Millisecond
	apiResp, err := sess.Query(msp.CmdAPIVersion, nil, queryTimeout)
	if err != nil {
		t.Fatalf("Query API_VERSION: %v", err)
	}
	variant := queryString(sess, msp.CmdFCVariant, queryTimeout)
	fcVer := queryFCVersion(sess, queryTimeout)
	apiVer := decodeAPIVersion(apiResp)
	board, target := queryBoardInfo(sess, queryTimeout)
	build := queryBuildInfo(sess, queryTimeout)
	craft := queryString(sess, msp.CmdName, queryTimeout)
	mcuid := queryMCUID(sess, queryTimeout)

	if variant != "BTFL" {
		t.Errorf("variant = %q, want %q", variant, "BTFL")
	}
	if fcVer != "4.5.0" {
		t.Errorf("fcVer = %q, want %q", fcVer, "4.5.0")
	}
	if apiVer != "1.47" {
		t.Errorf("apiVer = %q, want %q", apiVer, "1.47")
	}
	if target != "AT32F435G" {
		t.Errorf("target = %q, want %q", target, "AT32F435G")
	}
	if board != "LIONBEE_V1" {
		t.Errorf("board = %q, want %q", board, "LIONBEE_V1")
	}
	if build != "Apr  8 2026 / 10:20:28 (272fa72)" {
		t.Errorf("build = %q, want %q", build, "Apr  8 2026 / 10:20:28 (272fa72)")
	}
	if craft != "LionBee3" {
		t.Errorf("craft = %q, want %q", craft, "LionBee3")
	}
	if mcuid != "00842032000004090917190d" {
		t.Errorf("mcuid = %q, want %q", mcuid, "00842032000004090917190d")
	}

	// Crucial assertion: the session never wrote a '#' byte. If it did,
	// the FC would have switched to CLI mode and subsequent MSP queries
	// (from a separate `bfctl msp …` invocation) would all fail with
	// ErrMSPInCLIMode. This is the regression test for the original bug.
	port.mu.Lock()
	written := port.written.Bytes()
	port.mu.Unlock()
	if bytes.IndexByte(written, '#') >= 0 {
		t.Errorf("info path wrote a '#' byte to the FC — would enter CLI mode and break later MSP queries.\nwritten: %x", written)
	}
}

// TestQueryInfoSurfacesCLIMode confirms that if the FC is already in CLI
// mode (echoes our request bytes), QueryInfo returns ErrMSPInCLIMode
// rather than emitting a stub Info struct full of empty strings.
func TestQueryInfoSurfacesCLIMode(t *testing.T) {
	port := &fakePort{}
	port.handler = func(req []byte) []byte {
		// Echo the request back — that's what a CLI-mode FC does. The
		// MSP parser will see direction byte `<` (0x3c) and surface
		// ErrMSPInCLIMode.
		return req
	}

	sess := &MSPSession{port: port, path: "/dev/fake"}
	defer func() { _ = sess.Close() }()

	_, err := sess.Query(msp.CmdAPIVersion, nil, 200*time.Millisecond)
	if !errors.Is(err, ErrMSPInCLIMode) {
		t.Fatalf("Query echoed request: got err %v, want ErrMSPInCLIMode", err)
	}
}
