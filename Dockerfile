# syntax=docker/dockerfile:1.8
ARG GO_VERSION=1.26
ARG BUF_VERSION=1.66.0

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS buf
ARG BUF_VERSION
RUN apk add --no-cache curl
RUN curl -sSL \
      "https://github.com/bufbuild/buf/releases/download/v${BUF_VERSION}/buf-$(uname -s)-$(uname -m)" \
      -o /usr/local/bin/buf && \
    chmod +x /usr/local/bin/buf

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

COPY --from=buf /usr/local/bin/buf /usr/local/bin/buf
RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY buf.gen.yaml buf.yaml ./
# buf.gen.yaml pins the spk-ai/api revision the bindings are generated from.
RUN buf generate --include-imports --template buf.gen.yaml

COPY . .

ARG TARGETOS TARGETARCH
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-s -w" -o /out/agynd ./cmd/agynd

FROM alpine:3.21 AS runtime

WORKDIR /app

COPY --from=build /out/agynd /app/agynd

RUN addgroup -g 10001 -S app && adduser -u 10001 -S app -G app

USER 10001

ENTRYPOINT ["/app/agynd"]
