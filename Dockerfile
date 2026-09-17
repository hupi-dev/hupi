# One image, every HUPI binary — see docs/GAP_CLOSURE_PLAN.md §5. A
# Kubernetes Deployment runs the gateway (the default CMD); CronJobs and
# one-off Jobs run everything else (hupi-consolidate, hupi-selfcheck,
# hupi-admin, hupi-trace, hupi-correct, hupi-audit, hupi-export,
# hupi-import, hupi-rotate-key, hupi-reembed) from the exact same image
# via `command:` overrides — one build, one thing to version and scan for
# vulnerabilities, not twelve.

# ---- web stage ----
# Builds cmd/hupi-admin-ui's React frontend (cmd/hupi-admin-ui/web) into
# static assets that the Go binary embeds via //go:embed (see
# cmd/hupi-admin-ui/assets.go). This has to happen before the build stage
# below produces the hupi-admin-ui binary — go:embed reads web/dist off
# disk at compile time, not at runtime.
FROM node:20-alpine AS web
WORKDIR /src/cmd/hupi-admin-ui/web
COPY cmd/hupi-admin-ui/web/package*.json ./
RUN npm ci
COPY cmd/hupi-admin-ui/web/ ./
RUN npm run build

# ---- build stage ----
FROM golang:1.25-bookworm AS build
WORKDIR /src

# Dependencies cached in their own layer, invalidated only by go.mod/go.sum
# changing — not by every source edit.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

# Overwrites the placeholder cmd/hupi-admin-ui/web/dist/index.html
# (committed so a plain `go build` never fails with a go:embed error) with
# the real build from the web stage above.
COPY --from=web /src/cmd/hupi-admin-ui/web/dist /src/cmd/hupi-admin-ui/web/dist

# CGO_ENABLED=0: static binaries, no libc dependency on the runtime image
# (Debian's, in the builder, isn't necessarily what the runtime stage
# has — pgx's pure-Go driver never needed CGO anyway, so this costs
# nothing here).
RUN mkdir -p /out && \
    for cmd in hupi hupi-consolidate hupi-selfcheck hupi-trace hupi-correct \
               hupi-admin hupi-admin-ui hupi-audit hupi-export hupi-import hupi-rotate-key hupi-reembed; do \
      CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "/out/$cmd" "./cmd/$cmd"; \
    done

# ---- runtime stage ----
FROM alpine:3.20
# ca-certificates: every provider call and the age library's nothing to
# do with TLS, but the LLM vendor HTTPS calls (OpenAI/Anthropic/etc.) need
# real root certs, which alpine doesn't ship by default.
# postgresql-client: schema/migrate.sh's only dependency — see its own
# doc comment for why this image runs migrations via psql rather than a
# bespoke Go tool.
RUN apk add --no-cache ca-certificates postgresql-client && \
    addgroup -S hupi && adduser -S hupi -G hupi

WORKDIR /app
COPY --from=build /out/ /app/bin/
COPY schema/ /app/schema/
COPY providers.yaml.example /app/providers.yaml.example
ENV PATH="/app/bin:${PATH}"

USER hupi
# Not 127.0.0.1: a container's loopback interface is only reachable from
# inside the same container, which would make the gateway unreachable
# from the Service/Ingress in front of it (docs/GAP_CLOSURE_PLAN.md §5) —
# see deploy/k8s/deployment.yaml's HUPI_LISTEN_ADDR.
EXPOSE 8787 8788
ENTRYPOINT ["hupi"]
