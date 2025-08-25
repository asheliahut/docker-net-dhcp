FROM golang:1.24-alpine3.22 AS builder

WORKDIR /usr/local/src/docker-net-dhcp
COPY go.* ./
RUN go mod download

COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
RUN mkdir bin/ && go build -o bin/ ./cmd/...


FROM alpine:3.22

RUN mkdir -p /run/docker/plugins

COPY --from=builder /usr/local/src/docker-net-dhcp/bin/net-dhcp /usr/sbin/

ENTRYPOINT ["/usr/sbin/net-dhcp"]
