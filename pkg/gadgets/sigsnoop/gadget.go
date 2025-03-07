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

package sigsnoop

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/kinvolk/inspektor-gadget/pkg/gadgets"
	"github.com/kinvolk/inspektor-gadget/pkg/gadgets/sigsnoop/tracer"

	coretracer "github.com/kinvolk/inspektor-gadget/pkg/gadgets/sigsnoop/tracer/core"
	"github.com/kinvolk/inspektor-gadget/pkg/gadgets/sigsnoop/types"

	gadgetv1alpha1 "github.com/kinvolk/inspektor-gadget/pkg/apis/gadget/v1alpha1"

	log "github.com/sirupsen/logrus"
)

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
	return `sigsnoop traces all signals sent on the system.

The following parameters are supported:
- failed: Trace only failed signal sending (default to false).
- signal: Which particular signal to trace (default to all).
- pid: Which particular pid to trace (default to all).
`
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
			Doc: "Start sigsnoop gadget",
			Operation: func(name string, trace *gadgetv1alpha1.Trace) {
				f.LookupOrCreate(name, n).(*Trace).Start(trace)
			},
		},
		"stop": {
			Doc: "Stop sigsnoop gadget",
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
			log.Warnf("Gadget %s: error marshalling event: %s", trace.Spec.Gadget, err)
			return
		}
		t.resolver.PublishEvent(traceName, string(r))
	}

	params := trace.Spec.Parameters

	targetSignal := ""
	if signal, ok := params["signal"]; ok {
		targetSignal = signal
	}

	targetPid := int32(0)
	if pid, ok := params["pid"]; ok {
		pidParsed, err := strconv.ParseInt(pid, 10, 32)
		if err != nil {
			trace.Status.OperationError = fmt.Sprintf("%q is not valid for PID", pid)
			return
		}

		targetPid = int32(pidParsed)
	}

	failedOnly := false
	if failed, ok := params["failed"]; ok {
		failedParsed, err := strconv.ParseBool(failed)
		if err != nil {
			trace.Status.OperationError = fmt.Sprintf("%q is not valid for failed", failed)
			return
		}

		failedOnly = failedParsed
	}

	var err error

	config := &tracer.Config{
		MountnsMap:   gadgets.TracePinPath(trace.ObjectMeta.Namespace, trace.ObjectMeta.Name),
		TargetPid:    targetPid,
		TargetSignal: targetSignal,
		FailedOnly:   failedOnly,
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
