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

package h264

import (
	"errors"
	"strings"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/pkg/samplebuilder"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/h264writer"
	"github.com/pion/webrtc/v3/pkg/media/ivfwriter"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
	m "github.com/vinq1911/livekit-sip/pkg/media"
	"github.com/vinq1911/livekit-sip/pkg/media/rtp"
)

const SDPName = "H264/90000"

func init() {
	m.RegisterCodec(rtp.NewVideoCodec(m.CodecInfo{
		SDPName:     SDPName,
		RTPDefType:  97,
		RTPIsStatic: true,
		Priority:    1,
		Disabled:    false,
	}, Decode, Encode))
}

type Encoder struct {
	w   m.H264Writer
	buf m.H264Sample
}

type Decoder struct {
	w m.H264Writer
}

func (e *Encoder) WriteSample(in m.H264Sample) error {
	return e.w.WriteSample(in)
}

func (d *Decoder) WriteSample(in m.H264Sample) error {
	// TODO: reuse buffer

	return d.w.WriteSample(in)
}

func Encode(w m.H264Writer) m.H264Writer {
	return &Encoder{w: w}
}

func Decode(w m.H264Writer) m.H264Writer {
	return &Decoder{w: w}
}

type SampleWriter interface {
	WriteSample(sample m.H264Sample) error
}

type LocalSampleWriter interface {
	WriteSample(sample media.Sample) error
}

func BuildRTCLocalSampleWriter(w LocalSampleWriter) m.Writer[m.H264Sample] {

	return m.WriterFunc[m.H264Sample](func(in m.H264Sample) error {
		//logger.Debugw("Local sample", "sample", in)
		/// zero data?
		return w.WriteSample(media.Sample{Data: in})
	})
}

func BuildSampleWriter[T ~[]byte](w SampleWriter, sampleDur time.Duration) m.Writer[T] {
	return m.WriterFunc[T](func(in T) error {
		data := make([]byte, len(in))
		copy(data, in)
		/// zero data?
		return w.WriteSample(data)
	})
}

const (
	maxVideoLate = 1000 // nearly 2s for fhd video
	maxAudioLate = 200  // 4s for audio
)

type TrackWriter struct {
	sb     *samplebuilder.SampleBuilder
	writer media.Writer
	track  *webrtc.TrackRemote
}

func NewTrackWriter(track *webrtc.TrackRemote, pliWriter lksdk.PLIWriter, stream rtp.MediaStreamIn[m.H264Sample]) (*TrackWriter, error) {
	var (
		sb     *samplebuilder.SampleBuilder
		writer media.Writer
		err    error
	)
	switch {
	case strings.EqualFold(track.Codec().MimeType, "video/vp8"):
		sb = samplebuilder.New(maxVideoLate, &codecs.VP8Packet{}, track.Codec().ClockRate, samplebuilder.WithPacketDroppedHandler(func() {
			pliWriter(track.SSRC())
		}))
		// ivfwriter use frame count as PTS, that might cause video played in a incorrect framerate(fast or slow)
		writer, err = ivfwriter.New(fileName + ".ivf")

	case strings.EqualFold(track.Codec().MimeType, "video/h264"):
		sb = samplebuilder.New(maxVideoLate, &codecs.H264Packet{}, track.Codec().ClockRate, samplebuilder.WithPacketDroppedHandler(func() {
			pliWriter(track.SSRC())
		}))
		writer = h264writer.NewWith(stream)

	case strings.EqualFold(track.Codec().MimeType, "audio/opus"):
		sb = samplebuilder.New(maxAudioLate, &codecs.OpusPacket{}, track.Codec().ClockRate)
		writer, err = oggwriter.New(fileName+".ogg", 48000, track.Codec().Channels)

	default:
		return nil, errors.New("unsupported codec type")
	}

	if err != nil {
		return nil, err
	}

	t := &TrackWriter{
		sb:     sb,
		writer: writer,
		track:  track,
	}
	go t.start()
	return t, nil
}

func (t *TrackWriter) start() {
	defer t.writer.Close()
	for {
		pkt, _, err := t.track.ReadRTP()
		if err != nil {
			break
		}
		t.sb.Push(pkt)

		for _, p := range t.sb.PopPackets() {
			t.writer.WriteRTP(p)
		}
	}
}
