package transport

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	MaxFrameSize = 4 << 20 // 4 MB — must fit worst case: 256 raw 4KB blocks + headers
	MagicHeader  uint16 = 0x7E17
	BufSize      = 256 * 1024 // 256KB read/write buffers
)

// Conn wraps a TCP connection with buffered Telepathi framing
type Conn struct {
	conn   net.Conn
	writer *bufio.Writer
	reader *bufio.Reader
	mu     sync.Mutex
	readMu sync.Mutex
	BytesTx uint64
	BytesRx uint64
}

// NewConn wraps a net.Conn with buffered I/O
func NewConn(conn net.Conn) *Conn {
	// Enable TCP_NODELAY to reduce latency on small control messages
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetWriteBuffer(BufSize)
		tc.SetReadBuffer(BufSize)
	}
	return &Conn{
		conn:   conn,
		writer: bufio.NewWriterSize(conn, BufSize),
		reader: bufio.NewReaderSize(conn, BufSize),
	}
}

// Dial connects to a remote Telepathi endpoint
func Dial(addr string, timeout time.Duration) (*Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return NewConn(conn), nil
}

// Listen starts a Telepathi listener
func Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return ln, nil
}

// SendMessage sends a length-prefixed message with magic header
func (c *Conn) SendMessage(msgType byte, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(data) > MaxFrameSize {
		return fmt.Errorf("message too large: %d > %d", len(data), MaxFrameSize)
	}

	// Combine header + payload into one write
	buf := make([]byte, 7+len(data))
	binary.BigEndian.PutUint16(buf[0:2], MagicHeader)
	buf[2] = msgType
	binary.BigEndian.PutUint32(buf[3:7], uint32(len(data)))
	copy(buf[7:], data)

	if _, err := c.writer.Write(buf); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := c.writer.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}

	c.BytesTx += uint64(len(buf))
	return nil
}

// RecvMessage receives a length-prefixed message
func (c *Conn) RecvMessage() (msgType byte, data []byte, err error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	header := make([]byte, 7)
	if _, err := io.ReadFull(c.reader, header); err != nil {
		return 0, nil, fmt.Errorf("read header: %w", err)
	}

	magic := binary.BigEndian.Uint16(header[0:2])
	if magic != MagicHeader {
		return 0, nil, fmt.Errorf("invalid magic: %04x", magic)
	}

	msgType = header[2]
	length := binary.BigEndian.Uint32(header[3:7])

	if length > MaxFrameSize {
		return 0, nil, fmt.Errorf("message too large: %d", length)
	}

	data = make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(c.reader, data); err != nil {
			return 0, nil, fmt.Errorf("read payload: %w", err)
		}
	}

	c.BytesRx += uint64(7 + len(data))
	return msgType, data, nil
}

// Close closes the connection
func (c *Conn) Close() error {
	return c.conn.Close()
}

// RemoteAddr returns the remote address
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// LocalAddr returns the local address
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// SetDeadline sets the connection deadline
func (c *Conn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

// Message types
const (
	MsgHandshake   byte = 0x01
	MsgSeedExchange byte = 0x02
	MsgProfile     byte = 0x03
	MsgReady       byte = 0x04
	MsgData        byte = 0x10 // Telepathi-encoded data frame
	MsgRawData     byte = 0x11 // Raw data fallback
	MsgFileStart   byte = 0x20 // File transfer begin
	MsgFileEnd     byte = 0x21 // File transfer complete
	MsgStats       byte = 0x30 // Stats exchange
	MsgClose       byte = 0xFF
)
