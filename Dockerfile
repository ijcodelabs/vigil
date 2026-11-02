# The build stage runs on the builder's own platform and cross-compiles, so
# a multi-arch build (docker buildx --platform linux/amd64,linux/arm64)
# needs no emulation. Without buildx, TARGETOS and TARGETARCH are empty and
# Go builds for the machine it runs on.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.10-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN for b in vigil vigil-agent vigil-server vigil-relay; do \
      CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X github.com/ijcodelabs/vigil/internal/version.Version=${VERSION}" -o /out/$b ./cmd/$b || exit 1; \
    done

# One image per part, so a DaemonSet on every node pulls only the agent:
#   docker build --target agent -t vigil-agent .
FROM scratch AS agent
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/vigil-agent /vigil-agent
ENTRYPOINT ["/vigil-agent"]

FROM scratch AS server
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/vigil-server /vigil-server
ENTRYPOINT ["/vigil-server"]

FROM scratch AS relay
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/vigil-relay /vigil-relay
ENTRYPOINT ["/vigil-relay"]

# The default target is the all-in-one `vigil`, as before, for one release.
FROM scratch AS all
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/vigil /vigil
ENTRYPOINT ["/vigil"]
