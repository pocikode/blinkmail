# Blinkmail — multi-stage container build.
#
# Uses a pure-Go SQLite driver (modernc.org/sqlite), so the whole build runs
# with CGO_ENABLED=0 and produces a static binary that runs on a minimal base.

# ---- Build stage ----
FROM golang:1.27.2-alpine AS build

WORKDIR /src

# Cache dependencies separately from source so code changes do not re-download
# the module graph.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Static binary, no CGO. -trimpath removes local filesystem paths; ldflags strip
# symbol tables to shrink the image.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/blinkmail ./cmd/blinkmail

# Create the data directory in a stage that has a shell, so the distroless
# runtime can COPY it in with the correct owner and no chown binary.
RUN mkdir -p /data

# ---- Runtime stage ----
# distroless/static has no shell or package manager, minimising attack surface.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# The nonroot user (uid 65532) owns the data volume so SQLite can create its
# database and WAL files.
USER nonroot

# Persistent data directory; mounted from a named volume in compose.
WORKDIR /
ENV LISTEN_ADDR=":8080" \
    DB_PATH="/data/blinkmail.db"

COPY --from=build /out/blinkmail /usr/local/bin/blinkmail

# Seed /data owned by the nonroot user (uid 65532). Docker initialises an empty
# named volume from the image's mount-point ownership, so the runtime user can
# create the SQLite database and its WAL files without a shell chown.
COPY --from=build --chown=65532:65532 /data /data

EXPOSE 8080

# Container-level health check against the app's own /healthz.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/blinkmail", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/blinkmail"]
