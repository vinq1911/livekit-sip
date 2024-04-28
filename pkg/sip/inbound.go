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
	"sync/atomic"
	"time"

	"github.com/vinq1911/livekit-sip/pkg/media/h264"
	"github.com/vinq1911/livekit-sip/pkg/media/ulaw"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/sdp/v2"

	"github.com/vinq1911/livekit-sip/pkg/config"
	"github.com/vinq1911/livekit-sip/pkg/media"
	"github.com/vinq1911/livekit-sip/pkg/media/dtmf"
	"github.com/vinq1911/livekit-sip/pkg/media/rtp"
	"github.com/vinq1911/livekit-sip/pkg/stats"
)

const (
	// inboundHidePort controls how SIP endpoint responds to unverified inbound requests.
	// Setting it to true makes SIP server silently drop INVITE requests if it gets a negative Auth or Dispatch response.
	// Doing so hides our SIP endpoint from (a low effort) port scanners.
	inboundHidePort = true
	// audioBridgeMaxDelay delays sending audio for certain time, unless RTP packet is received.
	// This is done because of audio cutoff at the beginning of calls observed in the wild.
	audioBridgeMaxDelay = 1 * time.Second
)

func sipErrorOrDrop(tx sip.ServerTransaction, req *sip.Request) {
	if inboundHidePort {
		tx.Terminate()
	} else {
		sipErrorResponse(tx, req)
	}
}

func (s *Server) handleInviteAuth(req *sip.Request, tx sip.ServerTransaction, from, username, password string) (ok bool) {
	if username == "" || password == "" {
		return true
	}
	if inboundHidePort {
		// We will send password request anyway, so might as well signal that the progress is made.
		_ = tx.Respond(sip.NewResponseFromRequest(req, 180, "Ringing", nil))
	}

	var inviteState *inProgressInvite
	for i := range s.inProgressInvites {
		if s.inProgressInvites[i].from == from {
			inviteState = s.inProgressInvites[i]
		}
	}

	if inviteState == nil {
		if len(s.inProgressInvites) >= digestLimit {
			s.inProgressInvites = s.inProgressInvites[1:]
		}

		inviteState = &inProgressInvite{from: from}
		s.inProgressInvites = append(s.inProgressInvites, inviteState)
	}

	h := req.GetHeader("Proxy-Authorization")
	if h == nil {
		logger.Infow("Requesting inbound auth", "from", from)
		inviteState.challenge = digest.Challenge{
			Realm:     UserAgent,
			Nonce:     fmt.Sprintf("%d", time.Now().UnixMicro()),
			Algorithm: "MD5",
		}

		res := sip.NewResponseFromRequest(req, 407, "Unauthorized", nil)
		res.AppendHeader(sip.NewHeader("Proxy-Authenticate", inviteState.challenge.String()))
		_ = tx.Respond(res)
		return false
	}

	cred, err := digest.ParseCredentials(h.Value())
	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 401, "Bad credentials", nil))
		return false
	}

	digCred, err := digest.Digest(&inviteState.challenge, digest.Options{
		Method:   req.Method.String(),
		URI:      cred.URI,
		Username: cred.Username,
		Password: password,
	})

	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 401, "Bad credentials", nil))
		return false
	}

	if cred.Response != digCred.Response {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 401, "Unauthorized", nil))
		return false
	}

	return true
}

func (s *Server) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	ctx := context.Background()
	s.mon.InviteReqRaw(stats.Inbound)

	if !inboundHidePort {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 180, "Ringing", nil))
	}
	tag, err := getTagValue(req)
	if err != nil {
		sipErrorOrDrop(tx, req)
		return
	}

	from, ok := req.From()
	if !ok {
		sipErrorOrDrop(tx, req)
		return
	}

	to, ok := req.To()
	if !ok {
		sipErrorOrDrop(tx, req)
		return
	}
	src := req.Source()

	cmon := s.mon.NewCall(stats.Inbound, from.Address.String(), to.Address.String())

	cmon.InviteReq()
	defer cmon.SessionDur()()
	joinDur := cmon.JoinDur()
	logger.Infow("INVITE received", "tag", tag, "from", from, "to", to)

	username, password, drop, err := s.handler.GetAuthCredentials(ctx, from.Address.User, to.Address.User, to.Address.Host, src)
	logger.Debugw("Auth credentials caught", "username", username, "password", password, "drop", drop, "err", err)

	if err != nil {
		cmon.InviteErrorShort("no-rule")
		logger.Warnw("Rejecting inbound, doesn't match any Trunks", err,
			"tag", tag, "src", src, "from", from, "to", to, "to-host", to.Address.Host)
		sipErrorResponse(tx, req)
		return
	} else if drop {
		cmon.InviteErrorShort("flood")
		logger.Debugw("Dropping inbound flood", "src", src, "from", from, "to", to, "to-host", to.Address.Host)
		tx.Terminate()
		return
	}
	if !s.handleInviteAuth(req, tx, from.Address.User, username, password) {
		cmon.InviteErrorShort("unauthorized")
		// handleInviteAuth will generate the SIP Response as needed
		return
	}
	logger.Debugw("Call is good to go", "username", username, "password", password)
	cmon.InviteAccept()

	call := s.newInboundCall(cmon, tag, from, to, src)
	call.joinDur = joinDur
	call.handleInvite(call.ctx, req, tx, s.conf)
}

