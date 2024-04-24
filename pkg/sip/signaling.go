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
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/livekit/protocol/logger"
	"github.com/pion/sdp/v2"
	"github.com/vinq1911/livekit-sip/pkg/media"
	"github.com/vinq1911/livekit-sip/pkg/media/dtmf"
	"github.com/vinq1911/livekit-sip/pkg/media/rtp"
	lksdp "github.com/vinq1911/livekit-sip/pkg/media/sdp"
)

func getCodecs() []sdpCodecInfo {
	const dynamicType = 101
	codecs := media.EnabledCodecs()
	slices.SortFunc(codecs, func(a, b media.Codec) int {
		ai, bi := a.Info(), b.Info()
		if ai.RTPIsStatic && bi.RTPIsStatic {
			return int(ai.RTPDefType) - int(bi.RTPDefType)
		}
		if ai.RTPIsStatic {
			return -1
		} else if bi.RTPIsStatic {
			return 1
		}
		return bi.Priority - ai.Priority
	})
	infos := make([]sdpCodecInfo, 0, len(codecs))
	nextType := byte(dynamicType)
	for _, c := range codecs {
		cinfo := c.Info()
		info := sdpCodecInfo{
			Codec: c,
		}
		if cinfo.RTPIsStatic {
			info.Type = cinfo.RTPDefType
		} else {
			typ := nextType
			nextType++
			info.Type = typ
		}
		infos = append(infos, info)
	}
	return infos
}

type sdpCodecInfo struct {
	Type  byte
	Codec media.Codec
}

func sdpMediaOffer(audioListenerPort int, videoListenerPort int) []*sdp.MediaDescription {
	// Static compiler check for sample rate hardcoded below.
	var _ = [1]struct{}{}[8000-rtp.DefSampleRate]

	codecs := getCodecs()
	attrs := make([]sdp.Attribute, 0, len(codecs)+4)
	formats := make([]string, 0, len(codecs))
	dtmfType := -1
	for _, codec := range codecs {
		if codec.Codec.Info().SDPName == dtmf.SDPName {
			dtmfType = int(codec.Type)
		}
		styp := strconv.Itoa(int(codec.Type))
		formats = append(formats, styp)
		attrs = append(attrs, sdp.Attribute{
			Key:   "rtpmap",
			Value: styp + " " + codec.Codec.Info().SDPName,
		})
	}
	if dtmfType > 0 {
		attrs = append(attrs, sdp.Attribute{
			Key: "fmtp", Value: fmt.Sprintf("%d 0-16", dtmfType),
		})
	}
	attrs = append(attrs, []sdp.Attribute{
		{Key: "ptime", Value: "20"},
		{Key: "maxptime", Value: "150"},
		{Key: "sendrecv"},
	}...)

	return []*sdp.MediaDescription{
		{
			MediaName: sdp.MediaName{
				Media:   "audio",
				Port:    sdp.RangedPort{Value: audioListenerPort},
				Protos:  []string{"RTP", "AVP"},
				Formats: formats,
			},
			Attributes: attrs,
		}, {
			MediaName: sdp.MediaName{
				Media:   "video",
				Port:    sdp.RangedPort{Value: videoListenerPort},
				Protos:  []string{"RTP", "AVP"},
				Formats: []string{"102", "97", "125"},
			},
			Attributes: []sdp.Attribute{
				{Key: "rtpmap", Value: "102 H264/90000"},
				{Key: "fmtp", Value: "102 profile-level-id=42001f"},
				{Key: "rtpmap", Value: "97 H264/90000"},
				{Key: "fmtp", Value: "97 profile-level-id=42801F"},
				/*{Key: "rtpmap", Value: "104 H264/90000"},
				{Key: "fmtp", Value: "104 level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42001f"},
				{Key: "rtpmap", Value: "106 H264/90000"},
				{Key: "fmtp", Value: "106 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
				{Key: "rtpmap", Value: "108 H264/90000"},
				{Key: "fmtp", Value: "108 level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f"},
				{Key: "rtpmap", Value: "112 H264/90000"},
				{Key: "fmtp", Value: "112 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f"},*/
				{Key: "rtpmap", Value: "125 H264/90000"},
				{Key: "fmtp", Value: "125 profile-level-id=42801E;packetization-mode=0"},
				/*{Key: "rtpmap", Value: "127 H264/90000"},
				{Key: "fmtp", Value: "127 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f"},*/
			},
		},
	}
}

