FROM golang:1.24-alpine AS builder

RUN apk add --no-cache make

WORKDIR /src/sticky-overseer
COPY . ./
RUN make install PREFIX=/out

FROM alpine:3.21

COPY --from=builder /out/bin/sticky-overseer /usr/local/bin/sticky-overseer

ENV OVERSEER_PORT=8080
ENV OVERSEER_TRUSTED_CIDRS=""
ENV OVERSEER_LOG_FILE=""
ENV OVERSEER_DB=""

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://localhost:${OVERSEER_PORT}/ws 2>&1 | grep -q "400 Bad Request" || exit 1

CMD ["sticky-overseer"]
