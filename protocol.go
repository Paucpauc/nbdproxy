package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

var be = binary.BigEndian

const (
	nbdMagic     uint64 = 0x4e42444d41474943
	optMagic     uint64 = 0x49484156454f5054
	repMagic     uint64 = 0x3e889045565a9
	requestMagic uint32 = 0x25609513
	replyMagic   uint32 = 0x67446698
	maxOption           = 8192
	maxRequest          = 32 << 20
	// Only advertise commands implemented here. In particular, never multi-conn,
	// structured replies, write-zeroes, or fast-zero.
	supportedFlags uint16 = 1 | 2 | 4 | 8 | 32 | 256
)

type export struct {
	Size  uint64
	Flags uint16
}

func (e export) public() export { e.Flags &= supportedFlags; e.Flags &^= 256; return e }
func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
func send(w io.Writer, values ...any) error {
	for _, v := range values {
		if p, ok := v.([]byte); ok {
			if err := writeAll(w, p); err != nil {
				return err
			}
		} else if err := binary.Write(w, be, v); err != nil {
			return err
		}
	}
	return nil
}
func optionReply(c io.Writer, opt, kind uint32, p []byte) error {
	return send(c, repMagic, opt, kind, uint32(len(p)), p)
}
func upstreamHandshake(c net.Conn, name string, timeout time.Duration) (export, error) {
	c.SetDeadline(time.Now().Add(timeout))
	defer c.SetDeadline(time.Time{})
	var hello struct {
		Magic, Opt uint64
		Flags      uint16
	}
	if err := binary.Read(c, be, &hello); err != nil {
		return export{}, err
	}
	if hello.Magic != nbdMagic || hello.Opt != optMagic || hello.Flags&1 == 0 {
		return export{}, errors.New("backend must support fixed-newstyle NBD")
	}
	flags := uint32(1)
	if hello.Flags&2 != 0 {
		flags |= 2
	}
	if err := send(c, flags, optMagic, uint32(1), uint32(len(name)), []byte(name)); err != nil {
		return export{}, err
	}
	var e export
	if err := binary.Read(c, be, &e); err != nil {
		return e, err
	}
	if flags&2 == 0 {
		var pad [124]byte
		if _, err := io.ReadFull(c, pad[:]); err != nil {
			return e, err
		}
	}
	if e.Size == 0 || e.Flags&1 == 0 {
		return e, errors.New("invalid backend export")
	}
	return e, nil
}
func exportName(opt uint32, p []byte) (string, error) {
	if opt == 1 {
		return string(p), nil
	}
	if len(p) < 6 {
		return "", errors.New("short INFO/GO")
	}
	n := uint64(be.Uint32(p))
	if n > uint64(len(p)-6) {
		return "", errors.New("invalid export length")
	}
	if uint64(len(p)) != 4+n+2+2*uint64(be.Uint16(p[4+n:])) {
		return "", errors.New("invalid info requests")
	}
	return string(p[4 : 4+n]), nil
}

