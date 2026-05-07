// Package msp implements MultiWii Serial Protocol v1 framing — the protocol
// Betaflight (and Cleanflight, INAV, …) speak before the user sends `#` to
// switch into CLI mode.
//
// Wire format:
//
//	request:  $ M < <size> <code> <payload...> <checksum>
//	response: $ M > <size> <code> <payload...> <checksum>
//	error:    $ M ! <size> <code> <payload...> <checksum>
//
// `size` and `code` are each one byte. `payload` is `size` bytes long.
// `checksum` is XOR of `size`, `code`, and every payload byte.
//
// MSP v2 (preamble `$X<`, 16-bit codes) is intentionally not implemented —
// the codes bfctl cares about all have v1 mappings.
package msp

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// Wire constants.
const (
	Preamble1 = '$'
	Preamble2 = 'M'
	DirReq    = '<' // host → FC
	DirResp   = '>' // FC → host, success
	DirErr    = '!' // FC → host, FC rejected the command
)

// MaxScanCode is the absolute upper bound bfctl will let a scan reach.
// Codes 200+ are MSP_SET_* writers; sending them with a 0-byte payload
// usually produces a no-op error response, but to be safe we don't poke
// them.
const MaxScanCode = 199

// SafeScanCeiling is the highest code probed when `bfctl msp` is invoked
// without an explicit --max. A scan once bricked a LIONBEE_V1 (Betaflight
// 2025.12.1, AT32F435G); reading Betaflight's `src/main/msp/msp.c` pinned
// the cause to MSP_SET_OSD_CANVAS (188), which on a 0-byte payload reads
// garbage canvas dimensions, force-sets video_system to HD, calls
// writeEEPROM(), and reboots — corrupting the persisted OSD config.
// That code (along with the other writers in 131–199) is on Denylist, so
// it's safe to scan up to MaxScanCode by default.
const SafeScanCeiling = MaxScanCode

// Denylist is the set of MSP codes scan mode refuses to send. Each entry
// is an "in" handler that, when called with a 0-byte payload (which is
// what scan sends), would reboot the FC, wipe storage, or corrupt config.
// 188 is the proven brick; 68 desyncs the scan (FC reboots mid-loop, so
// no replies follow until reopen); 72 silently erases blackbox flash;
// the others are zero-cost adds — a scan can't read writers anyway.
// Single-code queries (`bfctl msp 188`) bypass this for opt-in research.
//
// Source citations are in betaflight/src/main/msp/msp.c (line numbers as
// of upstream main 407f3f9):
//
//	 68 MSP_REBOOT                — line 2417: registers mspRebootFn; FC reboots after a 1-byte reply
//	 72 MSP_DATAFLASH_ERASE       — line 3912: unconditional blackboxEraseAll()
//	141 MSP_SET_SIMPLIFIED_TUNING — line 3855: garbage→PID profile + gyro filter
//	181 MSP_SET_OSD_VIDEO_CONFIG  — protocol-defined writer (no current handler)
//	183 MSP_COPY_PROFILE          — line 2873: pidCopyProfile(garbage, garbage)
//	185 MSP_SET_BEEPER_CONFIG     — line 3972: garbage→beeper_off_flags
//	186 MSP_SET_TX_INFO           — line 4251: garbage→RSSI MSP
//	188 MSP_SET_OSD_CANVAS        — line 4702: writeEEPROM() + systemReset()
var Denylist = []uint8{68, 72, 141, 181, 183, 185, 186, 188}

// IsDenylisted reports whether a code is in Denylist.
func IsDenylisted(code uint8) bool {
	for _, c := range Denylist {
		if c == code {
			return true
		}
	}
	return false
}

// Response is one parsed MSP v1 reply.
type Response struct {
	Direction byte   // DirResp or DirErr
	Code      uint8  // command echoed back by the FC
	Payload   []byte // size bytes
}

// OK reports whether the FC accepted the request.
func (r Response) OK() bool { return r.Direction == DirResp }

// Request encodes one MSP v1 request frame.
func Request(code uint8, payload []byte) []byte {
	if len(payload) > 255 {
		// MSP v1 size byte is 8-bit. Caller bug.
		panic(fmt.Sprintf("msp.Request: payload %d bytes exceeds v1 max 255", len(payload)))
	}
	size := uint8(len(payload))
	cksum := size ^ code
	for _, b := range payload {
		cksum ^= b
	}
	out := make([]byte, 0, 6+len(payload))
	out = append(out, Preamble1, Preamble2, DirReq, size, code)
	out = append(out, payload...)
	out = append(out, cksum)
	return out
}

