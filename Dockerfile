# syntax=docker/dockerfile:1
#
# Default image: static binary on distroless. Probes every HTTP provider
# (claude, codex, kiro, copilot, deepseek) plus the local-only verdicts
# (gemini, muse). It does NOT contain the agy CLI, so agy sources report
# cause=not_installed — see the README "agy" section for the sidecar /
# derived-image options (or mount this image as a Kubernetes image volume
# into a container that has agy, and run /usr/local/bin/ccleft from there).
# Cross-compiled on the build platform (no QEMU): linux/amd64 + linux/arm64.
#
# Base images are pinned to multi-arch index digests for reproducibility and
# supply-chain security (tuna-os/ccleft#21); Renovate keeps the digests current.
# Resolve a digest with:
#   skopeo inspect --raw docker://<image>:<tag> | sha256sum
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS build
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

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/ccleft /usr/local/bin/ccleft
USER nonroot:nonroot
EXPOSE 9464
ENTRYPOINT ["/usr/local/bin/ccleft"]
CMD ["serve", "--addr", ":9464", "--config", "/etc/ccleft/ccleft.yaml"]
