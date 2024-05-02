// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rtp

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtp"
)

var _ Writer = (*Conn)(nil)

const (
	timeoutCheckInterval = time.Second * 30
)

func NewConn(timeoutCallback func(), name string) *Conn {
	c := &Conn{
		name:    name,
		readBuf: make([]byte, 1500), // MTU
	}
	if timeoutCallback != nil {
		c.onTimeout(timeoutCallback)
	}
	return c
}

type Conn struct {
	name        string
	wmu         sync.Mutex
	conn        *net.UDPConn
	closed      core.Fuse
	readBuf     []byte
	packetCount atomic.Uint64

	dest  atomic.Pointer[net.UDPAddr]
	onRTP atomic.Pointer[Handler]
}

func (c *Conn) LocalAddr() *net.UDPAddr {
	//fmt.Printf("LocalAddr %#v\n", c)
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.LocalAddr().(*net.UDPAddr)
}

func (c *Conn) DestAddr() *net.UDPAddr {
	//fmt.Printf("DestAddr %#v\n", c)
	if c == nil {
		return nil
	}
	return c.dest.Load()
}

func (c *Conn) SetDestAddr(addr *net.UDPAddr) {

	fmt.Printf("SetDestAddr %#v\n", addr)
	c.dest.Store(addr)
}

func (c *Conn) OnRTP(h Handler) {
	//fmt.Printf("OnRTP %#v\n", c)
	if c == nil {
		return
	}
	if h == nil {
		c.onRTP.Store(nil)
	} else {
		c.onRTP.Store(&h)
	}
}

func (c *Conn) Close() error {
	//fmt.Printf("Close %#v\n", c)
	if c == nil {
		return nil
	}
	c.closed.Once(func() {
		c.conn.Close()
	})
	logger.Debugw("Connection closed")
	return nil
}

func (c *Conn) Listen(portMin, portMax int, listenAddr string) error {
	//fmt.Printf("Listen %#v\n", c)
	if listenAddr == "" {
		listenAddr = "0.0.0.0"
	}

	var err error
	c.conn, err = ListenUDPPortRange(portMin, portMax, net.ParseIP(listenAddr).To4())
	logger.Debugw("Listener initialized", "conn", c.conn.LocalAddr())
	if err != nil {
		logger.Debugw("Failed to listen on UDP Port", "error", err)
		return err
	}
	return nil
}

func (c *Conn) ListenAndServe(portMin, portMax int, listenAddr string) error {

	if err := c.Listen(portMin, portMax, listenAddr); err != nil {

		return err
	}
	go c.readLoop()
	fmt.Printf("ListenAndServe %#v\n", c.conn)
	return nil
}

func (c *Conn) readLoop() {
	//fmt.Printf("ReadLoop start %#v\n", c)
	conn, buf := c.conn, c.readBuf
	var p rtp.Packet
	var er error
	for {
		n, srcAddr, err := conn.ReadFromUDP(buf)

		if err != nil {
			logger.Debugw("Failed to read from UDP", "error", err)
			return
		}
		c.dest.Store(srcAddr)

		p = rtp.Packet{}

		if err := p.Unmarshal(buf[:n]); err != nil {
			logger.Debugw("Failed to unmarshal RTP packet", "error", err, "remote", srcAddr.String())
			continue
		}

		c.packetCount.Add(1)
		if h := c.onRTP.Load(); h != nil {
			//fmt.Printf("RTP Read %#v", h)
			er = (*h).HandleRTP(&p)
			if er != nil {
				logger.Debugw("RTP Handler", "error", er, "header", p.Header, "remote", srcAddr.String(), "local", conn.LocalAddr().String())
			}
		} else {
			logger.Debugw("RTP Handler load error")
		}
	}
}

func (c *Conn) WriteRTP(p *rtp.Packet) error {
	//fmt.Printf("WriteRTP %#v\n", c)
	addr := c.DestAddr()
	if addr == nil {
		return nil
	}
	data, err := p.Marshal()
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	//logger.Debugw("RTP Write", "header", p.Header, "remote", addr.String(), "local", c.conn.LocalAddr().String())
	_, err = c.conn.WriteTo(data, addr)
	//_, err = c.conn.WriteTo(data, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 60000})
	return err
}

func (c *Conn) ReadRTP() (*rtp.Packet, *net.UDPAddr, error) {

	buf := c.readBuf
	n, addr, err := c.conn.ReadFromUDP(buf)

	if err != nil {
		return nil, nil, err
	}
	var p rtp.Packet
	if err = p.Unmarshal(buf[:n]); err != nil {
		//logger.Debugw("RTP Read", "header", p.Header, "remote", c.conn.RemoteAddr().String(), "local", c.conn.LocalAddr().String())
		return nil, addr, err
	}
	return &p, addr, nil
}

func (c *Conn) onTimeout(timeoutCallback func()) {
	//fmt.Printf("onTimeout %#v\n", c)
	go func() {
		ticker := time.NewTicker(timeoutCheckInterval)
		defer ticker.Stop()

		var lastPacketCount uint64
		for {
			select {
			case <-c.closed.Watch():
				return
			case <-ticker.C:
				currentPacketCount := c.packetCount.Load()
				if currentPacketCount == lastPacketCount {
					timeoutCallback()
					return
				}

				lastPacketCount = currentPacketCount
			}
		}
	}()
}
