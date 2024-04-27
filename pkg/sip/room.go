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
	"sync/atomic"

	"github.com/vinq1911/livekit-sip/pkg/internal/ringbuf"
	"github.com/vinq1911/livekit-sip/pkg/media/h264"

	"github.com/frostbyte73/core"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/pkg/samplebuilder"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/h264writer"
	"github.com/vinq1911/livekit-sip/pkg/config"
	"github.com/vinq1911/livekit-sip/pkg/media"
	"github.com/vinq1911/livekit-sip/pkg/media/opus"
	"github.com/vinq1911/livekit-sip/pkg/media/rtp"
	"github.com/vinq1911/livekit-sip/pkg/mixer"
)

type Participant struct {
	ID       string
	RoomName string
	Identity string
}

type Room struct {
	room     *lksdk.Room
	mix      *mixer.Mixer
	audioOut media.SwitchWriter[media.PCM16Sample]
	videoOut media.SwitchWriter[media.H264Sample]
	identity string
	p        Participant
	ready    atomic.Bool
	stopped  core.Fuse
}

type lkRoomConfig struct {
	roomName string
	identity string
	wsUrl    string
	token    string
}

func NewRoom() *Room {
	r := &Room{}
	r.mix = mixer.NewMixer(&r.audioOut, rtp.DefFrameDur, rtp.DefSampleRate)
	return r
}

func (r *Room) Closed() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.stopped.Watch()
}

func (r *Room) Connect(conf *config.Config, roomName, identity, wsUrl, token string) error {
	var (
		err  error
		room *lksdk.Room
	)
	r.p = Participant{RoomName: roomName, Identity: identity}
	roomCallback := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackPublished: func(publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {

				if publication.Kind() == lksdk.TrackKindAudio {
					logger.Debugw("Participant Audio track published")
					if err := publication.SetSubscribed(true); err != nil {
						logger.Errorw("cannot subscribe to the track", err, "trackID", publication.SID())
					}
				}

				if publication.Kind() == lksdk.TrackKindVideo {
					logger.Debugw("Participant Video track published", "trackID", publication.SID(), "participant", rp.Identity(), "mimetype", publication.MimeType())
					if err := publication.SetSubscribed(true); err != nil {
						logger.Errorw("cannot subscribe to the track", err, "trackID", publication.SID())
					}
				}

			},
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				logger.Debugw("Participant OnTrackSubscribed", "trackID", pub.SID(), "participant", rp.Identity(), "kind", pub.Kind().String(), "mimetype", pub.MimeType())

				mTrack := r.NewTrack()

				defer mTrack.Close()

				if track.Kind() == webrtc.RTPCodecTypeAudio {

					odec, err := opus.Decode(mTrack, rtp.DefSampleRate, channels)
					if err != nil {
						logger.Debugw("Error in OPUS decode", "error", err)
						return
					}

					logger.Debugw("Setting RTP NewMediaStreamIn")
					h := rtp.NewMediaStreamIn[opus.Sample](odec)
					logger.Debugw("Setting HandleLoop for remote track")
					_ = rtp.HandleLoop(track, h)
				}

				if track.Kind() == webrtc.RTPCodecTypeVideo {

					sb := samplebuilder.New(1000, &codecs.H264Packet{}, track.Codec().ClockRate, samplebuilder.WithPacketDroppedHandler(func() {
						rp.WritePLI(track.SSRC())
					}))

					buffer := ringbuf.New[byte](1000)
					w := h264writer.NewWith(buffer)

					logger.Debugw("Running video track loop")
					go VideoTrackLoop(sb, w, track)

				}

			},
		},
		OnDisconnected: func() {
			r.stopped.Break()
		},
	}

	if wsUrl == "" || token == "" {
		logger.Debugw("Connecting to room without wsUrl and token")
		room, err = lksdk.ConnectToRoom(conf.WsUrl,
			lksdk.ConnectInfo{
				APIKey:              conf.ApiKey,
				APISecret:           conf.ApiSecret,
				RoomName:            roomName,
				ParticipantIdentity: identity,
				ParticipantKind:     lksdk.ParticipantSIP,
			}, roomCallback, lksdk.WithAutoSubscribe(false))
	} else {
		logger.Debugw("Connecting to room with wsUrl and token", "wsurl", wsUrl, "token", token)
		room, err = lksdk.ConnectToRoomWithToken(wsUrl, token, roomCallback)
	}

	if err != nil {
		logger.Debugw("Error in Room Connect", "error", err)
		return err
	}
	r.room = room
	r.p.ID = r.room.LocalParticipant.SID()
	r.p.Identity = r.room.LocalParticipant.Identity()
	r.ready.Store(true)
	logger.Debugw("Room connection established", "room", room.Name(), "id", r.p.ID, "identity", r.p.Identity)
	return nil
}

