package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	"github.com/emiago/sipgo/parser"
	"github.com/emiago/sipgo/sip"
)

var (
	// WebSocketProtocols is used in setting websocket header
	// By default clients must accept protocol sip
	WebSocketProtocols = []string{"sip"}
)

// WS transport implementation
type WSTransport struct {
	parser    *parser.Parser
	log       *slog.Logger
	transport string

	pool   ConnectionPool
	dialer ws.Dialer
}

func NewWSTransport(par *parser.Parser) *WSTransport {
	p := &WSTransport{
		parser:    par,
		pool:      NewConnectionPool(),
		transport: TransportWS,
		dialer:    ws.DefaultDialer,
	}

	p.dialer.Protocols = WebSocketProtocols
	p.log = slog.With("caller", "transport<WS>")
	return p
}

func (t *WSTransport) String() string {
	return "transport<WS>"
}

func (t *WSTransport) Network() string {
	return t.transport
}

func (t *WSTransport) Close() error {
	t.pool.Clear()
	return nil
}

// Serve is direct way to provide conn on which this worker will listen
func (t *WSTransport) Serve(l net.Listener, handler sip.MessageHandler) error {
	t.log.Debug("begin listening on", "net", t.Network(), "addr", l.Addr())

	// Prepare handshake header writer from http.Header mapping.
	// Some phones want to return this
	// TODO make this configurable
	header := ws.HandshakeHeaderHTTP(http.Header{
		"Sec-WebSocket-Protocol": WebSocketProtocols,
	})

	u := ws.Upgrader{
		OnBeforeUpgrade: func() (ws.HandshakeHeader, error) {
			return header, nil
		},
	}

	if SIPDebug {
		u.OnHeader = func(key, value []byte) error {
			t.log.Debug("non-websocket header", string(key), string(value))
			return nil
		}
	}

	for {
		conn, err := l.Accept()
		if err != nil {
			t.log.Error("Fail to accept conenction", "err", err)
			return err
		}

		raddr := conn.RemoteAddr().String()

		t.log.Debug("New connection accept", "addr", raddr)

		_, err = u.Upgrade(conn)
		if err != nil {
			t.log.Error("Fail to upgrade", "err", err)
			continue
		}

		t.initConnection(conn, raddr, false, handler)
	}
}

func (t *WSTransport) initConnection(conn net.Conn, addr string, clientSide bool, handler sip.MessageHandler) Connection {
	// // conn.SetKeepAlive(true)
	// conn.SetKeepAlivePeriod(3 * time.Second)
	t.log.Debug("New WS connection", "raddr", addr)
	c := &WSConnection{
		Conn:       conn,
		refcount:   1,
		clientSide: clientSide,
	}
	t.pool.Add(addr, c)
	go t.readConnection(c, addr, handler)
	return c
}

// This should performe better to avoid any interface allocation
func (t *WSTransport) readConnection(conn *WSConnection, raddr string, handler sip.MessageHandler) {
	buf := make([]byte, transportBufferSize)
	// defer conn.Close()
	// defer t.pool.Del(raddr)
	defer t.pool.CloseAndDelete(conn, raddr)

	// Create stream parser context
	par := t.parser.NewSIPStream()

	for {
		num, err := conn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
				t.log.Debug("Read connection closed", "err", err)
				return
			}

			t.log.Error("Got TCP error", "err", err)
			return
		}

		if num == 0 {
			// // What todo
			t.log.Debug("Got no bytes, sleeping")
			time.Sleep(100 * time.Millisecond)
			continue
		}

		data := buf[:num]

		if len(bytes.Trim(data, "\x00")) == 0 {
			continue
		}

		// Check is keep alive
		if len(data) <= 4 {
			//One or 2 CRLF
			if len(bytes.Trim(data, "\r\n")) == 0 {
				t.log.Debug("Keep alive CRLF received")
				continue
			}
		}

		t.parseStream(par, data, raddr, handler)
	}

}

// TODO: Try to reuse this from TCP transport as func are same
func (t *WSTransport) parseStream(par *parser.ParserStream, data []byte, src string, handler sip.MessageHandler) {
	msg, err := t.parser.ParseSIP(data) //Very expensive operation
	if err != nil {
		t.log.Error("failed to parse", "err", err, "data", string(data))
		return
	}

	msg.SetTransport(t.transport)
	msg.SetSource(src)
	handler(msg)
}

// TODO use this when message size limit is defined
func (t *WSTransport) parseFull(data []byte, src string, handler sip.MessageHandler) {
	msg, err := t.parser.ParseSIP(data) //Very expensive operation
	if err != nil {
		t.log.Error("failed to parse", "err", err, "data", string(data))
		return
	}

	msg.SetTransport(t.transport)
	msg.SetSource(src)
	handler(msg)
}

func (t *WSTransport) ResolveAddr(addr string) (net.Addr, error) {
	return net.ResolveTCPAddr("tcp", addr)
}

