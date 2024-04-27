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

	"github.com/pion/webrtc/v3/pkg/media"
	m "github.com/vinq1911/livekit-sip/pkg/media"
	"github.com/vinq1911/livekit-sip/pkg/media/rtp"
)

const SDPName = "H264/90000"

func init() {
	m.RegisterCodec(rtp.NewVideoCodec(m.CodecInfo{
		SDPName:     SDPName,
		RTPDefType:  byte(126),
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
	out := in.Decode()
	return d.w.WriteSample(out)
}

func Encode(w m.Writer[m.H264Sample]) m.Writer[m.H264Sample] {
	return &Encoder{w: w}
}

func Decode(w m.H264Writer) m.Writer[m.H264Sample] {
	return &Decoder{w: w}
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