func (s *Server) onOptions(req *sip.Request, tx sip.ServerTransaction) {
	_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
}

func (s *Server) onBye(req *sip.Request, tx sip.ServerTransaction) {
	tag, err := getTagValue(req)
	if err != nil {
		sipErrorResponse(tx, req)
		return
	}
	logger.Infow("BYE", "tag", tag)

	s.cmu.RLock()
	c := s.activeCalls[tag]
	s.cmu.RUnlock()
	if c != nil {
		c.Close()
	} else if s.sipUnhandled != nil {
		s.sipUnhandled(req, tx)
	}
	_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
}

type inboundCall struct {
	s             *Server
	mon           *stats.CallMonitor
	tag           string
	ctx           context.Context
	cancel        func()
	inviteReq     *sip.Request
	inviteResp    *sip.Response
	from          *sip.FromHeader
	to            *sip.ToHeader
	src           string
	audioRtpConn  *rtp.Conn
	videoRtpConn  *rtp.Conn
	audioCodec    rtp.AudioCodec
	audioHandler  atomic.Pointer[rtp.Handler]
	videoCodec    rtp.VideoCodec
	videoHandler  atomic.Pointer[rtp.Handler]
	audioReceived atomic.Bool
	audioRecvChan chan struct{}
	audioType     byte
	videoType     byte
	dtmf          chan dtmf.Event // buffered
	lkRoom        *Room           // LiveKit room; only active after correct pin is entered
	callDur       func() time.Duration
	joinDur       func() time.Duration
	forwardDTMF   atomic.Bool
	done          atomic.Bool
}

