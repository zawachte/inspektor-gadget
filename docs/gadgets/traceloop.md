---
# Code generated by 'make generate-documentation'. DO NOT EDIT.
title: Gadget traceloop
---

The traceloop gadget traces system calls in a similar way to strace but with
some differences:

* traceloop uses BPF instead of ptrace
* traceloop&#39;s tracing granularity is the container instead of a process
* traceloop&#39;s traces are recorded in a fast, in-memory, overwritable ring
  buffer like a flight recorder. The tracing could be permanently enabled and
  inspected in case of crash.


### Example CR

```yaml
apiVersion: gadget.kinvolk.io/v1alpha1
kind: Trace
metadata:
  name: traceloop
  namespace: gadget
spec:
  node: ubuntu-hirsute
  gadget: traceloop
  runMode: Manual
  outputMode: ExternalResource
```

### Operations


#### start

Start traceloop

```bash
$ kubectl annotate -n gadget trace/traceloop \
    gadget.kinvolk.io/operation=start
```
#### stop

Stop traceloop

```bash
$ kubectl annotate -n gadget trace/traceloop \
    gadget.kinvolk.io/operation=stop
```

### Output Modes

* ExternalResource
