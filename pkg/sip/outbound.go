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

package sip

import (
	"context"
	"fmt"
	"sync"

	"github.com/emiago/sipgo/sip"
	"github.com/frostbyte73/core"
	"github.com/icholy/digest"
	"github.com/livekit/protocol/logger"
	"github.com/pion/sdp/v2"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/media/ulaw"
	"github.com/livekit/sip/pkg/stats"
)

type sipOutboundConfig struct {
	address string
	from    string
	to      string
	user    string
	pass    string
}

type outboundCall struct {
	c       *Client
	rtpConn *MediaConn
	stopped core.Fuse

	mu            sync.RWMutex
	mon           *stats.CallMonitor
	mediaRunning  bool
	lkRoom        *Room
	lkRoomIn      media.Writer[media.PCM16Sample]
	sipCur        sipOutboundConfig
	sipInviteReq  *sip.Request
	sipInviteResp *sip.Response
	sipRunning    bool
}

func (c *Client) newCall(conf *config.Config, room lkRoomConfig) (*outboundCall, error) {
	call := &outboundCall{
		c:       c,
		rtpConn: NewMediaConn(),
		stopped: core.NewFuse(),
	}
	if err := call.startMedia(conf); err != nil {
		call.close("media-failed")
		return nil, fmt.Errorf("start media failed: %w", err)
	}
	if err := call.updateRoom(room); err != nil {
		call.close("join-failed")
		return nil, fmt.Errorf("update room failed: %w", err)
	}

	c.cmu.Lock()
	defer c.cmu.Unlock()
	c.activeCalls[call] = struct{}{}
	return call, nil
}

func (c *outboundCall) Closed() <-chan struct{} {
	return c.stopped.Watch()
}

func (c *outboundCall) Disconnected() <-chan struct{} {
	if c.lkRoom == nil {
		return nil
	}
	return c.lkRoom.Closed()
}

func (c *outboundCall) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.close("shutdown")
	return nil
}

func (c *outboundCall) CloseWithReason(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.close(reason)
}

func (c *outboundCall) close(reason string) {
	if c.stopped.IsBroken() {
		return
	}
	c.stopped.Break()
	c.rtpConn.OnRTP(nil)
	c.lkRoom.SetOutput(nil)

	if c.mediaRunning {
		_ = c.rtpConn.Close()
	}
	c.mediaRunning = false

	if c.lkRoom != nil {
		_ = c.lkRoom.Close()
	}
	c.lkRoom = nil
	c.lkRoomIn = nil

	c.stopSIP(reason)
	c.sipCur = sipOutboundConfig{}

	c.c.cmu.Lock()
	delete(c.c.activeCalls, c)
	c.c.cmu.Unlock()
}

func (c *outboundCall) Participant() Participant {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lkRoom.Participant()
}

func (c *outboundCall) UpdateSIP(ctx context.Context, sipNew sipOutboundConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sipCur == sipNew {
		return nil
	}
	p := c.lkRoom.Participant()
	if sipNew.address == "" || sipNew.to == "" {
		logger.Infow("Shutdown of outbound SIP call",
			"roomName", p.RoomName, "from", sipNew.from, "to", sipNew.to, "address", sipNew.address)
		// shutdown the call
		c.close("shutdown")
		return nil
	}
	c.startMonitor(sipNew)
	if err := c.updateSIP(sipNew); err != nil {
		c.close("invite-failed")
		return fmt.Errorf("update SIP failed: %w", err)
	}
	c.relinkMedia()
	logger.Infow("Outbound SIP update complete",
		"roomName", p.RoomName, "from", sipNew.from, "to", sipNew.to, "address", sipNew.address)
	return nil
}

func (c *outboundCall) startMonitor(conf sipOutboundConfig) {
	c.mon = c.c.mon.NewCall(stats.Outbound, conf.from, conf.to)
}

func (c *outboundCall) startMedia(conf *config.Config) error {
	if c.mediaRunning {
		return nil
	}
	if err := c.rtpConn.Start(conf.RTPPort.Start, conf.RTPPort.End, "0.0.0.0"); err != nil {
		return err
	}
	c.mediaRunning = true
	return nil
}

