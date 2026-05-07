package fc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/justincampbell/bfctl/internal/msp"
)

// Info is the MSP-derived view of an FC's identity. Same shape as the JSON
// `bfctl info` emits.
//
// PilotName is intentionally not populated: there is no MSP v1 command for
// it (only MSP2_GET_TEXT, which bfctl doesn't implement). Reading it would
// require sending `#` to enter CLI mode, which is what `bfctl info` is
// specifically avoiding — once the FC is in CLI mode, all subsequent MSP
// queries fail until reboot. See msp_session.go ErrMSPInCLIMode.
type Info struct {
	Board     string
	MCUID     string
	Firmware  string
	CraftName string
	PilotName string
}

// QueryInfo opens an MSP-only session against the FC and returns its
// metadata. Critically it never enters CLI mode, so calling QueryInfo does
// not break subsequent MSP queries on the same FC.
//
// Each individual MSP query is best-effort: if a particular code times out
// or returns a malformed payload, the corresponding Info field is left
// empty rather than failing the whole call. Only an outright session-open
// failure (port held, FC absent) returns a non-nil error.
func QueryInfo(path string) (Info, error) {
	sess, err := OpenMSP(path)
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = sess.Close() }()

	const queryTimeout = 500 * time.Millisecond

	var info Info

	// MSP_API_VERSION first — also doubles as the "is the FC in MSP mode"
	// probe. If this fails with ErrMSPInCLIMode, every subsequent query
	// will too; surface the error so the user gets the actionable message
	// instead of a wall of "field empty" output.
	apiResp, err := sess.Query(msp.CmdAPIVersion, nil, queryTimeout)
	if err != nil {
		if errors.Is(err, ErrMSPInCLIMode) {
			return Info{}, err
		}
		// Other errors (timeout, etc.) — keep going; fields stay empty.
	}

	variant := queryString(sess, msp.CmdFCVariant, queryTimeout)
	fcVer := queryFCVersion(sess, queryTimeout)
	apiVer := decodeAPIVersion(apiResp)
	board, target := queryBoardInfo(sess, queryTimeout)
	build := queryBuildInfo(sess, queryTimeout)

	info.Board = board
	info.MCUID = queryMCUID(sess, queryTimeout)
	info.Firmware = composeFirmware(variant, fcVer, target, board, build, apiVer)
	info.CraftName = queryString(sess, msp.CmdName, queryTimeout)
	// info.PilotName intentionally left empty — see Info doc.

	return info, nil
}

// queryString reads code and returns its payload as an ASCII string with
// trailing NULs stripped. Returns "" on any failure.
func queryString(sess *MSPSession, code uint8, timeout time.Duration) string {
	resp, err := sess.Query(code, nil, timeout)
	if err != nil || !resp.OK() {
		return ""
	}
	return strings.TrimRight(string(resp.Payload), "\x00")
}

// queryFCVersion reads MSP_FC_VERSION and returns "major.minor.patch", or ""
// on failure.
func queryFCVersion(sess *MSPSession, timeout time.Duration) string {
	resp, err := sess.Query(msp.CmdFCVersion, nil, timeout)
	if err != nil || !resp.OK() || len(resp.Payload) < 3 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", resp.Payload[0], resp.Payload[1], resp.Payload[2])
}

// decodeAPIVersion turns an MSP_API_VERSION response into "X.Y", or "" if
// the response is missing/malformed.
func decodeAPIVersion(resp msp.Response) string {
	if !resp.OK() || len(resp.Payload) < 3 {
		return ""
	}
	// Payload is [protoVersion, apiMajor, apiMinor].
	return fmt.Sprintf("%d.%d", resp.Payload[1], resp.Payload[2])
}

// queryBoardInfo reads MSP_BOARD_INFO and returns (boardName, targetName).
// Either may be "" if the FC truncates the reply or the parse fails.
//
// Layout (all little-endian where applicable):
//
//	[0..4)   boardIdentifier (4 ASCII bytes)
//	[4..6)   hardwareRevision (uint16)
//	[6]      fcType (uint8)
//	[7]      targetCapabilities (uint8)
//	[8]      targetNameLen (N1)
//	[9..]    targetName (N1 bytes)
//	[..]     boardNameLen (N2) → boardName (N2 bytes)
//	[..]     manufacturerLen (N3) → manufacturer (N3 bytes)
//	…signature, mcuType…
//
// We only care about targetName and boardName here.
func queryBoardInfo(sess *MSPSession, timeout time.Duration) (board, target string) {
	resp, err := sess.Query(msp.CmdBoardInfo, nil, timeout)
	if err != nil || !resp.OK() {
		return "", ""
	}
	p := resp.Payload
	const fixedHeaderLen = 8 // boardIdentifier(4) + hardwareRev(2) + fcType(1) + targetCap(1)
	if len(p) < fixedHeaderLen+1 {
		return "", ""
	}
	off := fixedHeaderLen
	target, off = readPString(p, off)
	board, _ = readPString(p, off)
	return board, target
}

