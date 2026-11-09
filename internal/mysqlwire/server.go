// Package mysqlwire implements the MySQL 4.1 text protocol for the SQL engine.
package mysqlwire

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"minivectordb/internal/db"
)

const capabilities uint32 = 1 | 1<<3 | 1<<9 | 1<<13 | 1<<15 | 1<<19
const maxPacket = 0xffffff
const maxMessage = 64 << 20

var connectionID atomic.Uint32

type packets struct {
	conn net.Conn
	seq  byte
}

func (p *packets) read() ([]byte, error) {
	var result []byte
	for {
		var h [4]byte
		if _, err := io.ReadFull(p.conn, h[:]); err != nil {
			return nil, err
		}
		n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
		if h[3] != p.seq {
			return nil, fmt.Errorf("invalid packet sequence")
		}
		p.seq++
		if len(result)+n > maxMessage {
			return nil, fmt.Errorf("message exceeds 64 MiB")
		}
		start := len(result)
		result = append(result, make([]byte, n)...)
		if _, err := io.ReadFull(p.conn, result[start:]); err != nil {
			return nil, err
		}
		if n < maxPacket {
			return result, nil
		}
	}
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func (p *packets) write(b []byte) error {
	for {
		n := len(b)
		if n > maxPacket {
			n = maxPacket
		}
		h := []byte{byte(n), byte(n >> 8), byte(n >> 16), p.seq}
		p.seq++
		if err := writeAll(p.conn, h); err != nil {
			return err
		}
		if err := writeAll(p.conn, b[:n]); err != nil {
			return err
		}
		b = b[n:]
		if n < maxPacket {
			return nil
		}
	}
}
func lenenc(b []byte, n uint64) []byte {
	switch {
	case n < 251:
		return append(b, byte(n))
	case n < 1<<16:
		return append(b, 0xfc, byte(n), byte(n>>8))
	case n < 1<<24:
		return append(b, 0xfd, byte(n), byte(n>>8), byte(n>>16))
	default:
		b = append(b, 0xfe)
		return binary.LittleEndian.AppendUint64(b, n)
	}
}
func str(b []byte, s string) []byte { return append(lenenc(b, uint64(len(s))), s...) }
func (p *packets) ok(n int) error {
	b := lenenc([]byte{0}, uint64(n))
	return p.write(append(b, 0, 2, 0, 0, 0))
}
func (p *packets) fail(code uint16, msg string) error {
	b := []byte{0xff, byte(code), byte(code >> 8), '#', 'H', 'Y', '0', '0', '0'}
	return p.write(append(b, msg...))
}
func (p *packets) result(r db.Result) error {
	if r.Error != "" {
		return p.fail(1105, r.Error)
	}
	if len(r.Columns) == 0 {
		return p.ok(r.Affected)
	}
	if err := p.write(lenenc(nil, uint64(len(r.Columns)))); err != nil {
		return err
	}
	for _, name := range r.Columns {
		b := str(nil, "def")
		for _, s := range []string{"", "", "", name, name} {
			b = str(b, s)
		}
		// All values use textual VARCHAR metadata, including JSON vector values.
		b = append(b, 0x0c, 45, 0, 0xff, 0xff, 0xff, 0x00, 0xfd, 0, 0, 0, 0, 0)
		if err := p.write(b); err != nil {
			return err
		}
	}
	eof := []byte{0xfe, 0, 0, 2, 0}
	if err := p.write(eof); err != nil {
		return err
	}
	for _, row := range r.Rows {
		var b []byte
		for _, v := range row {
			if v == nil {
				b = append(b, 0xfb)
				continue
			}
			var s string
			switch x := v.(type) {
			case string:
				s = x
			default:
				raw, err := json.Marshal(x)
				if err != nil {
					return p.fail(1105, err.Error())
				}
				s = string(raw)
			}
			b = str(b, s)
		}
		if err := p.write(b); err != nil {
			return err
		}
	}
	return p.write(eof)
}

// Serve accepts any credentials. This intentionally unauthenticated contest server
// advertises mysql_native_password and ignores the client's auth response/plugin.
func Serve(c net.Conn, e *db.Engine) {
	defer c.Close()
	p := &packets{conn: c}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	salt := make([]byte, 20)
	if _, err := rand.Read(salt); err != nil {
		return
	}
	b := append([]byte{10}, []byte("8.0.36-minivectordb\x00")...)
	b = binary.LittleEndian.AppendUint32(b, connectionID.Add(1))
	b = append(b, salt[:8]...)
	b = append(b, 0)
	b = binary.LittleEndian.AppendUint16(b, uint16(capabilities&0xffff))
	b = append(b, 45, 2, 0)
	b = binary.LittleEndian.AppendUint16(b, uint16(capabilities>>16))
	b = append(b, 21)
	b = append(b, make([]byte, 10)...)
	b = append(b, salt[8:]...)
	b = append(b, 0)
	b = append(b, []byte("mysql_native_password\x00")...)
	if p.write(b) != nil {
		return
	}
	response, err := p.read()
	if err != nil {
		return
	}
	if len(response) < 33 || binary.LittleEndian.Uint32(response)&(1<<9) == 0 {
		p.fail(1043, "Protocol 4.1 handshake required")
		return
	}
	if p.ok(0) != nil {
		return
	}
	c.SetDeadline(time.Time{})
	s := e.Session()
	for {
		p.seq = 0
		command, err := p.read()
		if err != nil {
			return
		}
		if len(command) == 0 {
			if p.fail(1047, "empty command") != nil {
				return
			}
			continue
		}
		switch command[0] {
		case 1:
			return // COM_QUIT
		case 2, 14:
			err = p.ok(0) // COM_INIT_DB (single namespace), COM_PING
		case 3:
			sql := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(string(command[1:])), ";"))
			switch strings.ToUpper(sql) {
			case "SELECT 1":
				err = p.result(db.Result{Columns: []string{"1"}, Rows: [][]any{{1}}})
			case "SELECT VERSION()":
				err = p.result(db.Result{Columns: []string{"VERSION()"}, Rows: [][]any{{"8.0.36-minivectordb"}}})
			case "SET NAMES UTF8MB4", "SET NAMES UTF8":
				err = p.ok(0)
			default:
				err = p.result(s.Execute(sql))
			}
		default:
			err = p.fail(1047, "unsupported MySQL command; use COM_QUERY")
		}
		if err != nil {
			return
		}
	}
}
