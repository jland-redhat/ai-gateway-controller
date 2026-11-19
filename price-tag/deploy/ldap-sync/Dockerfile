FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        ldap-utils \
        postgresql-client \
        python3 \
        tzdata \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY scripts/sync-ldap-cost-centers.sh /app/scripts/sync-ldap-cost-centers.sh
COPY deploy/ldap-sync/run-service.sh /app/run-service.sh
RUN chmod 0755 /app/scripts/sync-ldap-cost-centers.sh /app/run-service.sh

ENV LDAP_SYNC_SCRIPT=/app/scripts/sync-ldap-cost-centers.sh
ENV LDAP_SYNC_OUTPUT_DIR=/var/lib/ldap-sync
ENV TZ=UTC

ENTRYPOINT ["/app/run-service.sh"]
