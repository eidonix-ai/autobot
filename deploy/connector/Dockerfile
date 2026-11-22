# The connector's image. Build from the repository root (compose.yml does this).
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /autobot-connector ./cmd/autobot-connector

# Ubuntu, so the nvidia-smi that the GPU device spec injects finds a recent enough C library.
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /autobot-connector /usr/local/bin/autobot-connector
ENTRYPOINT ["/usr/local/bin/autobot-connector"]
CMD ["run"]