// / might be obsolete
func ConnectToRoom(conf *config.Config, roomName string, identity string) (*Room, error) {
	r := NewRoom()
	if err := r.Connect(conf, roomName, identity, "", ""); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Room) AudioOutput() media.Writer[media.PCM16Sample] {
	return r.audioOut.Get()
}

func (r *Room) VideoOutput() media.Writer[media.H264Sample] {
	return r.videoOut.Get()
}

func (r *Room) SetAudioOutput(out media.Writer[media.PCM16Sample]) {
	if r == nil {
		return
	}
	r.audioOut.Set(out)
}

func (r *Room) SetVideoOutput(out media.Writer[media.H264Sample]) {
	if r == nil {
		return
	}
	r.videoOut.Set(out)
}

func VideoTrackLoop(sb *samplebuilder.SampleBuilder, mw *h264writer.H264Writer, track *webrtc.TrackRemote) {
	defer mw.Close()

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			logger.Debugw("Error in RTP processing", "error", err)
			break
		}
		sb.Push(pkt)

		for _, p := range sb.PopPackets() {
			mw.WriteRTP(p)
		}
	}
}

func (r *Room) Close() error {
	r.ready.Store(false)
	if r.room != nil {
		r.room.Disconnect()
		r.room = nil
	}
	if r.mix != nil {
		r.mix.Stop()
		r.mix = nil
	}
	return nil
}

func (r *Room) Participant() Participant {
	if r == nil {
		return Participant{}
	}
	return r.p
}

func (r *Room) NewParticipantTrack() (media.Writer[media.PCM16Sample], media.Writer[media.H264Sample], error) {
	audioTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, r.identity+"-audio", r.identity+"-audio-pion")
	if err != nil {
		return nil, nil, err
	}

	videoTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, r.identity+"-video", r.identity+"-video-pion")
	if err != nil {
		return nil, nil, err
	}

	p := r.room.LocalParticipant
	if _, err = p.PublishTrack(audioTrack, &lksdk.TrackPublicationOptions{
		Name: r.identity + "-audio",
	}); err != nil {
		return nil, nil, err
	}

	if _, err = p.PublishTrack(videoTrack, &lksdk.TrackPublicationOptions{
		Name:        r.identity + "-video",
		VideoWidth:  1280,
		VideoHeight: 720,
	}); err != nil {
		return nil, nil, err
	}

	ow := media.FromSampleWriter[opus.Sample](audioTrack, rtp.DefFrameDur)
	pw, err := opus.Encode(ow, rtp.DefSampleRate, channels)

	if err != nil {
		logger.Debugw("Error in OPUS encode", "error", err)
		return nil, nil, err
	}

	vw := h264.BuildSampleWriter[media.H264Sample](videoTrack, rtp.DefFrameDur)
	return pw, vw, nil
}

func (r *Room) SendData(data lksdk.DataPacket, opts ...lksdk.DataPublishOption) error {
	if r == nil || !r.ready.Load() {
		return nil
	}
	return r.room.LocalParticipant.PublishDataPacket(data, opts...)
}

func (r *Room) NewTrack() *Track {
	inp := r.mix.NewInput()
	return &Track{mix: r.mix, inp: inp}
}

type Track struct {
	mix *mixer.Mixer
	inp *mixer.Input
}

func (t *Track) Close() error {
	t.mix.RemoveInput(t.inp)
	return nil
}

func (t *Track) PlayAudio(ctx context.Context, frames []media.PCM16Sample) {
	_ = media.PlayAudio[media.PCM16Sample](ctx, t, rtp.DefFrameDur, frames)
}

func (t *Track) WriteSample(pcm media.PCM16Sample) error {
	return t.inp.WriteSample(pcm)
}