func (c *outboundCall) updateRoom(lkNew lkRoomConfig) error {
	if c.lkRoom != nil {
		_ = c.lkRoom.Close()
		c.lkRoom = nil
		c.lkRoomIn = nil
	}
	r := NewRoom()
	if err := r.Connect(c.c.conf, lkNew.roomName, lkNew.identity, lkNew.wsUrl, lkNew.token); err != nil {
		return err
	}
	local, err := r.NewParticipantTrack()
	if err != nil {
		_ = r.Close()
		return err
	}
	c.lkRoom = r
	c.lkRoomIn = local
	return nil
}

func (c *outboundCall) updateSIP(sipNew sipOutboundConfig) error {
	if c.sipCur == sipNew {
		return nil
	}
	c.stopSIP("update")
	if err := c.sipSignal(sipNew); err != nil {
		return err
	}

	if c.sipInviteResp != nil && c.rtpConn != nil {
		offer := sdp.SessionDescription{}
		if err := offer.Unmarshal(c.sipInviteResp.Body()); err != nil {
			return err
		}

		if dst := sdpGetAudioDest(offer); dst != nil {
			c.rtpConn.SetDestAddr(dst)
		}
	}

	c.sipRunning = true
	c.sipCur = sipNew
	return nil
}

func (c *outboundCall) relinkMedia() {
	if c.lkRoom == nil || !c.mediaRunning {
		c.lkRoom.SetOutput(nil)
		c.rtpConn.OnRTP(nil)
		return
	}
	// Encoding pipeline (LK -> SIP)
	s := rtp.NewMediaStreamOut[ulaw.Sample](&rtpStatsWriter{mon: c.mon, w: c.rtpConn}, rtpPacketDur)
	c.lkRoom.SetOutput(ulaw.Encode(s))

	// Decoding pipeline (SIP -> LK)
	law := ulaw.Decode(c.lkRoomIn)
	c.rtpConn.OnRTP(&rtpStatsHandler{mon: c.mon, h: rtp.NewMediaStreamIn(law)})
}

func (c *outboundCall) SendDTMF(ctx context.Context, digits string) error {
	c.mu.RLock()
	running := c.mediaRunning
	c.mu.RUnlock()
	if !running {
		return fmt.Errorf("call is not active")
	}
	// FIXME: c.media.WriteRTP()
	return nil
}

func sipResponse(tx sip.ClientTransaction) (*sip.Response, error) {
	select {
	case <-tx.Done():
		return nil, fmt.Errorf("transaction failed to complete")
	case res := <-tx.Responses():
		if res.StatusCode == 100 || res.StatusCode == 180 || res.StatusCode == 183 {
			return sipResponse(tx)
		}
		return res, nil
	}
}

func (c *outboundCall) stopSIP(reason string) {
	if c.sipInviteReq != nil {
		if err := c.sipBye(); err != nil {
			logger.Errorw("SIP bye failed", err)
		}
		if c.mon != nil {
			c.mon.CallTerminate(reason)
			c.mon.CallEnd()
		}
	}
	c.sipInviteReq = nil
	c.sipInviteResp = nil
	c.sipCur = sipOutboundConfig{}
	c.sipRunning = false
}

func (c *outboundCall) sipSignal(conf sipOutboundConfig) error {
	offer, err := sdpGenerateOffer(c.c.signalingIp, c.rtpConn.LocalAddr().Port)
	if err != nil {
		return err
	}
	c.mon.CallStart()
	joinDur := c.mon.JoinDur()
	inviteReq, inviteResp, err := c.sipInvite(offer, conf)
	if err != nil {
		c.mon.CallEnd()
		logger.Errorw("SIP invite failed", err)
		return err // TODO: should we retry? maybe new offer will work
	}
	err = c.sipAccept(inviteReq, inviteResp)
	if err != nil {
		c.mon.CallEnd()
		logger.Errorw("SIP accept failed", err)
		return err
	}
	joinDur()
	c.sipInviteReq, c.sipInviteResp = inviteReq, inviteResp
	return nil
}

