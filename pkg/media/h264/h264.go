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
	"time"

	"github.com/gotranspile/g722"
	"github.com/pion/mediadevices/pkg/codec/openh264"
	"github.com/pion/mediadevices/pkg/io/video"
	"github.com/pion/webrtc/v3/pkg/media"

	m "github.com/vinq1911/livekit-sip/pkg/media"
	"github.com/vinq1911/livekit-sip/pkg/media/rtp"
)

type Sample []byte

const SDPName = "H264/90000"

type H264Writer = m.Writer[Sample]

func init() {

	h264params, _ := openh264.NewParams()
	reader := video.Reader{
		Read: func() (video.Frame, error) {
			return video.Frame{}, nil
		},
	}
	h264encoder, _ := h264params.BuildVideoEncoder()

	m.RegisterCodec(rtp.NewVideoCodec(m.CodecInfo{
		SDPName:     SDPName,
		RTPDefType:  112,
		RTPIsStatic: true,
		Priority:    1,
		Disabled:    true,
	}, Decode, Encode))
}

type Encoder struct {
	w   H264Writer
	buf Sample
}

type Decoder struct {
	w H264Writer
}

func (e *Encoder) WriteSample(in Sample) error {
	return e.w.WriteSample(in)
}

func (d *Decoder) WriteSample(in Sample) error {
	// TODO: reuse buffer
	out := in.Decode()
	return d.w.WriteSample(out)
}

func Encode(w m.Writer[Sample]) m.Writer[Sample] {
	return &Encoder{w: w}
}

func Decode(w H264Writer) m.Writer[Sample] {
	return &Decoder{w: w}
}

func (s Sample) Decode() Sample {

	return g722.Decode(s, g722.Rate64000, g722.FlagSampleRate8000)
}

func (s *Sample) Encode(data Sample) {

	h264encoder := openh264.Encode()
	*s = g722.Encode(data, g722.Rate64000, g722.FlagSampleRate8000)
}

type SampleWriter interface {
	WriteSample(sample media.Sample) error
}

func BuildSampleWriter[T ~[]byte](w SampleWriter, sampleDur time.Duration) m.Writer[T] {
	return m.WriterFunc[T](func(in T) error {
		data := make([]byte, len(in))
		copy(data, in)
		return w.WriteSample(media.Sample{Data: data})
	})
}
