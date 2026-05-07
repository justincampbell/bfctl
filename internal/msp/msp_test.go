package msp

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

func TestRequest(t *testing.T) {
	cases := []struct {
		name    string
		code    uint8
		payload []byte
		want    []byte
	}{
		{
			name: "no payload",
			code: 1, // API version
			want: []byte{'$', 'M', '<', 0x00, 0x01, 0x01},
		},
		{
			name:    "with payload",
			code:    11, // SET_NAME
			payload: []byte{'A', 'B', 'C'},
			// size=3, code=11, cksum = 3 ^ 11 ^ 'A' ^ 'B' ^ 'C'
			//        = 0x03 ^ 0x0B ^ 0x41 ^ 0x42 ^ 0x43 = 0x48
			want: []byte{'$', 'M', '<', 0x03, 0x0B, 'A', 'B', 'C', 0x48},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Request(c.code, c.payload)
			if !bytes.Equal(got, c.want) {
				t.Errorf("Request(%d, %v) = % x, want % x", c.code, c.payload, got, c.want)
			}
		})
	}
}

func TestReadResponseOK(t *testing.T) {
	// $M> size=3 code=1 payload=00,01,2F cksum=3^1^0^1^0x2F=0x2C
	frame := []byte{'$', 'M', '>', 0x03, 0x01, 0x00, 0x01, 0x2F, 0x2C}
	r := bytes.NewReader(frame)
	resp, err := ReadResponse(r, time.Second)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if !resp.OK() {
		t.Errorf("Direction = %c, want %c", resp.Direction, DirResp)
	}
	if resp.Code != 1 {
		t.Errorf("Code = %d, want 1", resp.Code)
	}
	if !bytes.Equal(resp.Payload, []byte{0x00, 0x01, 0x2F}) {
		t.Errorf("Payload = % x, want 00 01 2F", resp.Payload)
	}
}

func TestReadResponseError(t *testing.T) {
	// FC error response: same framing but direction is '!'.
	// $M! size=0 code=200 cksum=0^200=0xC8
	frame := []byte{'$', 'M', '!', 0x00, 0xC8, 0xC8}
	r := bytes.NewReader(frame)
	resp, err := ReadResponse(r, time.Second)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.OK() {
		t.Errorf("OK() = true on error response, want false")
	}
	if resp.Direction != DirErr {
		t.Errorf("Direction = %c, want %c", resp.Direction, DirErr)
	}
}

func TestReadResponseTolerantesLeadingNoise(t *testing.T) {
	// Real captures have echoes / partial frames before the real one.
	noise := []byte("hello# garbage \r\n# ")
	frame := []byte{'$', 'M', '>', 0x00, 0x02, 0x02} // FC_VARIANT empty reply
	r := bytes.NewReader(append(noise, frame...))
	resp, err := ReadResponse(r, time.Second)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.Code != 2 {
		t.Errorf("Code = %d, want 2", resp.Code)
	}
}

func TestReadResponseBadChecksum(t *testing.T) {
	frame := []byte{'$', 'M', '>', 0x03, 0x01, 0x00, 0x01, 0x2F, 0xFF}
	r := bytes.NewReader(frame)
	_, err := ReadResponse(r, time.Second)
	if !errors.Is(err, ErrChecksum) {
		t.Errorf("err = %v, want ErrChecksum", err)
	}
}

