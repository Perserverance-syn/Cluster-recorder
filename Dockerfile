# One image, two static binaries. CGO is off: the SQLite driver and netlink are pure Go.
FROM golang:1.27 AS build
ARG VERSION=dev
WORKDIR /work
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/collector ./cmd/collector \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/agent ./cmd/agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/collector /collector
COPY --from=build /out/agent /agent
# The collector runs as this user. The agent DaemonSet overrides to root
# (read-only netlink and /etc/cni/net.d need it) with every capability dropped.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/collector"]