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

package fsslower

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kinvolk/inspektor-gadget/pkg/gadgets"
	"github.com/kinvolk/inspektor-gadget/pkg/gadgets/fsslower/tracer"

	coretracer "github.com/kinvolk/inspektor-gadget/pkg/gadgets/fsslower/tracer/core"
	"github.com/kinvolk/inspektor-gadget/pkg/gadgets/fsslower/types"

	gadgetv1alpha1 "github.com/kinvolk/inspektor-gadget/pkg/apis/gadget/v1alpha1"
)

var validFilesystems = []string{"btrfs", "ext4", "nfs", "xfs"}

type Trace struct {
	resolver gadgets.Resolver

	started bool
	tracer  tracer.Tracer
}

type TraceFactory struct {
	gadgets.BaseFactory
}

func NewFactory() gadgets.TraceFactory {
	return &TraceFactory{
		BaseFactory: gadgets.BaseFactory{DeleteTrace: deleteTrace},
	}
}

func (f *TraceFactory) Description() string {
	t := `fsslower shows open, read, write and fsync operations slower than a threshold

The following parameters are supported:
- type: Which filesystem to trace [%s]
- minlatency: Min latency to trace, in ms. (default %d)`

	return fmt.Sprintf(t, strings.Join(validFilesystems, ", "), types.MinLatencyDefault)
}

func (f *TraceFactory) OutputModesSupported() map[string]struct{} {
	return map[string]struct{}{
		"Stream": {},
	}
}

func deleteTrace(name string, t interface{}) {
	trace := t.(*Trace)
	if trace.tracer != nil {
		trace.tracer.Stop()
	}
}

func (f *TraceFactory) Operations() map[string]gadgets.TraceOperation {
	n := func() interface{} {
		return &Trace{
			resolver: f.Resolver,
		}
	}

	return map[string]gadgets.TraceOperation{
		"start": {
			Doc: "Start fsslower gadget",
			Operation: func(name string, trace *gadgetv1alpha1.Trace) {
				f.LookupOrCreate(name, n).(*Trace).Start(trace)
			},
		},
		"stop": {
			Doc: "Stop fsslower gadget",
			Operation: func(name string, trace *gadgetv1alpha1.Trace) {
				f.LookupOrCreate(name, n).(*Trace).Stop(trace)
			},
		},
	}
}

func (t *Trace) Start(trace *gadgetv1alpha1.Trace) {
	if t.started {
		trace.Status.State = "Started"
		return
	}

	traceName := gadgets.TraceName(trace.ObjectMeta.Namespace, trace.ObjectMeta.Name)

	eventCallback := func(event types.Event) {
		r, err := json.Marshal(event)
		if err != nil {
			fmt.Printf("error marshalling event: %s\n", err)
			return
		}
		t.resolver.PublishEvent(traceName, string(r))
	}

	var err error

	if trace.Spec.Parameters == nil {
		trace.Status.OperationError = "missing parameters"
		return
	}

	params := trace.Spec.Parameters

	filesystem, ok := params["type"]
	if !ok {
		trace.Status.OperationError = "missing filesystem type"
		return
	}

	minLatency := types.MinLatencyDefault

	val, ok := params["minlatency"]
	if ok {
		minLatencyParsed, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			trace.Status.OperationError = fmt.Sprintf("%q is not valid for minlatency", val)
			return
		}
		minLatency = uint(minLatencyParsed)
	}

	config := &tracer.Config{
		MountnsMap: gadgets.TracePinPath(trace.ObjectMeta.Namespace, trace.ObjectMeta.Name),
		Filesystem: filesystem,
		MinLatency: minLatency,
	}
	t.tracer, err = coretracer.NewTracer(config, t.resolver, eventCallback, trace.Spec.Node)
	if err != nil {
		trace.Status.OperationError = fmt.Sprintf("failed to create tracer: %s", err)
		return
	}

	t.started = true

	trace.Status.State = "Started"
}

func (t *Trace) Stop(trace *gadgetv1alpha1.Trace) {
	if !t.started {
		trace.Status.OperationError = "Not started"
		return
	}

	t.tracer.Stop()
	t.tracer = nil
	t.started = false

	trace.Status.State = "Stopped"
}
