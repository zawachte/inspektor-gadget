ARG BUILDER_IMAGE=golang:1.18-bullseye
ARG BASE_IMAGE=alpine:3.14

FROM ${BUILDER_IMAGE} as builder

# default image that will be used in the deploy command
ARG CONTAINER_REPO="docker.io/kinvolk/gadget"
ENV CONTAINER_REPO=${CONTAINER_REPO}

ARG IMAGE_TAG
ENV IMAGE_TAG=${IMAGE_TAG}

# Cache go modules so they won't be downloaded at each build
COPY go.mod go.sum /gadget/
RUN cd /gadget && go mod download

# This COPY is limited by .dockerignore
COPY ./ /gadget
RUN cd /gadget && make kubectl-gadget

FROM ${BASE_IMAGE}
COPY --from=builder /gadget/kubectl-gadget /kubectl-gadget
