// Copyright 2019-2022 The Inspektor Gadget authors
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

package crio

import (
	"time"

	runtimeclient "github.com/kinvolk/inspektor-gadget/pkg/container-utils/runtime-client"
)

const (
	Name              = "cri-o"
	DefaultSocketPath = "/run/crio/crio.sock"
	DefaultTimeout    = 2 * time.Second
)

type CrioClient struct {
	runtimeclient.CRIClient
}

func NewCrioClient(socketPath string) (runtimeclient.ContainerRuntimeClient, error) {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}

	criClient, err := runtimeclient.NewCRIClient(Name, socketPath, DefaultTimeout)
	if err != nil {
		return nil, err
	}

	return &CrioClient{
		CRIClient: criClient,
	}, nil
}