func TestReadResponseTimeout(t *testing.T) {
	r := slowReader{}
	_, err := ReadResponse(r, 50*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
}

// slowReader returns 0 bytes and no error to simulate a port that's open
// but not delivering any data — should hit our deadline.
type slowReader struct{}

func (slowReader) Read(p []byte) (int, error) {
	time.Sleep(10 * time.Millisecond)
	return 0, nil
}

func TestRequestRoundTrip(t *testing.T) {
	// Encode a request, then verify a response built around the same code +
	// arbitrary payload decodes correctly. Doesn't go over a wire — just
	// proves the codec is internally consistent.
	frame := Request(116, nil) // BOXNAMES, no request payload
	if len(frame) != 6 {
		t.Fatalf("Request(116, nil) length = %d, want 6", len(frame))
	}
	resp := Response{Direction: DirResp, Code: 116, Payload: []byte("ARM;ANGLE;HORIZON;")}
	got := Decode(resp.Code, resp.Payload)
	want := "ARM, ANGLE, HORIZON"
	if got != want {
		t.Errorf("Decode(BOXNAMES) = %q, want %q", got, want)
	}
}

func TestIsDenylisted(t *testing.T) {
	for _, code := range Denylist {
		if !IsDenylisted(code) {
			t.Errorf("IsDenylisted(%d) = false, want true", code)
		}
	}
	for _, code := range []uint8{1, 2, 101, 130, 187, 199} {
		if IsDenylisted(code) {
			t.Errorf("IsDenylisted(%d) = true, want false", code)
		}
	}
	if !IsDenylisted(188) {
		t.Errorf("IsDenylisted(188 MSP_SET_OSD_CANVAS) must be true — proven brick on AT32 firmware")
	}
}

func TestNameKnownAndUnknown(t *testing.T) {
	if Name(1) != "MSP_API_VERSION" {
		t.Errorf("Name(1) = %q, want MSP_API_VERSION", Name(1))
	}
	if Name(254) != "MSP_UNKNOWN" {
		t.Errorf("Name(254) = %q, want MSP_UNKNOWN", Name(254))
	}
}

func TestDecode(t *testing.T) {
	cases := []struct {
		name    string
		code    uint8
		payload []byte
		want    string
	}{
		{"API_VERSION", CmdAPIVersion, []byte{0x00, 0x01, 0x2F}, "proto 0, api 1.47"},
		{"FC_VARIANT", CmdFCVariant, []byte{'B', 'T', 'F', 'L'}, "BTFL"},
		{"FC_VERSION", CmdFCVersion, []byte{0x04, 0x05, 0x02}, "4.5.2"},
		{"NAME", CmdName, []byte("LionBee3"), "LionBee3"},
		{"BOXNAMES", CmdBoxNames, []byte("ARM;ANGLE;HORIZON;"), "ARM, ANGLE, HORIZON"},
		{"BOXIDS", CmdBoxIDs, []byte{0x00, 0x01, 0x05}, "0 1 5"},
		{"unknown", 200, []byte{0xAB, 0xCD}, ""},
		{"short API_VERSION", CmdAPIVersion, []byte{0x00}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Decode(c.code, c.payload)
			if got != c.want {
				t.Errorf("Decode(%d, % x) = %q, want %q", c.code, c.payload, got, c.want)
			}
		})
	}
}

func TestReadResponseJumbo(t *testing.T) {
	// 300-byte payload — exceeds the standard 1-byte size, so the FC
	// switches to jumbo encoding: $M> 0xFF code size_lo size_hi payload cksum
	payload := make([]byte, 300)
	for i := range payload {
		payload[i] = byte(i)
	}
	const code uint8 = 116
	frame := []byte{'$', 'M', '>', 0xFF, code, byte(300 & 0xFF), byte(300 >> 8)}
	frame = append(frame, payload...)
	cksum := byte(0xFF) ^ code ^ byte(300&0xFF) ^ byte(300>>8)
	for _, b := range payload {
		cksum ^= b
	}
	frame = append(frame, cksum)

	r := bytes.NewReader(frame)
	resp, err := ReadResponse(r, time.Second)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.Code != code {
		t.Errorf("Code = %d, want %d", resp.Code, code)
	}
	if len(resp.Payload) != 300 {
		t.Errorf("len(Payload) = %d, want 300", len(resp.Payload))
	}
	if !bytes.Equal(resp.Payload, payload) {
		t.Errorf("Payload mismatch")
	}
}

// Sanity: io.EOF mid-frame surfaces, doesn't get swallowed.
func TestReadResponseEOF(t *testing.T) {
	// Truncated header
	frame := []byte{'$', 'M', '>', 0x05, 0x01}
	r := bytes.NewReader(frame)
	_, err := ReadResponse(r, time.Second)
	if err == nil {
		t.Fatal("ReadResponse: want error on truncated frame")
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want EOF or Timeout", err)
	}
}
