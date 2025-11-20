# Copyright The Linux Foundation and its contributors.
# SPDX-License-Identifier: MIT

# checkov:skip=CKV_DOCKER_7:No free access to Chainguard versioned labels.
# hadolint global ignore=DL3007

# Build stage
FROM --platform=$BUILDPLATFORM cgr.dev/chainguard/go:latest AS builder

# Set necessary environment variables needed for our image. Allow building to
# other architectures via cross-compilation build-arg.
ARG TARGETARCH
ENV CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH

# Set working directory
WORKDIR /build

# Download dependencies to go modules cache
COPY go.mod go.sum ./
RUN go mod download

# Copy the code into the container
COPY . .

# Build the application
RUN go build -o /go/bin/v1-sync-helper -trimpath -ldflags="-w -s" .

# Run our go binary standalone
FROM cgr.dev/chainguard/static:latest

# Implicit with base image; setting explicitly for linters.
USER nonroot

COPY --from=builder /go/bin/v1-sync-helper /v1-sync-helper

ENTRYPOINT ["/v1-sync-helper"]
