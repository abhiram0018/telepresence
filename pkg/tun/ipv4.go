package tun

import (
	"encoding/binary"
	"errors"

	"golang.org/x/net/ipv4"
)

// ParseIPv4Header parses b as a wire-format IPv4 header and stores the result in h.
func ParseIPv4Header(b []byte) (*ipv4.Header, error) {
	if b == nil {
		return nil, errors.New("nil header")
	}
	if len(b) < ipv4.HeaderLen {
		return nil, errors.New("header too short")
	}
	headerLen := int(b[0]&0x0f) << 2
	if len(b) < headerLen {
		return nil, errors.New("extension header too short")
	}
	fragOff := int(binary.BigEndian.Uint16(b[6:8]))
	return &ipv4.Header{
		Version:  int(b[0] >> 4),
		Len:      headerLen,
		TOS:      int(b[1]),
		TotalLen: int(binary.BigEndian.Uint16(b[2:4])),
		ID:       int(binary.BigEndian.Uint16(b[4:6])),
		Flags:    ipv4.HeaderFlags(fragOff&0xe000) >> 13,
		FragOff:  fragOff & 0x1fff,
		TTL:      int(b[8]),
		Protocol: int(b[9]),
		Checksum: int(binary.BigEndian.Uint16(b[10:12])),
		Src:      b[12:16],
		Dst:      b[16:20],
		Options:  b[20:headerLen],
	}, nil
}
