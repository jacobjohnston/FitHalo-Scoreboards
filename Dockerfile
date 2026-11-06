# The board's container image: one static binary and the root certificates its cloud stores need,
# run as an unprivileged user with its data in /data.

# The build stage runs on the builder's platform and cross-compiles, so arm64 needs no emulation.
# golang:1.27, pinned by the digest of its multi-platform index.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc AS build
WORKDIR /source
COPY . .
# VERSION is the release stamped into the binary.
ARG VERSION=development
# TARGETOS and TARGETARCH are set by buildx for each target platform.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /fithalo-scoreboard ./cmd/fithalo-scoreboard \
    && mkdir /data

# The final stage holds only the binary, the certificates and an empty data directory.
FROM scratch
# VERSION labels the image with the release.
ARG VERSION=development
LABEL org.opencontainers.image.title="FitHalo Scoreboards" \
      org.opencontainers.image.description="A self-hosted scoreboard server for FitHalo." \
      org.opencontainers.image.source="https://github.com/jacobjohnston/FitHalo-Scoreboards" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /fithalo-scoreboard /fithalo-scoreboard
COPY --from=build --chown=65532:65532 /data /data
# 65532 is the conventional nonroot user and group of distroless images.
USER 65532:65532
# The board listens on all interfaces behind the platform's TLS-terminating proxy.
ENV SCOREBOARD_DATA=/data \
    SCOREBOARD_LISTEN=0.0.0.0:8080 \
    SCOREBOARD_TLS=proxy
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/fithalo-scoreboard"]
