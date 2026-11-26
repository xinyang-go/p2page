FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

RUN apk add --no-cache ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
	go build -trimpath -ldflags="-s -w" -o /out/p2page ./cmd/p2page

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata \
	&& addgroup -g 10001 -S p2page \
	&& adduser -u 10001 -S -D -H -G p2page p2page \
	&& mkdir -p /etc/p2page /var/lib/p2page \
	&& chown 10001:10001 /var/lib/p2page \
	&& chmod 0700 /var/lib/p2page

COPY --from=build /out/p2page /usr/local/bin/p2page

USER 10001:10001
WORKDIR /var/lib/p2page
VOLUME ["/var/lib/p2page"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/p2page"]
CMD ["server", "-config", "/etc/p2page/server.json"]
