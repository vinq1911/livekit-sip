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

package mixer

import (
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"github.com/livekit/protocol/logger"

	"github.com/vinq1911/livekit-sip/pkg/internal/ringbuf"
	"github.com/vinq1911/livekit-sip/pkg/media"
)

type VideoInput struct {
	mu           sync.Mutex
	hasFrame     bool
	frameBuffer  ringbuf.Buffer[media.H264Sample]
	currentFrame *media.H264Sample
}

type VideoMixer struct {
	out      media.Writer[media.H264Sample]
	hasFrame bool
	mu       sync.Mutex
	inputs   []*VideoInput

	tickerDur time.Duration
	ticker    *time.Ticker
	mixTmp    media.H264Sample // temp buffer for reading input buffers

	lastMix time.Time
	stopped core.Fuse
	mixCnt  uint
}

func NewVideoMixer(out media.Writer[media.H264Sample], bufferDur time.Duration, sampleRate int) *VideoMixer {
	mixSize := int(time.Duration(sampleRate) * bufferDur / time.Second)
	m := newVideoMixer(out, mixSize)
	m.tickerDur = bufferDur
	m.ticker = time.NewTicker(bufferDur)

	go m.start()

	return m
}

func newVideoMixer(out media.Writer[media.H264Sample], mixSize int) *VideoMixer {
	return &VideoMixer{
		out:    out,
		mixTmp: media.H264Sample{},
	}
}

func (m *VideoMixer) mixInputs() {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Keep at least half of the samples buffered.

	m.inputs[0].readSample(m.mixTmp)
}

func (m *VideoMixer) reset() {
	logger.Debugw("Resetting mixer")
}

func (m *VideoMixer) mixOnce() {
	m.mixCnt++
	m.reset()
	m.mixInputs()

	// TODO: if we can guarantee that WriteSample won't store the sample, we can avoid allocation
	if m.hasFrame {
		_ = m.out.WriteSample(m.mixTmp)
	}
}

func (m *VideoMixer) mixUpdate() {
	n := 1
	if !m.lastMix.IsZero() {
		// In case scheduler stops us for too long, we will detect it and run mix multiple times.
		// This happens if we get scheduled by OS/K8S on a lot of CPUs, but for a very short time.
		if dt := time.Since(m.lastMix); dt > 0 {
			n = int(dt / m.tickerDur)
		}
	}
	if n == 0 {
		n = 1
	} else if n > inputBufferFrames {
		n = inputBufferFrames
	}
	for i := 0; i < n; i++ {
		m.mixOnce()
	}
	m.lastMix = time.Now()
}

func (m *VideoMixer) start() {
	defer m.ticker.Stop()
	for {
		select {
		case <-m.ticker.C:
			m.mixUpdate()
		case <-m.stopped.Watch():
			return
		}
	}
}

func (m *VideoMixer) Stop() {
	m.stopped.Break()
}

func (m *VideoMixer) NewInput() *VideoInput {
	m.mu.Lock()
	defer m.mu.Unlock()

	inp := &VideoInput{
		hasFrame: false,
	}
	m.inputs = append(m.inputs, inp)
	return inp
}

func (m *VideoMixer) RemoveInput(inp *VideoInput) {
	if m == nil || inp == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, cur := range m.inputs {
		if cur == inp {
			m.inputs = append(m.inputs[:i], m.inputs[i+1:]...)
			break
		}
	}
}

func (i *VideoInput) readSample(out media.H264Sample) bool {
	i.mu.Lock()
	defer i.mu.Unlock()

	out = i.frameBuffer.Pop()
	if out == nil {
		i.hasFrame = false // starving; pause the input and start buffering again
		return false
	}
	return true
}

func (i *VideoInput) WriteSample(sample media.H264Sample) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.frameBuffer.Push(sample)
	return nil
}
