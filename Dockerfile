FROM golang:1.26-bookworm AS builder

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends build-essential git && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
ARG GO_BUILD_PARALLELISM=2

RUN CGO_ENABLED=1 GOOS=linux go build -p "${GO_BUILD_PARALLELISM}" -buildvcs=false -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

RUN CGO_ENABLED=1 GOOS=linux go build -p "${GO_BUILD_PARALLELISM}" -buildvcs=false -buildmode=c-shared -o /app/bps.so ./examples/plugin/bps/go

FROM debian:bookworm

RUN apt-get update && apt-get install -y --no-install-recommends tzdata ca-certificates && rm -rf /var/lib/apt/lists/*

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

# Keep bundled artifacts outside the mounted plugin directory. Deployment copies
# the matching BPS build into that directory before replacing the server.
COPY --from=builder /app/bps.so /opt/cliproxy-plugins/bps.so

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

# CI builds and verifies the customized panel before production deployment.
# Source-only Docker builds retain the upstream automatic panel bootstrap.
COPY management-ui /opt/cpa-management
RUN mkdir -p /CLIProxyAPI/static && \
    if test -f /opt/cpa-management/management.html; then cp /opt/cpa-management/management.html /CLIProxyAPI/static/management.html; fi && \
    rm -rf /opt/cpa-management

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