// downstreamHandshake only publishes geometry after the backend has negotiated.
func downstreamHandshake(c net.Conn, name string, e export, timeout time.Duration) error {
	c.SetDeadline(time.Now().Add(timeout))
	defer c.SetDeadline(time.Time{})
	if err := send(c, nbdMagic, optMagic, uint16(3)); err != nil {
		return err
	}
	var flags uint32
	if err := binary.Read(c, be, &flags); err != nil {
		return err
	}
	if flags&1 == 0 || flags & ^uint32(3) != 0 {
		return errors.New("invalid client handshake flags")
	}
	for {
		var h struct {
			Magic       uint64
			Opt, Length uint32
		}
		if err := binary.Read(c, be, &h); err != nil {
			return err
		}
		if h.Magic != optMagic || h.Length > maxOption {
			return errors.New("invalid option header")
		}
		p := make([]byte, h.Length)
		if _, err := io.ReadFull(c, p); err != nil {
			return err
		}
		switch h.Opt {
		case 2:
			if err := optionReply(c, h.Opt, 1, nil); err != nil {
				return err
			}
			return io.EOF
		case 3:
			if len(p) != 0 {
				if err := optionReply(c, h.Opt, 0x80000003, nil); err != nil {
					return err
				}
				continue
			}
			q := make([]byte, 4+len(name))
			be.PutUint32(q, uint32(len(name)))
			copy(q[4:], name)
			if err := optionReply(c, h.Opt, 2, q); err != nil {
				return err
			}
			if err := optionReply(c, h.Opt, 1, nil); err != nil {
				return err
			}
		case 1, 6, 7:
			selected, err := exportName(h.Opt, p)
			if err != nil {
				if err = optionReply(c, h.Opt, 0x80000003, nil); err != nil {
					return err
				}
				continue
			}
			if selected != name {
				if h.Opt == 1 {
					return errors.New("unknown export")
				}
				if err := optionReply(c, h.Opt, 0x80000006, nil); err != nil {
					return err
				}
				continue
			}
			if h.Opt == 1 {
				if err := send(c, e); err != nil {
					return err
				}
				if flags&2 == 0 {
					if err := send(c, [124]byte{}); err != nil {
						return err
					}
				}
				return nil
			}
			q := make([]byte, 12)
			be.PutUint64(q[2:], e.Size)
			be.PutUint16(q[10:], e.Flags)
			if err := optionReply(c, h.Opt, 3, q); err != nil {
				return err
			}
			// Supply block sizes even if not requested: protocol allows extra INFO.
			q = make([]byte, 14)
			be.PutUint16(q, 3)
			be.PutUint32(q[2:], 1)
			be.PutUint32(q[6:], 4096)
			be.PutUint32(q[10:], maxRequest)
			if err := optionReply(c, h.Opt, 3, q); err != nil {
				return err
			}
			if err := optionReply(c, h.Opt, 1, nil); err != nil {
				return err
			}
			if h.Opt == 7 {
				return nil
			}
		default:
			if err := optionReply(c, h.Opt, 0x80000001, nil); err != nil {
				return err
			}
		}
	}
}

type request struct {
	Magic          uint32
	Flags, Type    uint16
	Handle, Offset uint64
	Length         uint32
}
type response struct {
	Magic, Error uint32
	Handle       uint64
}

func validate(r request, e export) uint32 {
	if r.Type > 4 {
		return 22
	}
	if r.Flags & ^uint16(1) != 0 || (r.Flags != 0 && r.Type != 1 && r.Type != 4) {
		return 22
	}
	if r.Flags&1 != 0 && e.Flags&8 == 0 {
		return 22
	}
	switch r.Type {
	case 0, 1, 4:
		if r.Length == 0 || r.Offset > e.Size || uint64(r.Length) > e.Size-r.Offset {
			return 22
		}
		if (r.Type == 0 || r.Type == 1) && r.Length > maxRequest {
			return 22
		}
		if (r.Type == 1 || r.Type == 4) && e.Flags&2 != 0 {
			return 1
		}
		if r.Type == 4 && e.Flags&32 == 0 {
			return 22
		}
	case 3:
		if r.Offset != 0 || r.Length != 0 || e.Flags&4 == 0 {
			return 22
		}
	}
	return 0
}
func exchange(c net.Conn, r request, p []byte, timeout time.Duration) (response, []byte, error) {
	c.SetDeadline(time.Now().Add(timeout))
	defer c.SetDeadline(time.Time{})
	if err := send(c, r, p); err != nil {
		return response{}, nil, err
	}
	var reply response
	if err := binary.Read(c, be, &reply); err != nil {
		return reply, nil, err
	}
	if reply.Magic != replyMagic || reply.Handle != r.Handle {
		return reply, nil, fmt.Errorf("invalid backend reply magic or handle")
	}
	var data []byte
	if reply.Error == 0 && r.Type == 0 {
		data = make([]byte, r.Length)
		if _, err := io.ReadFull(c, data); err != nil {
			return reply, nil, err
		}
	}
	return reply, data, nil
}
