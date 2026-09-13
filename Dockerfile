FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
COPY resources ./resources
RUN CGO_ENABLED=0 go test ./cmd/... ./internal/... ./resources/... && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nyande-bot ./cmd/nyande-bot

FROM denoland/deno:bin-2.9.4 AS deno-runtime

FROM python:3.12-slim-bookworm AS media-runtime
ENV PYTHONUNBUFFERED=1 \
    UV_LINK_MODE=copy
COPY --from=ghcr.io/astral-sh/uv:0.12.12 /uv /usr/local/bin/uv
COPY --from=deno-runtime /deno /usr/local/bin/deno
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl ffmpeg \
    && rm -rf /var/lib/apt/lists/*
RUN --mount=type=cache,id=nyande-uv,target=/root/.cache/uv,sharing=locked \
    uv pip install --system "yt-dlp[default]"
WORKDIR /app
RUN useradd --create-home --uid 10001 nyande && mkdir -p /app/data \
	&& chown -R nyande:nyande /app
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