func (s *Server) newInboundCall(mon *stats.CallMonitor, tag string, from *sip.FromHeader, to *sip.ToHeader, src string) *inboundCall {
	c := &inboundCall{
		s:             s,
		mon:           mon,
		tag:           tag,
		from:          from,
		to:            to,
		src:           src,
		audioRecvChan: make(chan struct{}),
		dtmf:          make(chan dtmf.Event, 10),
		lkRoom:        NewRoom(), // we need it created earlier so that the audio mixer is available for pin prompts
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	s.cmu.Lock()
	s.activeCalls[tag] = c
	s.cmu.Unlock()
	return c
}

func (c *inboundCall) handleInvite(ctx context.Context, req *sip.Request, tx sip.ServerTransaction, conf *config.Config) {
	c.mon.CallStart()
	defer c.mon.CallEnd()
	defer c.close("other")
	// Send initial request. In the best case scenario, we will immediately get a room name to join.
	// Otherwise, we could even learn that this number is not allowed and reject the call, or ask for pin if required.
	disp := c.s.handler.DispatchCall(ctx, &CallInfo{
		FromUser:   c.from.Address.User,
		ToUser:     c.to.Address.User,
		ToHost:     c.to.Address.Host,
		SrcAddress: c.src,
		Pin:        "",
		NoPin:      false,
	})

	logger.Debugw("Deciding what to do with the call", "from", c.from.Address.User, "to", c.to.Address.User, "to-host", c.to.Address.Host, "src", c.src, "result", disp.Result)
	switch disp.Result {
	default:
		logger.Errorw("Rejecting inbound call", fmt.Errorf("unexpected dispatch result: %v", disp.Result), "from", c.from.Address.User, "to", c.to.Address.User, "to-host", c.to.Address.Host, "src", c.src)
		sipErrorResponse(tx, req)
		c.close("unexpected-result")
		return
	case DispatchNoRuleDrop:
		logger.Debugw("Rejecting inbound flood", "from", c.from.Address.User, "to", c.to.Address.User, "to-host", c.to.Address.Host, "src", c.src)
		tx.Terminate()
		c.close("flood")
		return
	case DispatchNoRuleReject:
		logger.Infow("Rejecting inbound call, doesn't match any Dispatch Rules", "from", c.from.Address.User, "to", c.to.Address.User, "to-host", c.to.Address.Host, "src", c.src)
		sipErrorResponse(tx, req)
		c.close("no-dispatch")
		return
	case DispatchAccept, DispatchRequestPin:
		// continue
	}

	// We need to start media first, otherwise we won't be able to send audio prompts to the caller, or receive DTMF.
	answerData, err := c.runMediaConn(req.Body(), conf)
	if err != nil {
		logger.Debugw("Media response had error", "error", err)
		sipErrorResponse(tx, req)
		c.close("media-failed")
		return
	}

	res := sip.NewResponseFromRequest(req, 200, "OK", answerData)

	// This will effectively redirect future SIP requests to this server instance (if signalingIp is not LB).
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Host: c.s.signalingIp, Port: c.s.conf.SIPPort}})

	// When behind LB, the source IP may be incorrect and/or the UDP "session" timeout may expire.
	// This is critical for sending new requests like BYE.
	//
	// Thus, instead of relying on LB, we will contact the source IP directly (should be the first Via).
	// BYE will also copy the same destination address from our response to INVITE.
	if h, ok := req.Via(); ok && h.Host != "" {
		port := 5060
		if h.Port != 0 {
			port = h.Port
		}
		res.SetDestination(fmt.Sprintf("%s:%d", h.Host, port))
	}

	res.AppendHeader(&contentTypeHeaderSDP)
	if err = tx.Respond(res); err != nil {
		logger.Errorw("Cannot respond to INVITE", err)
		return
	}
	c.inviteReq = req
	c.inviteResp = res

	// Wait for either a first RTP packet or a predefined delay.
	//
	// If the delay kicks in earlier than the caller is ready, they might miss some audio packets.
	//
	// On the other hand, if we always wait for RTP, it might be harder to diagnose firewall/routing issues.
	// In that case both sides will hear nothing, instead of only one side having issues.
	//
	// Thus, we wait at most a fixed amount of time before bridging audio.

	// We own this goroutine, so can freely block.
	delay := time.NewTimer(audioBridgeMaxDelay)
	select {
	case <-ctx.Done():
		delay.Stop()
		c.close("hangup")
		return
	case <-c.audioRecvChan:
		delay.Stop()
	case <-delay.C:
	}
	switch disp.Result {
	default:
		logger.Errorw("Rejecting inbound call", fmt.Errorf("unreachable dispatch result path: %v", disp.Result), "from", c.from.Address.User, "to", c.to.Address.User, "to-host", c.to.Address.Host, "src", c.src)
		sipErrorResponse(tx, req)
		c.close("unreachable-path")
		return
	case DispatchRequestPin:
		c.pinPrompt(ctx)
	case DispatchAccept:
		c.joinRoom(ctx, disp.RoomName, disp.Identity, disp.WsUrl, disp.Token)
	}
	// Wait for the caller to terminate the call.
	select {
	case <-ctx.Done():
		c.close("hangup")
	case <-c.lkRoom.Closed():
		c.close("removed")
	}
}

func (c *inboundCall) sendBye() {
	if c.inviteReq == nil {
		return
	}
	// This function is for clients, so we need to swap src and dest
	bye := sip.NewByeRequest(c.inviteReq, c.inviteResp, nil)
	if contact, ok := c.inviteReq.Contact(); ok {
		bye.Recipient = &contact.Address
	} else {
		bye.Recipient = &c.from.Address
	}
	bye.SetSource(c.inviteResp.Source())
	bye.SetDestination(c.inviteResp.Destination())
	bye.RemoveHeader("From")
	bye.AppendHeader((*sip.FromHeader)(c.to))
	bye.RemoveHeader("To")
	bye.AppendHeader((*sip.ToHeader)(c.from))
	if route, ok := bye.RecordRoute(); ok {
		bye.RemoveHeader("Record-Route")
		bye.AppendHeader(&sip.RouteHeader{Address: route.Address})
	}
	_ = c.s.sipSrv.TransportLayer().WriteMsg(bye)
	c.inviteReq = nil
	c.inviteResp = nil
}