func sdpAnswerMediaDesc(audioListenerPort int, videoListenerPort int, res *sdpCodecResult) []*sdp.MediaDescription {
	// Static compiler check for sample rate hardcoded below.
	var _ = [1]struct{}{}[8000-rtp.DefSampleRate]

	attrs := make([]sdp.Attribute, 0, 6)
	attrs = append(attrs, sdp.Attribute{
		Key: "rtpmap", Value: fmt.Sprintf("%d %s", res.AudioType, res.Audio.Info().SDPName),
	})
	if res.DTMFType != 0 {
		attrs = append(attrs, []sdp.Attribute{
			{Key: "rtpmap", Value: fmt.Sprintf("%d %s", res.DTMFType, dtmf.SDPName)},
			{Key: "fmtp", Value: fmt.Sprintf("%d 0-16", res.DTMFType)},
		}...)
	}
	attrs = append(attrs, []sdp.Attribute{
		{Key: "ptime", Value: "20"},
		{Key: "maxptime", Value: "150"},
		{Key: "sendrecv"},
	}...)
	return []*sdp.MediaDescription{
		{
			MediaName: sdp.MediaName{
				Media:   "audio",
				Port:    sdp.RangedPort{Value: audioListenerPort},
				Protos:  []string{"RTP", "AVP"},
				Formats: []string{"0", "101"},
			},
			Attributes: attrs,
		}, {
			MediaName: sdp.MediaName{
				Media:   "video",
				Port:    sdp.RangedPort{Value: videoListenerPort},
				Protos:  []string{"RTP", "AVP"},
				Formats: []string{"102", "97", "125"},
			},
			Attributes: []sdp.Attribute{
				{Key: "rtpmap", Value: "102 H264/90000"},
				{Key: "fmtp", Value: "102 profile-level-id=42001f"},
				{Key: "rtpmap", Value: "97 H264/90000"},
				{Key: "fmtp", Value: "97 profile-level-id=42801F"},
				/*{Key: "rtpmap", Value: "104 H264/90000"},
				{Key: "fmtp", Value: "104 level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42001f"},
				{Key: "rtpmap", Value: "106 H264/90000"},
				{Key: "fmtp", Value: "106 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
				{Key: "rtpmap", Value: "108 H264/90000"},
				{Key: "fmtp", Value: "108 level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f"},
				{Key: "rtpmap", Value: "112 H264/90000"},
				{Key: "fmtp", Value: "112 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f"},*/
				{Key: "rtpmap", Value: "125 H264/90000"},
				{Key: "fmtp", Value: "125 profile-level-id=42801E;packetization-mode=0"},
				/*{Key: "rtpmap", Value: "127 H264/90000"},
				{Key: "fmtp", Value: "127 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f"},*/
			},
		},
	}
}

func sdpGenerateOffer(publicIp string, audioRtpListenerPort int, videoRtpListenerPort int) ([]byte, error) {
	sessId := rand.Uint64() // TODO: do we need to track these?

	mediaDesc := sdpMediaOffer(audioRtpListenerPort, videoRtpListenerPort)
	answer := sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      sessId,
			SessionVersion: sessId,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: publicIp,
		},
		SessionName: "LiveKit",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: publicIp},
		},
		TimeDescriptions: []sdp.TimeDescription{
			{
				Timing: sdp.Timing{
					StartTime: 0,
					StopTime:  0,
				},
			},
		},
		MediaDescriptions: mediaDesc,
	}

	data, err := answer.Marshal()
	return data, err
}

func sdpGenerateAnswer(offer sdp.SessionDescription, publicIp string, audioListenerPort int, videoListenerPort int, res *sdpCodecResult) ([]byte, error) {

	logger.Debugw("Generating SDP answer")
	answer := sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      offer.Origin.SessionID,
			SessionVersion: offer.Origin.SessionID + 2,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: publicIp,
		},
		SessionName: "LiveKit",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: publicIp},
		},
		TimeDescriptions: []sdp.TimeDescription{
			{
				Timing: sdp.Timing{
					StartTime: 0,
					StopTime:  0,
				},
			},
		},
		MediaDescriptions: sdpAnswerMediaDesc(audioListenerPort, videoListenerPort, res),
	}
	logger.Debugw("Formulated answer:", "answer", answer)
	return answer.Marshal()
}

