# syntax=docker/dockerfile:1

# The build context MUST include .git: go build stamps vcs.revision/vcs.time
# into the binary from it, and the deploy runbook verifies that stamp
# (strings|grep vcs.revision, /healthz vcs_revision). .dockerignore must never
# exclude .git — tests/image_pipeline_test.go guards both directions.
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
# -buildvcs=false is forbidden here: it would drop the vcs.revision stamp the
# runbook verifies. CGO is already absent; CGO_ENABLED=0 keeps the binary
# fully static for the distroless runtime stage.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -o /out/handoffkeep ./cmd/handoffkeep

# Final stage: exactly one static binary, nonroot, no shell, no source, no env
# files, no secrets. distroless/static-debian12 ships CA certs and a nonroot
# user (65532) so TLS clients (Linear, R2, hub) work unchanged.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/handoffkeep /handoffkeep
USER nonroot
ENTRYPOINT ["/handoffkeep"]
