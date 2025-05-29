# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM tonistiigi/xx AS xx
FROM --platform=$BUILDPLATFORM golang:1.24.2-alpine AS build-env
COPY --from=xx / /

RUN xx-apk add --no-cache \
    git \
    make \
    gcc \
    libc-dev \
    tzdata \
    zip \
    ca-certificates

ENV GO111MODULE=on \
    CGO_ENABLED=0

WORKDIR /src

COPY go.mod .
COPY go.sum .
RUN go mod download

# add source
ADD . .

ARG TARGETPLATFORM
RUN xx-info env

RUN xx-go --wrap
RUN make build
RUN xx-verify /src/bin/dex
RUN xx-go --unwrap

RUN mkdir -p \
        /rootfs/app \
        /rootfs/usr/share \
        /rootfs/etc/ssl/certs \
    && cp -t /rootfs/app /src/bin/dex \
    && : `# the timezone data:` \
    && cp -Rt /rootfs/usr/share /usr/share/zoneinfo \
    && : `# the tls certificates:` \
    && cp -t /rootfs/etc/ssl/certs /etc/ssl/certs/ca-certificates.crt

# final stage
FROM scratch
COPY --from=build-env /rootfs /

ENTRYPOINT ["/app/dex"]
