FROM golang:1.26-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
COPY resources ./resources
RUN CGO_ENABLED=0 go test ./cmd/... ./internal/... ./resources/... && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nyande-bot ./cmd/nyande-bot

FROM denoland/deno:bin-2.9.4 AS deno-runtime

FROM python:3.12-slim-bookworm AS media-runtime
ARG BGUTIL_VERSION=2.0.1
ENV PYTHONUNBUFFERED=1 \
    UV_LINK_MODE=copy
COPY --from=ghcr.io/astral-sh/uv:0.12.12 /uv /usr/local/bin/uv
COPY --from=deno-runtime /deno /usr/local/bin/deno
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl ffmpeg \
    && rm -rf /var/lib/apt/lists/*
RUN --mount=type=cache,id=nyande-uv,target=/root/.cache/uv,sharing=locked \
    uv pip install --system "yt-dlp[default,curl-cffi]" "bgutil-ytdlp-pot-provider==${BGUTIL_VERSION}"
WORKDIR /app
RUN useradd --create-home --uid 10001 nyande && mkdir -p /app/data \
	&& chown -R nyande:nyande /app
# yt-dlp discovers the provider in the runtime user's home; no daemon or cookies.
# ponytail: script startup adds latency; use bgutil's HTTP mode at high concurrency.
RUN mkdir -p /home/nyande/bgutil-ytdlp-pot-provider \
    && curl -fsSL --retry 3 "https://github.com/Brainicism/bgutil-ytdlp-pot-provider/archive/refs/tags/${BGUTIL_VERSION}.tar.gz" -o /tmp/bgutil.tar.gz \
    && tar -xzf /tmp/bgutil.tar.gz --strip-components=1 -C /home/nyande/bgutil-ytdlp-pot-provider \
    && cd /home/nyande/bgutil-ytdlp-pot-provider/server \
    && DENO_DIR=/tmp/bgutil-deno-cache deno install --prod --allow-scripts=npm:canvas --frozen \
    && rm -rf /tmp/bgutil.tar.gz /tmp/bgutil-deno-cache
ENTRYPOINT ["/usr/local/bin/nyande-bot"]

# Keep Python dependencies independent of changes to the Go binary.
FROM media-runtime AS whisper-runtime
RUN --mount=type=cache,id=nyande-uv,target=/root/.cache/uv,sharing=locked \
    uv pip install --system openai-whisper

FROM media-runtime AS runtime-lite
COPY --from=builder /out/nyande-bot /usr/local/bin/nyande-bot
USER nyande

FROM whisper-runtime AS runtime
COPY --from=builder /out/nyande-bot /usr/local/bin/nyande-bot
USER nyande