// ErrTimeout is returned when a response doesn't fully arrive before the
// caller's deadline.
var ErrTimeout = errors.New("msp: response timed out")

// ErrChecksum is returned when a response framed correctly but its checksum
// byte didn't match the XOR of size+code+payload.
var ErrChecksum = errors.New("msp: bad checksum")

// ErrCorrupt is returned when the wire bytes don't form a valid MSP frame
// (no preamble found, unexpected EOF mid-frame, etc.).
var ErrCorrupt = errors.New("msp: corrupt frame")

// ReadResponse reads exactly one MSP v1 response from r, tolerating leading
// noise (CLI banner echoes, partial frames from a previous query) by hunting
// for the `$M` preamble. The whole read must complete within timeout.
//
// Two frame layouts are supported, both v1:
//
//	standard: $M> <size> <code> <payload[size]> <cksum>           (size ≤ 254)
//	jumbo:    $M> 0xFF <code> <size_lo> <size_hi> <payload> <cksum> (size > 254)
//
// The jumbo path is what Betaflight uses for any reply larger than one byte
// can express (BOXNAMES, PIDNAMES, BOARD_INFO, …). Note the byte order
// difference: in standard frames size precedes code, in jumbo frames code
// precedes size. Checksum in both cases is XOR of every byte after the
// preamble through the final payload byte.
func ReadResponse(r io.Reader, timeout time.Duration) (Response, error) {
	deadline := time.Now().Add(timeout)
	if err := syncToPreamble(r, deadline); err != nil {
		return Response{}, err
	}
	dirBuf, err := readN(r, 1, deadline)
	if err != nil {
		return Response{}, err
	}
	dir := dirBuf[0]
	if dir != DirResp && dir != DirErr {
		return Response{}, fmt.Errorf("%w: bad direction byte %#02x", ErrCorrupt, dir)
	}
	sizeByte, err := readN(r, 1, deadline)
	if err != nil {
		return Response{}, err
	}

	var size int
	var code uint8
	var hdrXOR byte
	if sizeByte[0] == 0xFF {
		// Jumbo: code, size_lo, size_hi follow.
		rest, err := readN(r, 3, deadline)
		if err != nil {
			return Response{}, err
		}
		code = rest[0]
		size = int(rest[1]) | int(rest[2])<<8
		hdrXOR = 0xFF ^ rest[0] ^ rest[1] ^ rest[2]
	} else {
		// Standard.
		size = int(sizeByte[0])
		codeBuf, err := readN(r, 1, deadline)
		if err != nil {
			return Response{}, err
		}
		code = codeBuf[0]
		hdrXOR = sizeByte[0] ^ code
	}

	payload, err := readN(r, size, deadline)
	if err != nil {
		return Response{}, err
	}
	tail, err := readN(r, 1, deadline)
	if err != nil {
		return Response{}, err
	}
	want := hdrXOR
	for _, b := range payload {
		want ^= b
	}
	if tail[0] != want {
		return Response{}, fmt.Errorf("%w: got %#02x want %#02x", ErrChecksum, tail[0], want)
	}
	return Response{Direction: dir, Code: code, Payload: payload}, nil
}

// syncToPreamble consumes bytes from r until it has seen "$M" or the deadline
// elapses. Up to 4 KB of leading noise is tolerated.
func syncToPreamble(r io.Reader, deadline time.Time) error {
	buf := make([]byte, 1)
	state := 0 // 0=looking for '$', 1=saw '$', looking for 'M'
	scanned := 0
	for {
		if time.Now().After(deadline) {
			return ErrTimeout
		}
		n, err := r.Read(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		scanned++
		if scanned > 4096 {
			return fmt.Errorf("%w: no $M preamble in 4 KB of noise", ErrCorrupt)
		}
		switch state {
		case 0:
			if buf[0] == Preamble1 {
				state = 1
			}
		case 1:
			if buf[0] == Preamble2 {
				return nil
			}
			if buf[0] != Preamble1 {
				state = 0
			}
		}
	}
}

// readN reads exactly n bytes from r before deadline.
func readN(r io.Reader, n int, deadline time.Time) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if time.Now().After(deadline) {
			return out, ErrTimeout
		}
		nb, err := r.Read(buf[:n-len(out)])
		if err != nil {
			return out, err
		}
		out = append(out, buf[:nb]...)
	}
	return out, nil
}