func (c *outboundCall) sipAttemptInvite(offer []byte, conf sipOutboundConfig, authHeader string) (*sip.Request, *sip.Response, error) {
	c.mon.InviteReq()
	to := &sip.Uri{User: conf.to, Host: conf.address, Port: 5060}
	from := &sip.Uri{User: conf.from, Host: c.c.signalingIp, Port: 5060}
	req := sip.NewRequest(sip.INVITE, to)
	req.SetDestination(conf.address + ":5060")
	req.SetBody(offer)
	req.AppendHeader(&sip.ToHeader{Address: *to})
	req.AppendHeader(&sip.FromHeader{Address: *from})
	req.AppendHeader(&sip.ContactHeader{Address: *from})
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE"))

	if authHeader != "" {
		req.AppendHeader(sip.NewHeader("Proxy-Authorization", authHeader))
	}

	tx, err := c.c.sipCli.TransactionRequest(req)
	if err != nil {
		c.mon.InviteError("tx-failed")
		return nil, nil, err
	}
	defer tx.Terminate()

	resp, err := sipResponse(tx)
	if err != nil {
		c.mon.InviteError("tx-failed")
	}
	return req, resp, err
}

func (c *outboundCall) sipInvite(offer []byte, conf sipOutboundConfig) (*sip.Request, *sip.Response, error) {
	authHeader := ""
	for {
		req, resp, err := c.sipAttemptInvite(offer, conf, authHeader)
		if err != nil {
			return nil, nil, err
		}
		switch resp.StatusCode {
		default:
			c.mon.InviteError(fmt.Sprintf("status-%d", resp.StatusCode))
			return nil, nil, fmt.Errorf("Unexpected StatusCode from INVITE response %d", resp.StatusCode)
		case 400:
			c.mon.InviteError("status-400")
			var reason string
			if body := resp.Body(); len(body) != 0 {
				reason = string(body)
			} else if s := resp.GetHeader("X-Twillio-Error"); s != nil {
				reason = s.Value()
			}
			if reason != "" {
				return nil, nil, fmt.Errorf("INVITE failed: %s", reason)
			}
			return nil, nil, fmt.Errorf("INVITE failed with status %d", resp.StatusCode)
		case 200:
			c.mon.InviteAccept()
			return req, resp, nil
		case 407:
			// auth required
			c.mon.InviteError("auth-required")
		}
		if conf.user == "" || conf.pass == "" {
			return nil, nil, fmt.Errorf("Server responded with 407, but no username or password was provided")
		}
		headerVal := resp.GetHeader("Proxy-Authenticate")
		challenge, err := digest.ParseChallenge(headerVal.Value())
		if err != nil {
			return nil, nil, err
		}

		toHeader, ok := resp.To()
		if !ok {
			return nil, nil, fmt.Errorf("No To Header on Request")
		}

		cred, err := digest.Digest(challenge, digest.Options{
			Method:   req.Method.String(),
			URI:      toHeader.Address.String(),
			Username: conf.user,
			Password: conf.pass,
		})
		if err != nil {
			return nil, nil, err
		}
		authHeader = cred.String()
		// Try again with a computed digest
	}
}

func (c *outboundCall) sipAccept(inviteReq *sip.Request, inviteResp *sip.Response) error {
	if cont, ok := inviteResp.Contact(); ok {
		inviteReq.Recipient = &cont.Address
		inviteReq.Recipient.Port = 5060
	}
	return c.c.sipCli.WriteRequest(sip.NewAckRequest(inviteReq, inviteResp, nil))
}

func (c *outboundCall) sipBye() error {
	req := sip.NewByeRequest(c.sipInviteReq, c.sipInviteResp, nil)
	c.sipInviteReq.AppendHeader(sip.NewHeader("User-Agent", "LiveKit"))

	tx, err := c.c.sipCli.TransactionRequest(req)
	if err != nil {
		return err
	}
	_, err = sipResponse(tx)
	return err
}
