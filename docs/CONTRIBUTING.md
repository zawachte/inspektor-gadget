---
title: Contributing
weight: 100
description: >
  How to contribute to Inspektor Gadget.
---

Here you can learn how you can contribute to Inspektor Gadget.

## Getting started

To better understand how the pieces fit together, we recommend reading the
[architecture](architecture.md) documentation before starting to play with
Inspektor Gadget.

### Setup developer environment

- [Fork](https://github.com/kinvolk/inspektor-gadget/fork) and clone this repo:
    - `git clone git@github.com:your_account/inspektor-gadget.git`.
- Install [Docker](https://docs.docker.com/get-docker/) and [Golang](https://golang.org/doc/install).

## Building the code

Inspektor Gadget is composed of a client executable that runs on the
operator's machine, and a container image that runs in the Kubernetes
cluster. They can be built together or independently.

### Building the client executable

You can compile the client executable for your platform by running `make kubectl-gadget`.

To cross compile for all supported platforms, you can run `make
kubectl-gadget-all` or select a specific one with `make
kubectl-gadget-linux-amd64` or `make kubectl-gadget-darwin-amd64`.

### Building the gadget container image

Inspektor Gadget provides two different container images:
- gadget-default: Contains CO-RE and BCC gadgets
- gadget-core: Containes only CO-RE gadgets that are integrated with the gadget tracer manager
  * traceloop and the network policy advisor gadgets aren't included,
    they will be included once
    [#371](https://github.com/kinvolk/inspektor-gadget/issues/371) and
    [#172](https://github.com/kinvolk/inspektor-gadget/issues/172) are
    fixed.

**Note**: Using a locally built container image requires pushing it to a container
repository, either local or remote. The default repository can be
overridden by changing the value of the `CONTAINER_REPO` env variable,
which defaults to `docker.io/kinvolk/gadget` if not defined.

You can build and push the container gadget image by running the following commands:

```bash
$ make gadget-default-container # or make gadget-core-container
$ make push-gadget-container
```

The BPF code is built using a Docker container, so you don't have to worry
installing the compilers to build it.

If you push the container images to another registry, you can use the `--image`
argument when deploying to the Kubernetes cluster.

### Building notes

- The compilation uses `tools/image-tag` to choose the tag of the container
image to use according to the branch that you are compiling.
- If you wish to make changes to traceloop program, update
`gadget-default.Dockerfile` to pick your own image of traceloop.
- As for traceloop, it is also possible to change the BCC to be used as
described in [BCC](#Updating-BCC-from-upstream) section.
- You can generate the required BTF information for some well known
  kernel versions by setting `ENABLE_BTFGEN=true`

## Testing

### Development environment on minikube

For faster iteration, it's possible to make changes to Inspektor Gadget and
test them on minikube locally without pushing container images to any
registry.

* Follow the specific [installation instructions](install.md#minikube) for minikube.
* Deploy the locally modified version of Inspektor Gadget to an already
  running minikube cluster with `make minikube-install`.

### Unit tests

You can run the different unit tests with:

```bash
$ make test
```

### Integration tests

The integration tests use a Kubernetes cluster to deploy and test Inspektor Gadget.
Be sure that you have a valid kubeconfig and run:

```bash
$ export KUBECONFIG=... # not needed if valid config in $HOME/.kube/config
$ make integration-tests
```
### Continuous Integration

Inspektor Gadget uses GitHub Actions as CI. Please check dedicated [CI
documentation](ci.md) for details.

## Contribution Guidelines

### Code of Conduct

Please refer to the Kinvolk
[Code of Conduct](https://github.com/kinvolk/contribution/blob/master/CODE_OF_CONDUCT.md).

### Authoring PRs

For making PRs/commits consistent and easier to review, please check out
Kinvolk's [contribution guidelines on git](https://github.com/kinvolk/contribution/blob/master/topics/git.md).

### Good first issues

If you're looking where to start, you can check the issues with the
`good first issue` label on
[Inspektor Gadget](https://github.com/kinvolk/inspektor-gadget/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22) or
[traceloop](https://github.com/kinvolk/traceloop/issues?q=is%3Aopen+is%3Aissue+label%3A%22good+first+issue%22).
Don't hesitate to
[talk to us](https://github.com/kinvolk/inspektor-gadget#discussions)
if you need further help.

### Proposing new features

If you want to propose a new feature or do a big change in the architecture
it's highly recommended to open an issue first to discuss it with the team.

## Planning

Our planning is published through two different project boards:

 * [Inspektor Gadget Roadmap](https://github.com/orgs/kinvolk/projects/22/views/1)
   has the high level view of the big issues that we are planning to tackle
   in the upcoming months.
 * [Inspektor Gadget Sprint Planning](https://github.com/orgs/kinvolk/projects/29/views/2)
   has the week-to-week plans of which bugs we are currently working on,
   and the different priorities of the issues involved.

## BCC

### Porting BCC gadgets

This project uses some gadgets from [BCC](https://github.com/iovisor/bcc/).
Instead of keeping our patched versions, we prefer to make those gadgets
suitable to be used with Inspektor Gadget by contributing to the upstream project.

A BCC gadget has to provide a
[filtering mechanism](https://github.com/iovisor/bcc/blob/master/docs/special_filtering.md)
by cgroup id and mount namespace id in order to be compatible with Inspektor Gadget.
You can get some inspiration from the
[opensnoop](https://github.com/iovisor/bcc/blob/8cd2717de91983aeeadefd0886031bd4d8e920ee/tools/opensnoop.py#L127) and
[execsnoop](https://github.com/iovisor/bcc/blob/8cd2717de91983aeeadefd0886031bd4d8e920ee/tools/execsnoop.py#L149)
implementations to port a different BCC gadget.

Once the gadget has been updated in the BCC repo, it can be added to Inspektor
Gadget by filling a PR adding the gadget to
[`cmd/kubectl-gadget/bcck8s.go`](https://github.com/kinvolk/inspektor-gadget/blob/0cf97d9ea6432f080eafa1a3280f3447085ea96a/cmd/kubectl-gadget/bcck8s.go#L26).
The [add gadget bindsnoop](https://github.com/kinvolk/inspektor-gadget/pull/35/files#diff-f616fa5f11da59a9ae7344d196bbf357R40-R43)
PR is an example of it.

The [adding new BCC-based gadgets in Inspektor Gadget](https://kinvolk.io/blog/2020/04/adding-new-bcc-based-gadgets-in-inspektor-gadget/)
blogpost presents some more details about this process.

### Updating BCC from upstream

As you can see in `gadget-default.Dockerfile`, the gadget container image
uses the BCC container image as its parent image.
Given that there is not an official container repository to get that BCC image,
we keep a synchronised [Kinvolk BCC fork](https://github.com/kinvolk/bcc)
that is configured to publish the images on Kinvolk container registries
[Quay](https://quay.io/repository/kinvolk/bcc) and
[Docker Hub](https://hub.docker.com/r/kinvolk/bcc/), by using the
[Github actions](https://github.com/iovisor/bcc/blob/master/.github/workflows/publish.yml)
already available in [BCC upstream](https://github.com/iovisor/bcc).

Given that, if you want to update the BCC version used by Inspektor Gadget,
it is necessary to first update the
[Kinvolk BCC fork](https://github.com/kinvolk/bcc)
so that the Github actions are triggered, and a new image is published.
Once the image is available in registries, you have to update
`gadget-default.Dockerfile` so that it uses the just created image, same goes for local
compilation with `gadget-local.Dockerfile`. The
[Update BCC container image](https://github.com/kinvolk/inspektor-gadget/pull/190)
PR is an example of it.

Currently, we use Docker Hub to pull the BCC image when building the gadget
container image. Notice we do not use the `latest` tag because it is overwritten
after each push on master branch. Instead, we use the
[stable unique tags](https://github.com/elgohr/Publish-Docker-Github-Action#snapshot)
that are named with format: `<Timestamp><Commit-SHA>`. For instance, tag
`202107061407494e8e8c` describes that it was created in 2021-07-06 at 14:07:49
from commit SHA starting with `4e8e8c`.
