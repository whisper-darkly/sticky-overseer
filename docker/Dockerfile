ARG STICKY_RECORDER_REPO=https://github.com/whisper-darkly/sticky-recorder
ARG STICKY_RECORDER_REF=

FROM golang:1.24-alpine AS builder

ARG STICKY_RECORDER_REPO
ARG STICKY_RECORDER_REF

RUN apk add --no-cache git make

# Build sticky-overseer from the repo root (mounted as build context)
WORKDIR /src/sticky-overseer
COPY . ./
RUN make install PREFIX=/out

# Clone and build sticky-recorder
WORKDIR /src
RUN if [ -n "${STICKY_RECORDER_REF}" ]; then \
        git clone --depth=1 --branch "${STICKY_RECORDER_REF}" "${STICKY_RECORDER_REPO}" sticky-recorder; \
    else \
        git clone --depth=1 "${STICKY_RECORDER_REPO}" sticky-recorder; \
    fi
WORKDIR /src/sticky-recorder
RUN make install PREFIX=/out


FROM alpine:3.21

RUN apk add --no-cache ffmpeg

COPY --from=builder /out/bin/ /usr/local/bin/

# sticky-overseer
ENV OVERSEER_PORT=8080
ENV OVERSEER_TRUSTED_CIDRS=""
ENV OVERSEER_LOG_FILE=/data/overseer.jsonl

# sticky-recorder
ENV STICKY_OUT=/data/recordings/{{.Driver}}/{{.Source}}/{{.Session.Year}}-{{.Session.Month}}-{{.Session.Day}}_{{.Session.Hour}}-{{.Session.Minute}}-{{.Session.Second}}/{{.Recording.Year}}-{{.Recording.Month}}-{{.Recording.Day}}_{{.Recording.Hour}}-{{.Recording.Minute}}-{{.Recording.Second}}
ENV STICKY_DRIVER=chaturbate
ENV STICKY_RESOLUTION=720
ENV STICKY_FRAMERATE=30
ENV STICKY_SEGMENT_LENGTH=""
ENV STICKY_COOKIES=""
ENV STICKY_USER_AGENT=""
ENV STICKY_LOG=""
ENV STICKY_LOG_LEVEL=error
ENV STICKY_OUTPUT_FORMAT=json
ENV STICKY_RETRY_DELAY=5s
ENV STICKY_SEGMENT_TIMEOUT=5m
ENV STICKY_RECORDING_TIMEOUT=30m
ENV STICKY_CHECK_INTERVAL=1m
ENV STICKY_EXEC_FATAL=""
ENV STICKY_EXTERNAL_COOKIES=""
ENV STICKY_COOKIES_SAFE_DOMAINS=""
ENV STICKY_COOKIES_JSON=""
ENV STICKY_COOKIES_REFRESH=""

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://localhost:${OVERSEER_PORT}/ws 2>&1 | grep -q "400 Bad Request" || exit 1

CMD ["sticky-overseer", "-command", "sticky-recorder"]