func (t *WSTransport) GetConnection(addr string) (Connection, error) {
	raddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	addr = raddr.String()

	c := t.pool.Get(addr)
	return c, nil
}

func (t *WSTransport) CreateConnection(laddr Addr, raddr Addr, handler sip.MessageHandler) (Connection, error) {
	// raddr, err := net.ResolveTCPAddr("tcp", addr)
	// if err != nil {
	// 	return nil, err
	// }

	var tladdr *net.TCPAddr = nil
	if laddr.IP != nil {
		tladdr = &net.TCPAddr{
			IP:   laddr.IP,
			Port: laddr.Port,
		}
	}

	traddr := &net.TCPAddr{
		IP:   raddr.IP,
		Port: raddr.Port,
	}
	return t.createConnection(tladdr, traddr, handler)
}

func (t *WSTransport) createConnection(laddr *net.TCPAddr, raddr *net.TCPAddr, handler sip.MessageHandler) (Connection, error) {
	addr := raddr.String()
	t.log.Debug("Dialing new connection", "raddr", addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// How to define local interface
	if laddr != nil {
		t.log.Error("Dialing with local IP is not supported on ws", "laddr", laddr.String())
	}

	conn, _, _, err := t.dialer.Dial(ctx, "ws://"+addr)
	if err != nil {
		return nil, fmt.Errorf("%s dial err=%w", t, err)
	}

	c := t.initConnection(conn, addr, true, handler)
	return c, nil
}

type WSConnection struct {
	net.Conn

	clientSide bool
	mu         sync.RWMutex
	refcount   int
}

func (c *WSConnection) Ref(i int) int {
	c.mu.Lock()
	c.refcount += i
	ref := c.refcount
	c.mu.Unlock()
	slog.Debug("WS reference increment", "ip", c.RemoteAddr(), "ref", ref)
	return ref

}

func (c *WSConnection) Close() error {
	c.mu.Lock()
	c.refcount = 0
	c.mu.Unlock()
	slog.Debug("WS doing hard close", "ip", c.RemoteAddr(), "ref", c.refcount)
	return c.Conn.Close()
}

func (c *WSConnection) TryClose() (int, error) {
	c.mu.Lock()
	c.refcount--
	ref := c.refcount
	c.mu.Unlock()
	slog.Debug("WS reference decrement", "ip", c.RemoteAddr(), "ref", c.refcount)
	if ref > 0 {
		return ref, nil
	}

	if ref < 0 {
		slog.Warn("WS ref went negative", "ip", c.RemoteAddr(), "ref", c.refcount)
		return 0, nil
	}
	slog.Debug("WS closing", "ip", c.RemoteAddr(), "ref", c.refcount)
	return ref, c.Conn.Close()
}

func (c *WSConnection) Read(b []byte) (n int, err error) {
	state := ws.StateServerSide
	if c.clientSide {
		state = ws.StateClientSide
	}
	reader := wsutil.NewReader(c.Conn, state)
	for {
		header, err := reader.NextFrame()
		if err != nil {
			if errors.Is(err, io.EOF) && n > 0 {
				return n, nil
			}
			return n, err
		}

		if SIPDebug {
			slog.Debug("WS read connection header", "caller", c.RemoteAddr(), "op", header.OpCode, "sz", header.Length)
		}

		if header.OpCode == ws.OpClose {
			return n, net.ErrClosed
		}

		data := make([]byte, header.Length)

		// Read until
		_, err = io.ReadFull(c.Conn, data)
		if err != nil {
			return n, err
		}

		// if header.OpCode == ws.OpPing {
		// 	f := ws.NewPongFrame(data)
		// 	ws.WriteFrame(c.Conn, f)
		// 	continue
		// }

		if SIPDebug {
			slog.Debug("WS read", "local", c.Conn.LocalAddr(), "remote", c.Conn.RemoteAddr(), "data", string(data))
		}

		if header.Masked {
			ws.Cipher(data, header.Mask, 0)
		}
		// header.Masked = false

		n += copy(b[n:], data)

		if header.Fin {
			break
		}
	}

	return n, nil
}

func (c *WSConnection) Write(b []byte) (n int, err error) {
	if SIPDebug {
		slog.Debug("WS write", "caller", c.LocalAddr(), "remote", c.Conn.RemoteAddr(), "data", string(b))
	}

	fs := ws.NewFrame(ws.OpText, true, b)
	if c.clientSide {
		fs = ws.MaskFrameInPlace(fs)
	}
	err = ws.WriteFrame(c.Conn, fs)

	return len(b), err
}

func (c *WSConnection) WriteMsg(msg sip.Message) error {
	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()
	msg.StringWrite(buf)
	data := buf.Bytes()

	n, err := c.Write(data)
	if err != nil {
		return fmt.Errorf("conn %s write err=%w", c.RemoteAddr().String(), err)
	}

	if n == 0 {
		return fmt.Errorf("wrote 0 bytes")
	}

	if n != len(data) {
		return fmt.Errorf("fail to write full message")
	}
	return nil
}
