// Copyright 2024 Livekit, Inc.
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

package media

type H264Sample []byte

type H264Writer = Writer[H264Sample]

func (s H264Sample) Decode() []byte {
	/// passthrough for now
	return s
}

func (s *H264Sample) Encode(data []byte) {
	/// passthrough for now
	*s = data
}
