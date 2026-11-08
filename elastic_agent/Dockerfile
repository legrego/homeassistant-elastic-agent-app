# The status page served through Home Assistant ingress. It's a static Go
# binary with no dependencies beyond the standard library, cross-compiled for
# the target architecture.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS status
ARG TARGETARCH
WORKDIR /src
COPY status/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/ha-status-server .

# The slim image has the journald input but not journalctl, which the input runs
# to read the journal. journalctl and jq come from the UBI 9 repositories that
# match the base image.
FROM docker.elastic.co/elastic-agent/elastic-agent-slim:9.5.6

# Root is needed to read the host journal. See BEAT_SETUID_AS below.
# hadolint ignore=DL3002
USER 0

RUN microdnf install -y --nodocs --setopt=install_weak_deps=0 jq systemd \
    && microdnf clean all \
    && rm -rf /var/cache/dnf /var/cache/yum /var/lib/dnf/history*

# Beats normally drop root and run as the elastic-agent user. The journal files
# on Home Assistant OS are only readable by root and the host's journal group,
# so keep root. The container gets no capabilities beyond Docker's defaults.
ENV BEAT_SETUID_AS=""

COPY rootfs /
COPY --from=status /out/ha-status-server /usr/local/bin/ha-status-server

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/ha-entrypoint"]

LABEL \
    org.opencontainers.image.title="Home Assistant App: Elastic Agent" \
    org.opencontainers.image.description="Elastic Agent that ships Home Assistant OS logs to Elasticsearch" \
    org.opencontainers.image.source="https://github.com/legrego/homeassistant-elastic-agent-app" \
    org.opencontainers.image.licenses="MIT AND Elastic-2.0"
