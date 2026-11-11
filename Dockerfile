# syntax=docker/dockerfile:1
#
# One image holds every Ferry binary (/ferry/api, relay, worker, watcher,
# recon, migrate); each ECS service picks its command. The runtime stage is
# distroless "static": no shell, no package manager, runs as non-root, and
# suits a read-only root filesystem. Base images are pinned by digest so a
# moved tag cannot change what ships; bump them deliberately.

# Build stage. Go matches the toolchain line in go.mod (1.27.4, see SA-004);
# nothing from this stage ships except the binaries.
FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w" -o /out/ ./cmd/...

# Runtime stage.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/ /ferry/
# Public AWS RDS root CAs, so services verify the database's certificate
# (PGSSLMODE=verify-full, PGSSLROOTCERT=/ferry/rds-ca.pem). Source:
# https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem
COPY deploy/rds/global-bundle.pem /ferry/rds-ca.pem
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/ferry/api"]
