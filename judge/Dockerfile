# Judge image: a real herdr 0.9.3 (the release binary, pinned by SHA-256) in
# a minimal Ubuntu, plus a fake `claude` on PATH that draws the screens
# herdr's Claude manifest recognizes. The binary under test is not baked in;
# `run.sh` mounts it, so the image is cached across changes to either binary.
FROM ubuntu:24.04

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git jq sqlite3 \
    && rm -rf /var/lib/apt/lists/*

ARG HERDR_VERSION=0.9.3
ARG HERDR_SHA256=18a8dc65f1c2fa485884344356dea1cfd911c6f06cf46fa78e193f4087f4dba7
RUN curl -fsSL -o /usr/local/bin/herdr \
        "https://github.com/herdrdev/herdr/releases/download/v${HERDR_VERSION}/herdr-linux-x86_64" \
    && echo "${HERDR_SHA256}  /usr/local/bin/herdr" | sha256sum -c - \
    && chmod +x /usr/local/bin/herdr

RUN useradd -m -s /bin/bash agent
COPY --chmod=755 bin/claude /usr/local/bin/claude
USER agent
WORKDIR /home/agent
