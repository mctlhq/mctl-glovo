# One image, two entrypoints: the service (default) and the schema migrator
# (command override: glovo-migrate). Built centrally by mctl-gitops'
# build-image.yaml -> ghcr.io/mctlhq/mctl-glovo:<semver>, no v prefix.
#
# Base digest: golang:1.27.4-alpine manifest list, the same one mctl-api
# pins (resolved 2026-11-11). go.mod pins 1.26.11; a newer toolchain
# builds it as is.
FROM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Only the two cluster binaries. glovo-home and glovo-link run on a person's
# own machine and are not shipped here.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mctl-glovo ./cmd/mctl-glovo \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/glovo-migrate ./cmd/glovo-migrate

FROM alpine:3.24

RUN apk add --no-cache ca-certificates \
 && addgroup -g 10001 app && adduser -D -u 10001 -G app app

COPY --from=builder /out/mctl-glovo /out/glovo-migrate /usr/local/bin/

USER 10001:10001

EXPOSE 8080

# Default: the service. The migration Job overrides the command with
# glovo-migrate.
ENTRYPOINT ["mctl-glovo"]