func (c *inboundCall) runMediaConn(offerData []byte, conf *config.Config) (answerData []byte, _ error) {

	audioConn := rtp.NewConn(func() {
		c.close("audio-timeout")
	})

	videoConn := rtp.NewConn(func() {
		logger.Debugw("Video timeout", "tag", c.tag)
		/// we dont want this, since not everyone have video
		/// c.close("video-timeout")
	})

	offer := sdp.SessionDescription{}
	logger.Debugw("Offer received", "offer", string(offerData))

	if err := offer.Unmarshal(offerData); err != nil {
		logger.Debugw("Offer unmarshal error", err, "offer", offer)
		return nil, err
	}
	res, err := sdpGetCodecAndType(offer)
	logger.Debugw("SDP codec and type", "codec", res.Audio.Info().SDPName, "type", res.AudioType)
	if err != nil {
		return nil, err
	}

	mux := rtp.NewMux(nil)
	mux.SetDefault(newRTPStatsHandler(c.mon, "", nil))
	mux.Register(res.AudioType, newRTPStatsHandler(c.mon, res.Audio.Info().SDPName, rtp.HandlerFunc(c.HandleRTP)))
	if res.DTMFType != 0 {
		mux.Register(res.DTMFType, newRTPStatsHandler(c.mon, dtmf.SDPName, rtp.HandlerFunc(c.handleDTMF)))
	}
	audioConn.OnRTP(mux)
	if dst := sdpGetAudioDest(offer); dst != nil {
		audioConn.SetDestAddr(dst)
	}

	if err := audioConn.ListenAndServe(conf.RTPPort.Start, conf.RTPPort.End, "0.0.0.0"); err != nil {
		return nil, err
	}

	// Encoding pipeline (LK -> SIP)
	// Need to be created earlier to send the pin prompts.

	asw := rtp.NewSeqWriter(c.audioRtpConn)
	ast := asw.NewStream(c.audioType)

	aus := rtp.NewMediaStreamOut[ulaw.Sample](ast)
	c.lkRoom.SetAudioOutput(ulaw.Encode(aus))

	vsw := rtp.NewSeqWriter(c.videoRtpConn)

	if c.videoType != 0 {
		logger.Debugw("Creating video stream", "videoType", c.videoType)
		vst := vsw.NewStream(c.videoType)
		vis := rtp.NewMediaStreamOut[media.H264Sample](vst)
		c.lkRoom.SetVideoOutput(h264.Encode(vis))
	}

	videoConn.OnRTP(c)

	if vdst := sdpGetVideoDest(offer); vdst != nil {
		logger.Debugw("Video destination UDP", "address", vdst)
		videoConn.SetDestAddr(vdst)
	}

	if err := videoConn.ListenAndServe(conf.RTPPort.Start, conf.RTPPort.End, "0.0.0.0"); err != nil {
		return nil, err
	}

	c.audioRtpConn = audioConn
	c.videoRtpConn = videoConn
	c.audioCodec = res.Audio
	c.videoCodec = res.Video
	c.audioType = res.AudioType
	c.videoType = res.VideoType

	logger.Debugw("begin listening on UDP", "audio port", audioConn.LocalAddr().Port, "video port", videoConn.LocalAddr().Port)

	return sdpGenerateAnswer(offer, c.s.signalingIp, audioConn.LocalAddr().Port, videoConn.LocalAddr().Port, res)
}

func (c *inboundCall) pinPrompt(ctx context.Context) {
	logger.Infow("Requesting Pin for SIP call", "tag", c.tag, "from", c.from.Address.User, "to", c.to.Address.User)
	const pinLimit = 16
	c.playAudio(ctx, c.s.res.enterPin)
	pin := ""
	noPin := false
	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-c.dtmf:
			if !ok {
				c.Close()
				return
			}
			if b.Digit == 0 {
				continue // unrecognized
			}
			if b.Digit == '#' {
				// End of the pin
				noPin = pin == ""

				logger.Infow("Checking Pin for SIP call", "tag", c.tag, "from", c.from.Address.User, "to", c.to.Address.User, "pin", pin, "noPin", noPin)
				disp := c.s.handler.DispatchCall(ctx, &CallInfo{
					FromUser:   c.from.Address.User,
					ToUser:     c.to.Address.User,
					ToHost:     c.to.Address.Host,
					SrcAddress: c.src,
					Pin:        pin,
					NoPin:      noPin,
				})
				if disp.Result != DispatchAccept || disp.RoomName == "" {
					logger.Infow("Rejecting call", "tag", c.tag, "from", c.from.Address.User, "to", c.to.Address.User, "pin", pin, "noPin", noPin)
					c.playAudio(ctx, c.s.res.wrongPin)
					c.close("wrong-pin")
					return
				}
				c.playAudio(ctx, c.s.res.roomJoin)
				c.joinRoom(ctx, disp.RoomName, disp.Identity, disp.WsUrl, disp.Token)
				return
			}
			// Gather pin numbers
			pin += string(b.Digit)
			if len(pin) > pinLimit {
				c.playAudio(ctx, c.s.res.wrongPin)
				c.close("wrong-pin")
				return
			}
		}
	}
}

