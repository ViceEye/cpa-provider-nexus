FROM golang:1.26.7-bookworm AS builder
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN /usr/local/go/bin/go mod download

COPY . .
RUN /usr/local/go/bin/gofmt -w cmd/cpa-provider-nexus/*.go internal/*/*.go \
    && /usr/local/go/bin/go vet ./... \
    && /usr/local/go/bin/go test ./... \
    && mkdir -p /out/linux/${TARGETARCH} \
    && CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH} /usr/local/go/bin/go build \
       -buildvcs=false -trimpath -buildmode=c-shared \
       -ldflags="-s -w" \
       -o /out/linux/${TARGETARCH}/cpa-provider-nexus-v0.10.1.so ./cmd/cpa-provider-nexus \
    && rm -f /out/linux/${TARGETARCH}/*.h \
    && cd /out/linux/${TARGETARCH} \
    && sha256sum cpa-provider-nexus-v0.10.1.so > cpa-provider-nexus-v0.10.1.so.sha256

FROM scratch
COPY --from=builder /out/ /
