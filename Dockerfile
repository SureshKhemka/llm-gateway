# One image for the gateway; the simulator and CLI tools ship in it too so the demo and
# load tests can use the same artifact [NFR-17].
#   docker build -t llm-gateway .
#   docker build --secret id=ca,src=/path/to/proxy-ca.pem -t llm-gateway .   # behind a TLS-intercepting proxy
#   docker run -p 8080:8080 -v $PWD/deploy/demo:/etc/gateway llm-gateway
FROM golang:1.26.10-alpine AS build
WORKDIR /src
ARG VERSION=dev
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=secret,id=ca \
    if [ -f /run/secrets/ca ]; then export SSL_CERT_FILE=/run/secrets/ca; fi; go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ \
      ./cmd/gateway ./cmd/simprovider ./cmd/workload ./cmd/loadtest ./cmd/gwctl

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY deploy/demo/gateway.yaml /etc/gateway/gateway.yaml
ENV GATEWAY_CONFIG=/etc/gateway/gateway.yaml WAL_DIR=/var/lib/gateway/wal
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gateway"]