// close should only be called from handleInvite.
func (c *inboundCall) close(reason string) {
	if !c.done.CompareAndSwap(false, true) {
		return
	}
	c.mon.CallTerminate(reason)
	logger.Infow("Closing inbound call", "tag", c.tag, "from", c.from.Address.User, "to", c.to.Address.User, "reason", reason)
	c.sendBye()
	c.closeMedia()
	if c.callDur != nil {
		c.callDur()
	}
	c.s.cmu.Lock()
	delete(c.s.activeCalls, c.tag)
	c.s.cmu.Unlock()
	c.cancel()
}

func (c *inboundCall) Close() error {
	c.cancel()
	return nil
}

func (c *inboundCall) closeMedia() {
	c.audioHandler.Store(nil)
	if p := c.lkRoom; p != nil {
		p.Close()
		c.lkRoom = nil
	}
	if c.audioRtpConn != nil {
		c.audioRtpConn.Close()
		c.audioRtpConn = nil
	}
	if c.videoRtpConn != nil {
		c.videoRtpConn.Close()
		c.videoRtpConn = nil
	}
}

func (c *inboundCall) HandleRTP(p *rtp.Packet) error {
	if c.audioReceived.CompareAndSwap(false, true) {
		close(c.audioRecvChan)
	}
	// TODO: Audio data appears to be coming with PayloadType=0, so maybe enforce it?
	//logger.Infow("------------ pt == ", p.PayloadType)
	if p.PayloadType < 96 {
		if h := c.audioHandler.Load(); h != nil {
			return (*h).HandleRTP(p)
		}
	} else {
		if h := c.videoHandler.Load(); h != nil {
			return (*h).HandleRTP(p)
		}
	}

	return nil
}

func (c *inboundCall) createLiveKitParticipant(ctx context.Context, roomName, participantIdentity, wsUrl, token string) error {
	c.forwardDTMF.Store(true)
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	err := c.lkRoom.Connect(c.s.conf, roomName, participantIdentity, wsUrl, token)
	if err != nil {
		return err
	}
	audioTrack, videoTrack, err := c.lkRoom.NewParticipantTrack()
	if err != nil {
		_ = c.lkRoom.Close()
		return err
	}

	logger.Debugw("Registering Audio Handler")
	var h rtp.Handler = c.audioCodec.DecodeRTP(audioTrack, c.audioType)
	c.audioHandler.Store(&h)

	if c.videoType != byte(0) {
		logger.Debugw("Registering Video Handler", "videotype", c.videoType, "videoCodec", c.videoCodec)

		// var vh rtp.Handler = c.videoCodec.DecodeRTP(videoTrack, c.videoType)

		var vh rtp.Handler = rtpHandlerHack(videoTrack, c.videoType)
		c.videoHandler.Store(&vh)
	}

	return nil
}

func rtpHandlerHack(videoTrack media.Writer[[]byte], videoType byte) rtp.Handler {

	return rtp.HandlerFunc(func(rp *rtp.Packet) error {

		err := videoTrack.WriteSample(rp.Raw)

		return err
	})
}

func (c *inboundCall) joinRoom(ctx context.Context, roomName, identity, wsUrl, token string) {
	if c.joinDur != nil {
		c.joinDur()
	}
	c.callDur = c.mon.CallDur()
	logger.Infow("Bridging SIP call", "tag", c.tag, "from", c.from.Address.User, "to", c.to.Address.User, "roomName", roomName, "identity", identity, "wsUrl", wsUrl, "token", token)
	if err := c.createLiveKitParticipant(ctx, roomName, identity, wsUrl, token); err != nil {
		logger.Errorw("Cannot create LiveKit participant", err, "tag", c.tag, "from", c.from.Address.User, "to", c.to.Address.User, "roomName", roomName, "identity", identity, "wsUrl", wsUrl, "token", token)
		c.close("participant-failed")
	}
}

func (c *inboundCall) playAudio(ctx context.Context, frames []media.PCM16Sample) {
	t := c.lkRoom.NewTrack()
	defer t.Close()
	t.PlayAudio(ctx, frames)
}

func (c *inboundCall) handleDTMF(p *rtp.Packet) error {
	tone, ok := dtmf.DecodeRTP(p)
	if !ok {
		return nil
	}
	if c.forwardDTMF.Load() {
		_ = c.lkRoom.SendData(&livekit.SipDTMF{
			Code:  uint32(tone.Code),
			Digit: string([]byte{tone.Digit}),
		}, lksdk.WithDataPublishReliable(true))
		return nil
	}
	// We should have enough buffer here.
	select {
	case c.dtmf <- tone:
	default:
	}
	return nil
}
