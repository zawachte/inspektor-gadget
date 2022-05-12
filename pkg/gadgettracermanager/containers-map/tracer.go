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

package containersmap

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"

	pb "github.com/kinvolk/inspektor-gadget/pkg/gadgettracermanager/api"
	"github.com/kinvolk/inspektor-gadget/pkg/gadgettracermanager/pubsub"
)

// #include "../common.h"
import "C"

const (
	BPFMapName        = "containers"
	NameMaxLength     = C.NAME_MAX_LENGTH
	NameMaxCharacters = NameMaxLength - 1
)

type Container = C.struct_container

func copyToC(dest *[NameMaxLength]C.char, source string) {
	for i := 0; i < len(source) && i < NameMaxCharacters; i++ {
		dest[i] = C.char(source[i])
	}
}

// ContainersMap creates a global map /sys/fs/bpf/gadget/containers
// exposing container details for each mount namespace.
//
// This makes it possible for gadgets to access that information and
// display it directly from the BPF code. Example of such code:
//
//     struct container *container_entry;
//     container_entry = bpf_map_lookup_elem(&containers, &mntns_id);
//
// External tools such as tracee or bpftrace could also benefit from this just
// by using this "containers" map (other interaction with Inspektor Gadget is
// not necessary for this).
type ContainersMap struct {
	// containersMap is the global map at /sys/fs/bpf/gadget/containers
	// exposing container details for each mount namespace.
	containersMap *ebpf.Map

	pinPath string
}

func NewContainersMap(pinPath string) (*ContainersMap, error) {
	if pinPath != "" {
		if err := os.Mkdir(pinPath, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("failed to create folder for pinning bpf maps: %w", err)
		}
	}

	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(ebpfProg))
	if err != nil {
		return nil, fmt.Errorf("failed to load asset: %w", err)
	}

	opts := ebpf.CollectionOptions{}
	if pinPath != "" {
		spec.Maps["containers"].Pinning = ebpf.PinByName
		opts.Maps.PinPath = pinPath
	}

	coll, err := ebpf.NewCollectionWithOptions(spec, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create BPF collection: %w", err)
	}

	m, ok := coll.Maps[BPFMapName]
	if !ok {
		return nil, fmt.Errorf("failed to find map %s", BPFMapName)
	}
	return &ContainersMap{
		containersMap: m,
		pinPath:       pinPath,
	}, nil
}

func (cm *ContainersMap) addContainerInMap(c *pb.ContainerDefinition) {
	if cm.containersMap == nil || c.Mntns == 0 {
		return
	}
	mntnsC := uint64(c.Mntns)

	val := Container{}

	copyToC(&val.container_id, c.Id)
	copyToC(&val.namespace, c.Namespace)
	copyToC(&val.pod, c.Podname)
	copyToC(&val.container, c.Name)

	cm.containersMap.Put(mntnsC, val)
}

func (cm *ContainersMap) deleteContainerFromMap(c *pb.ContainerDefinition) {
	if cm.containersMap == nil || c.Mntns == 0 {
		return
	}
	cm.containersMap.Delete(uint64(c.Mntns))
}

func (cm *ContainersMap) ContainersMapUpdater() pubsub.FuncNotify {
	return func(event pubsub.PubSubEvent) {
		switch event.Type {
		case pubsub.EventTypeAddContainer:
			// Skip the pause container
			if event.Container.Name == "" {
				return
			}

			cm.addContainerInMap(&event.Container)

		case pubsub.EventTypeRemoveContainer:
			cm.deleteContainerFromMap(&event.Container)
		}
	}
}

func (cm *ContainersMap) ContainersMap() *ebpf.Map {
	return cm.containersMap
}

func (cm *ContainersMap) Close() {
	if cm == nil {
		return
	}
	os.Remove(filepath.Join(cm.pinPath, BPFMapName))
}