// readPString reads a Pascal-style string (one byte length, then bytes)
// from p starting at off, returning the string and the offset after it.
// On a short read it returns ("", len(p)) so the caller naturally stops.
func readPString(p []byte, off int) (string, int) {
	if off >= len(p) {
		return "", len(p)
	}
	n := int(p[off])
	off++
	if off+n > len(p) {
		return "", len(p)
	}
	return string(p[off : off+n]), off + n
}

// queryBuildInfo reads MSP_BUILD_INFO and returns "<date> / <time> (<git>)",
// matching the diff `# Betaflight …` line layout closely enough for users
// to recognise. Returns "" on failure.
//
// Wire layout: 11 bytes date, 8 bytes time, 7 bytes short git rev, then
// optional flags (added in API 1.46, ignored here).
func queryBuildInfo(sess *MSPSession, timeout time.Duration) string {
	resp, err := sess.Query(msp.CmdBuildInfo, nil, timeout)
	if err != nil || !resp.OK() {
		return ""
	}
	const dateLen, timeLen, gitLen = 11, 8, 7
	if len(resp.Payload) < dateLen+timeLen+gitLen {
		return ""
	}
	date := strings.TrimRight(string(resp.Payload[:dateLen]), "\x00 ")
	t := strings.TrimRight(string(resp.Payload[dateLen:dateLen+timeLen]), "\x00 ")
	git := strings.TrimRight(string(resp.Payload[dateLen+timeLen:dateLen+timeLen+gitLen]), "\x00 ")
	return fmt.Sprintf("%s / %s (%s)", date, t, git)
}

// queryMCUID reads MSP_UID and returns it formatted like Betaflight CLI's
// `mcu_id` line: three uint32s concatenated as %08x. Returns "" on failure.
//
// Note the byte-order subtlety: MSP_UID writes the three uint32 IDs in
// little-endian on the wire (sbufWriteU32 → LSB first). The CLI prints
// them as `%08x%08x%08x` of the host-native uint32 — i.e. MSB first per
// word. We decode LE then format big-endian per word so MSP and CLI
// produce identical strings.
func queryMCUID(sess *MSPSession, timeout time.Duration) string {
	resp, err := sess.Query(msp.CmdUID, nil, timeout)
	if err != nil || !resp.OK() {
		return ""
	}
	return formatMCUID(resp.Payload)
}

// formatMCUID renders a 12-byte MSP_UID payload the way Betaflight CLI's
// `mcu_id` command does. Returns "" if the payload is too short.
func formatMCUID(payload []byte) string {
	if len(payload) < 12 {
		return ""
	}
	w0 := binary.LittleEndian.Uint32(payload[0:4])
	w1 := binary.LittleEndian.Uint32(payload[4:8])
	w2 := binary.LittleEndian.Uint32(payload[8:12])
	return fmt.Sprintf("%08x%08x%08x", w0, w1, w2)
}

// composeFirmware builds a one-line firmware string out of the available
// MSP fragments, matching the diff's `# Betaflight / TARGET (BOARD) X.Y.Z
// DATE / TIME (GITREV) MSP API: A.B` format closely enough for humans.
// Missing fragments are simply omitted.
func composeFirmware(variant, fcVer, target, board, build, apiVer string) string {
	if variant == "" && fcVer == "" && target == "" && board == "" && build == "" && apiVer == "" {
		return ""
	}
	var b strings.Builder
	if variant != "" {
		// "BTFL" → "Betaflight" so the output reads like the CLI line.
		b.WriteString(expandVariant(variant))
	}
	if target != "" {
		fmt.Fprintf(&b, " / %s", target)
	}
	if board != "" {
		fmt.Fprintf(&b, " (%s)", board)
	}
	if fcVer != "" {
		fmt.Fprintf(&b, " %s", fcVer)
	}
	if build != "" {
		fmt.Fprintf(&b, " %s", build)
	}
	if apiVer != "" {
		fmt.Fprintf(&b, " MSP API: %s", apiVer)
	}
	return strings.TrimSpace(b.String())
}

// expandVariant rewrites the 4-char MSP_FC_VARIANT tag into the project
// name users see in `diff all`. Only BTFL is in active use; other variants
// pass through unchanged so a future port still produces a readable string.
func expandVariant(v string) string {
	switch strings.ToUpper(v) {
	case "BTFL":
		return "Betaflight"
	case "INAV":
		return "INAV"
	case "CLFL":
		return "Cleanflight"
	default:
		return v
	}
}
