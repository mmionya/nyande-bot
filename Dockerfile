FROM golang:1.24-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
COPY resources ./resources
RUN CGO_ENABLED=0 go test ./cmd/... ./internal/... ./resources/... && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nyande-bot ./cmd/nyande-bot

FROM denoland/deno:bin-2.9.4 AS deno-runtime

FROM python:3.12-slim-bookworm AS runtime-lite
ENV PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1
COPY --from=deno-runtime /deno /usr/local/bin/deno
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl ffmpeg \
    && pip install --no-cache-dir "yt-dlp[default]" \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /out/nyande-bot /usr/local/bin/nyande-bot
RUN useradd --create-home --uid 10001 nyande && mkdir -p /app/data \
	&& chown -R nyande:nyande /app
USER nyande
ENTRYPOINT ["/usr/local/bin/nyande-bot"]

FROM runtime-lite AS runtime
USER root
RUN pip install --no-cache-dir openai-whisper
USER nyande
