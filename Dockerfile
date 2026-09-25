# syntax=docker/dockerfile:1
#
# Default image: static binary on distroless. Probes every HTTP provider
# (claude, codex, kiro, copilot, deepseek) plus the local-only verdicts
# (gemini, muse). It does NOT contain the agy CLI, so agy sources report
# cause=not_installed — see the README "agy" section for the sidecar /
# derived-image options (or mount this image as a Kubernetes image volume
# into a container that has agy, and run /usr/local/bin/ccleft from there).
# Cross-compiled on the build platform (no QEMU): linux/amd64 + linux/arm64.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.24 AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X github.com/tuna-os/ccleft.Version=${VERSION}" \
      -o /out/ccleft ./cmd/ccleft

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ccleft /usr/local/bin/ccleft
USER nonroot:nonroot
EXPOSE 9464
ENTRYPOINT ["/usr/local/bin/ccleft"]
CMD ["serve", "--addr", ":9464", "--config", "/etc/ccleft/ccleft.yaml"]
