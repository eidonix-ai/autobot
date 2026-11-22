# The connector's image, for linux/amd64 and linux/arm64. Build from the repository root:
#   docker buildx build --platform linux/amd64,linux/arm64 -f deploy/connector/Dockerfile .
# The Go program is cross-compiled on the build machine, and the final stage runs nothing, so one
# machine builds both architectures without emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/eidonix-ai/autobot/internal/connector.Version=$VERSION" \
      -o /autobot-connector ./cmd/autobot-connector

# Ubuntu, so the nvidia-smi that the GPU device spec injects finds a recent enough C library.
FROM ubuntu:24.04
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /autobot-connector /usr/local/bin/autobot-connector
LABEL org.opencontainers.image.source=https://github.com/eidonix-ai/autobot \
      org.opencontainers.image.description="Autobot connector: lends a machine to Autobot for isolated test runs"
ENTRYPOINT ["/usr/local/bin/autobot-connector"]
CMD ["run"]