func sdpGetAudioVideo(offer sdp.SessionDescription) (*sdp.MediaDescription, *sdp.MediaDescription) {
	var audio, video *sdp.MediaDescription

	for _, m := range offer.MediaDescriptions {
		if m.MediaName.Media == "audio" {
			audio = m
		}
		if m.MediaName.Media == "video" {
			video = m
		}
	}

	/// hacky
	if video == nil {
		video = &sdp.MediaDescription{Attributes: []sdp.Attribute{}}
	}

	return audio, video
}

func sdpGetAudioDest(offer sdp.SessionDescription) *net.UDPAddr {
	ci := offer.ConnectionInformation

	if ci.NetworkType != "IN" {
		return nil
	}

	ip, err := netip.ParseAddr(ci.Address.Address)
	if err != nil {
		return nil
	}

	audio, _ := sdpGetAudioVideo(offer)

	if audio == nil {
		return nil
	}

	return &net.UDPAddr{
		IP:   ip.AsSlice(),
		Port: audio.MediaName.Port.Value,
	}
}

type sdpCodecResult struct {
	Video     rtp.VideoCodec
	VideoType byte
	Audio     rtp.AudioCodec
	AudioType byte
	DTMFType  byte
}

func sdpGetCodecAndType(offer sdp.SessionDescription) (*sdpCodecResult, error) {

	logger.Debugw("Getting audio and video from offer", "offer", offer)

	audio, video := sdpGetAudioVideo(offer)
	if audio == nil {
		return nil, errors.New("no audio in sdp")
	}

	logger.Debugw("Getting codec from audio", "audio", audio)
	attrs, err := sdpGetCodec(audio.Attributes, video.Attributes)

	logger.Debugw("Returning codec and type", "attributes", attrs, "error", err)
	return attrs, err
}

func sdpGetCodec(audioAttrs []sdp.Attribute, videoAttrs []sdp.Attribute) (*sdpCodecResult, error) {

	var (
		priority   int
		audioCodec rtp.AudioCodec
		videoCodec rtp.VideoCodec
		videoType  byte
		audioType  byte
		dtmfType   byte
	)

	for _, m := range audioAttrs {
		switch m.Key {
		case "rtpmap":
			sub := strings.SplitN(m.Value, " ", 2)
			if len(sub) != 2 {
				continue
			}
			typ, err := strconv.Atoi(sub[0])
			if err != nil {
				continue
			}
			name := sub[1]
			if name == dtmf.SDPName {
				dtmfType = byte(typ)
				continue
			}
			logger.Debugw("Testing audio codec", "name", name, "value", m.Value)
			codec, ok := lksdp.CodecByName(name).(rtp.AudioCodec)
			if !ok {
				continue
			}
			if audioCodec == nil || codec.Info().Priority > priority {
				audioType = byte(typ)
				audioCodec = codec
				priority = codec.Info().Priority
			}
		}
	}

	for _, m := range videoAttrs {
		switch m.Key {
		case "rtpmap":
			sub := strings.SplitN(m.Value, " ", 2)
			if len(sub) != 2 {
				continue
			}
			typ, err := strconv.Atoi(sub[0])
			if err != nil {
				continue
			}
			name := sub[1]
			codec, ok := lksdp.CodecByName(name).(rtp.VideoCodec)
			if !ok {
				continue
			}
			if videoCodec == nil || codec.Info().Priority > priority {
				videoType = byte(typ)
				videoCodec = codec
				priority = codec.Info().Priority
			}
		}
	}

	if audioCodec == nil {
		logger.Debugw("Audio codec NOT found")
		return nil, errors.New("common audio codec not found")
	} else {
		logger.Debugw("Audio codec found", "codec", audioCodec.Info().SDPName, "type", string(audioType))
	}

	if videoCodec == nil {
		logger.Debugw("Video codec NOT found")
	} else {
		logger.Debugw("Video codec found", "codec", videoCodec.Info().SDPName, "type", string(videoType))
	}

	return &sdpCodecResult{
		Video:     videoCodec,
		VideoType: videoType,
		Audio:     audioCodec,
		AudioType: audioType,
		DTMFType:  dtmfType,
	}, nil
}
