package msp

import (
	"fmt"
	"strings"
)

// Common MSP v1 command codes. Names match Betaflight's `msp_protocol.h`.
// Not exhaustive — bfctl only needs constants for the codes it consumes
// directly. The Name() function below covers a wider set for display.
const (
	CmdAPIVersion = 1
	CmdFCVariant  = 2
	CmdFCVersion  = 3
	CmdBoardInfo  = 4
	CmdBuildInfo  = 5
	CmdName       = 10
	CmdStatus     = 101
	CmdAttitude   = 108
	CmdAltitude   = 109
	CmdAnalog     = 110
	CmdBoxNames   = 116
	CmdPIDNames   = 117
	CmdBoxIDs     = 119
)

// names is a curated map of MSP v1 codes to their Betaflight names. Used for
// display only — Decode() doesn't depend on it. Codes that aren't in this
// map render as "MSP_UNKNOWN" in scan output.
var names = map[uint8]string{
	1:   "MSP_API_VERSION",
	2:   "MSP_FC_VARIANT",
	3:   "MSP_FC_VERSION",
	4:   "MSP_BOARD_INFO",
	5:   "MSP_BUILD_INFO",
	6:   "MSP_INAV_PID",
	7:   "MSP_SET_INAV_PID",
	10:  "MSP_NAME",
	11:  "MSP_SET_NAME",
	12:  "MSP_NAV_POSHOLD",
	14:  "MSP_CALIBRATION_DATA",
	16:  "MSP_POSITION_ESTIMATION_CONFIG",
	18:  "MSP_WP_MISSION_LOAD",
	19:  "MSP_WP_MISSION_SAVE",
	20:  "MSP_WP_GETINFO",
	21:  "MSP_RTH_AND_LAND_CONFIG",
	23:  "MSP_FW_CONFIG",
	32:  "MSP_BATTERY_CONFIG",
	34:  "MSP_MODE_RANGES",
	35:  "MSP_ADJUSTMENT_RANGES",
	36:  "MSP_VOLTAGE_METER_CONFIG",
	37:  "MSP_CURRENT_METER_CONFIG",
	38:  "MSP_BATTERY_STATE",
	48:  "MSP_MOTOR_CONFIG",
	54:  "MSP_FEATURE_CONFIG",
	56:  "MSP_BOARD_ALIGNMENT_CONFIG",
	58:  "MSP_RX_CONFIG",
	60:  "MSP_LED_STRIP_CONFIG",
	61:  "MSP_RSSI_CONFIG",
	64:  "MSP_LED_COLORS",
	68:  "MSP_FAILSAFE_CONFIG",
	70:  "MSP_RXFAIL_CONFIG",
	75:  "MSP_RX_MAP",
	86:  "MSP_VTX_CONFIG",
	88:  "MSP_VTXTABLE_BAND",
	89:  "MSP_VTXTABLE_POWERLEVEL",
	90:  "MSP_MOTOR_TELEMETRY",
	92:  "MSP_SIMPLIFIED_TUNING",
	101: "MSP_STATUS",
	102: "MSP_RAW_IMU",
	103: "MSP_SERVO",
	104: "MSP_MOTOR",
	105: "MSP_RC",
	106: "MSP_RAW_GPS",
	107: "MSP_COMP_GPS",
	108: "MSP_ATTITUDE",
	109: "MSP_ALTITUDE",
	110: "MSP_ANALOG",
	111: "MSP_RC_TUNING",
	112: "MSP_PID",
	113: "MSP_ACTIVEBOXES",
	114: "MSP_MISC",
	115: "MSP_MOTOR_PINS",
	116: "MSP_BOXNAMES",
	117: "MSP_PIDNAMES",
	118: "MSP_WP",
	119: "MSP_BOXIDS",
	120: "MSP_SERVO_CONFIGURATIONS",
	121: "MSP_NAV_STATUS",
	122: "MSP_NAV_CONFIG",
	123: "MSP_MOTOR_3D_CONFIG",
	124: "MSP_RC_DEADBAND",
	125: "MSP_SENSOR_ALIGNMENT",
	126: "MSP_LED_STRIP_MODECOLOR",
	127: "MSP_VOLTAGE_METERS",
	128: "MSP_CURRENT_METERS",
	130: "MSP_BATTERY_STATE",
	131: "MSP_MOTOR_CONFIG_LEGACY",
	137: "MSP_OSD_CONFIG",
	150: "MSP_STATUS_EX",
	151: "MSP_SENSOR_STATUS",
	160: "MSP_UID",
	164: "MSP_GPSSVINFO",
	166: "MSP_GPSSTATISTICS",
	141: "MSP_SET_SIMPLIFIED_TUNING",
	181: "MSP_SET_OSD_VIDEO_CONFIG",
	183: "MSP_COPY_PROFILE",
	185: "MSP_SET_BEEPER_CONFIG",
	186: "MSP_SET_TX_INFO",
	187: "MSP_TX_INFO",
	188: "MSP_SET_OSD_CANVAS",
	240: "MSP_ACC_TRIM",
}

// Name returns the human-readable name for an MSP code, or "MSP_UNKNOWN" if
// bfctl doesn't have it in its table. Always safe — never panics, never hits
// the wire.
func Name(code uint8) string {
	if n, ok := names[code]; ok {
		return n
	}
	return "MSP_UNKNOWN"
}

// Decode renders an MSP response payload as a human-readable string for the
// codes bfctl knows how to parse. Unknown shapes return "" — callers should
// fall back to a hex dump.
//
// Decoded codes:
//
//	  1 API_VERSION  → "proto N, api X.Y"
//	  2 FC_VARIANT   → ASCII tag (e.g. "BTFL")
//	  3 FC_VERSION   → "major.minor.patch"
//	 10 NAME         → craft name as ASCII
//	116 BOXNAMES     → comma-joined list
//	117 PIDNAMES     → comma-joined list
//	119 BOXIDS       → space-joined decimal IDs
func Decode(code uint8, payload []byte) string {
	switch code {
	case CmdAPIVersion:
		if len(payload) < 3 {
			return ""
		}
		return fmt.Sprintf("proto %d, api %d.%d", payload[0], payload[1], payload[2])
	case CmdFCVariant:
		return strings.TrimRight(string(payload), "\x00")
	case CmdFCVersion:
		if len(payload) < 3 {
			return ""
		}
		return fmt.Sprintf("%d.%d.%d", payload[0], payload[1], payload[2])
	case CmdName:
		return strings.TrimRight(string(payload), "\x00")
	case CmdBoxNames, CmdPIDNames:
		return decodeSemiList(payload)
	case CmdBoxIDs:
		return decodeBoxIDs(payload)
	}
	return ""
}

func decodeSemiList(payload []byte) string {
	s := strings.TrimRight(string(payload), "\x00")
	s = strings.TrimSuffix(s, ";")
	parts := strings.Split(s, ";")
	return strings.Join(parts, ", ")
}

func decodeBoxIDs(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var b strings.Builder
	for i, id := range payload {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d", id)
	}
	return b.String()
}
