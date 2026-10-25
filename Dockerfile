FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN for b in vigil vigil-agent vigil-server vigil-relay; do \
      CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/ijcodelabs/vigil/internal/version.Version=${VERSION}" -o /out/$b ./cmd/$b || exit 1; \
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
