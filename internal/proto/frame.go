// Package proto is the wire protocol: frame layout, message types, and
// allocation-free encoding and decoding (decision 008).
//
// Every frame is:
//
//	offset  size  field
//	0       4     length   u32 LE: number of bytes after this field (2 + payload)
//	4       1     version  must be Version
//	5       1     type     MsgType
//	6       n     payload  fixed layout per type, little-endian
//
// The package depends on nothing else in this module. It knows bytes and
// field layouts, not books or engines; the gateway translates.
package proto

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Version is the only protocol version this build speaks. A frame with any
// other version is rejected and the connection closed.
const Version byte = 1

// HeaderSize is the size of the length, version and type fields.
const HeaderSize = 6

// MaxPayload bounds a frame's payload. It protects the server from a
// corrupt or hostile length field asking it to buffer gigabytes. The largest
// current message (Fill) is 70 bytes; the headroom is for future messages.
const MaxPayload = 1024

// Errors returned by Reader.Next and the Decode methods. All of them mean
// the byte stream can't be trusted, so the connection must be closed: with a
// length-prefixed protocol there is no way to resynchronise.
var (
	ErrVersion     = errors.New("proto: unsupported protocol version")
	ErrFrameLength = errors.New("proto: invalid frame length")
	ErrPayloadSize = errors.New("proto: payload size does not match message type")
)

// MsgType identifies a message.
type MsgType uint8

// Inbound (client to server) types are below 0x80; outbound are 0x80 and up.
// Heartbeat is the same type in both directions.
const (
	TypeLogin     MsgType = 0x01
	TypeNewOrder  MsgType = 0x02
	TypeCancel    MsgType = 0x03
	TypeSubscribe MsgType = 0x04
	TypeHeartbeat MsgType = 0x05
	TypeResend    MsgType = 0x06

	TypeLoginAck  MsgType = 0x81
	TypeAck       MsgType = 0x82
	TypeReject    MsgType = 0x83
	TypeFill      MsgType = 0x84
	TypeCancelAck MsgType = 0x85

	// Market data (milestone 6, decision 016).
	TypeBookDelta     MsgType = 0x86
	TypeSnapshot      MsgType = 0x87
	TypeTrade         MsgType = 0x88
	TypeSnapshotLevel MsgType = 0x89
)

func (t MsgType) String() string {
	switch t {
	case TypeLogin:
		return "Login"
	case TypeNewOrder:
		return "NewOrder"
	case TypeCancel:
		return "Cancel"
	case TypeSubscribe:
		return "Subscribe"
	case TypeHeartbeat:
		return "Heartbeat"
	case TypeResend:
		return "Resend"
	case TypeBookDelta:
		return "BookDelta"
	case TypeSnapshot:
		return "Snapshot"
	case TypeTrade:
		return "Trade"
	case TypeSnapshotLevel:
		return "SnapshotLevel"
	case TypeLoginAck:
		return "LoginAck"
	case TypeAck:
		return "Ack"
	case TypeReject:
		return "Reject"
	case TypeFill:
		return "Fill"
	case TypeCancelAck:
		return "CancelAck"
	}
	return fmt.Sprintf("type(0x%02x)", uint8(t))
}

// appendHeader appends a frame header for a payload of n bytes.
func appendHeader(dst []byte, t MsgType, n int) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, uint32(2+n))
	return append(dst, Version, byte(t))
}

// Reader reads frames from a byte stream. It reuses one payload buffer, so
// the slice returned by Next is only valid until the next call: decode it
// before reading again. Not safe for concurrent use; each connection has
// exactly one reader goroutine.
type Reader struct {
	br  *bufio.Reader
	hdr [HeaderSize]byte
	buf [MaxPayload]byte
}

// NewReader returns a Reader that buffers reads from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 16<<10)}
}

// Next reads one frame and returns its type and payload.
//
// It returns io.EOF only if the stream ended cleanly between frames. A stream
// that ends inside a frame gives io.ErrUnexpectedEOF. A bad length or version
// gives ErrFrameLength or ErrVersion, and the caller must close the
// connection.
func (r *Reader) Next() (MsgType, []byte, error) {
	if _, err := io.ReadFull(r.br, r.hdr[:]); err != nil {
		return 0, nil, err // io.EOF if nothing was read, else io.ErrUnexpectedEOF
	}
	length := binary.LittleEndian.Uint32(r.hdr[0:4])
	if length < 2 || length-2 > MaxPayload {
		return 0, nil, fmt.Errorf("%w: %d", ErrFrameLength, length)
	}
	if r.hdr[4] != Version {
		return 0, nil, fmt.Errorf("%w: %d", ErrVersion, r.hdr[4])
	}
	n := int(length - 2)
	if _, err := io.ReadFull(r.br, r.buf[:n]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF // the header promised a payload
		}
		return 0, nil, err
	}
	return MsgType(r.hdr[5]), r.buf[:n], nil
}
