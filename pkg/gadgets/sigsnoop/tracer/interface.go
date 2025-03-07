// Copyright 2022 The Inspektor Gadget authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tracer

type Tracer interface {
	Stop()
}

type Config struct {
	// TODO: Make it a *ebpf.Map once
	// https://github.com/cilium/ebpf/issues/515 and
	// https://github.com/cilium/ebpf/issues/517 are fixed
	MountnsMap string

	TargetSignal string
	TargetPid    int32
	FailedOnly   bool
}
