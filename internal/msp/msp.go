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

// Denylist is every "in message" MSP code in 1..MaxScanCode — i.e. every
// writer reachable by a default scan. The scan sends a 0-byte payload,
// and Betaflight's sbufReadU8 doesn't bounds-check (streambuf.c:98), so
// a writer pulls uninitialized bytes from past the MSP RX buffer into
// whatever config field it targets. The damage ranges from invisible
// (corrupt motor PWM trim) to user-visible (LEDs go red, motor mixer
// scrambled) to catastrophic (188 MSP_SET_OSD_CANVAS persists garbage
// canvas dims via writeEEPROM() + systemReset() and bricks the FC).
//
// Rather than enumerate which corruptions are "safe enough", we refuse
// every writer in scan range. A scan can't read anything useful from a
// writer anyway — the FC either replies 0 bytes or rejects with an error
// frame — so denylisting them costs zero coverage. Single-code queries
// (`bfctl msp 188`) bypass this for opt-in research and supply a real
// payload via stdin if the user wants to actually exercise the writer.
//
// Highlight cases (line numbers in betaflight/src/main/msp/msp.c, upstream
// main 407f3f9):
//
//	 47 MSP_SET_LED_COLORS        — line 4182: corrupts LED color array → LEDs go red until reboot
//	 68 MSP_REBOOT                — line 2417: registers mspRebootFn; FC reboots mid-scan, desyncs the loop
//	 72 MSP_DATAFLASH_ERASE       — line 3912: unconditional blackboxEraseAll()
//	188 MSP_SET_OSD_CANVAS        — line 4702: writeEEPROM() + systemReset() — proven brick on AT32
//
// The full list is the set of "in message" codes in msp_protocol.h with
// number ≤ MaxScanCode.
var Denylist = []uint8{
	11,  // MSP_SET_NAME
	33,  // MSP_SET_BATTERY_CONFIG
	35,  // MSP_SET_MODE_RANGE
	37,  // MSP_SET_FEATURE_CONFIG
	39,  // MSP_SET_BOARD_ALIGNMENT_CONFIG
	41,  // MSP_SET_CURRENT_METER_CONFIG
	43,  // MSP_SET_MIXER_CONFIG
	45,  // MSP_SET_RX_CONFIG
	47,  // MSP_SET_LED_COLORS
	49,  // MSP_SET_LED_STRIP_CONFIG
	51,  // MSP_SET_RSSI_CONFIG
	53,  // MSP_SET_ADJUSTMENT_RANGE
	55,  // MSP_SET_CF_SERIAL_CONFIG
	57,  // MSP_SET_VOLTAGE_METER_CONFIG
	60,  // MSP_SET_PID_CONTROLLER
	62,  // MSP_SET_ARMING_CONFIG
	65,  // MSP_SET_RX_MAP
	68,  // MSP_REBOOT
	72,  // MSP_DATAFLASH_ERASE
	76,  // MSP_SET_FAILSAFE_CONFIG
	78,  // MSP_SET_RXFAIL_CONFIG
	81,  // MSP_SET_BLACKBOX_CONFIG
	83,  // MSP_SET_TRANSPONDER_CONFIG
	85,  // MSP_SET_OSD_CONFIG
	87,  // MSP_OSD_CHAR_WRITE
	89,  // MSP_SET_VTX_CONFIG
	91,  // MSP_SET_ADVANCED_CONFIG
	93,  // MSP_SET_FILTER_CONFIG
	95,  // MSP_SET_PID_ADVANCED
	97,  // MSP_SET_SENSOR_CONFIG
	99,  // MSP_SET_ARMING_DISABLED
	141, // MSP_SET_SIMPLIFIED_TUNING
	181, // MSP_SET_OSD_VIDEO_CONFIG
	183, // MSP_COPY_PROFILE
	185, // MSP_SET_BEEPER_CONFIG
	186, // MSP_SET_TX_INFO
	188, // MSP_SET_OSD_CANVAS
}

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
