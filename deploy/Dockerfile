# Images for the server (default target) and the CI runner. Build from the
# repository root:
#   docker build -f deploy/Dockerfile -t forge .
#   docker build -f deploy/Dockerfile --target runner -t forge-runner .
# or use deploy/docker-compose.yml.
# The build stage runs on the build machine's own platform and cross-compiles,
# so multi-arch builds (buildx --platform) don't compile under emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.1.0-dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /forge ./cmd/forge

# Runner: git for checkouts, the docker CLI to start job containers through
# the host's Docker socket. Runs as root because it needs that socket anyway.
FROM alpine:3.20 AS runner
RUN apk add --no-cache git git-lfs docker-cli docker-cli-buildx ca-certificates
COPY --from=build /forge /usr/local/bin/forge
ENV FORGE_RUNNER_EXECUTOR=docker FORGE_LOG_FORMAT=json
ENTRYPOINT ["forge", "runner"]

# Server.
FROM alpine:3.20 AS server
RUN apk add --no-cache git git-daemon openssh-client ca-certificates && adduser -D -h /data forge && chmod 700 /data
COPY --from=build /forge /usr/local/bin/forge
USER forge
ENV FORGE_DATA_DIR=/data FORGE_SSH_LISTEN=:23231 FORGE_HTTP_LISTEN=:23232
VOLUME /data
EXPOSE 23231 23232
ENTRYPOINT ["forge"]
CMD ["serve"]
