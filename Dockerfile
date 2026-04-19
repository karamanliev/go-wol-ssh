# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -o /out/ssh-wol ./...

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/ssh-wol /usr/local/bin/ssh-wol
ENTRYPOINT ["/usr/local/bin/ssh-wol"]
CMD ["/etc/ssh-wol/config.yaml"]
