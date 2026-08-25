# syntax=docker/dockerfile:1.7

FROM golang:1.26.6-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=0.2.0-preview
ARG COMMIT=unknown
ARG BUILD_DATE=1970-01-01T00:00:00Z

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY web ./web

RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -buildvcs=false -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
    -o /out/brclio-mail ./cmd/brclio-mail

FROM alpine:3.22

ARG VERSION=0.2.0-preview
ARG COMMIT=unknown
ARG BUILD_DATE=1970-01-01T00:00:00Z

LABEL org.opencontainers.image.title="Brclio Mail" \
      org.opencontainers.image.description="Preview single-node private mail server" \
      org.opencontainers.image.source="https://github.com/Brclio/brclio-mail" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later AND CC-BY-NC-SA-4.0"

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 brclio \
    && adduser -S -D -H -u 10001 -G brclio brclio \
    && install -d -o brclio -g brclio -m 0700 /data /run/tls

COPY --from=build /out/brclio-mail /usr/local/bin/brclio-mail
COPY LICENSE NOTICE THIRD_PARTY_NOTICES /usr/share/licenses/brclio-mail/
COPY LICENSES /usr/share/licenses/brclio-mail/LICENSES

USER 10001:10001
WORKDIR /data

EXPOSE 8080 8443 2525 2465 2587 2993

ENTRYPOINT ["/usr/local/bin/brclio-mail"]
CMD ["serve"]
