FROM golang:1.21-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /build/spam-observer .

FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 1000 appuser

COPY --from=builder /build/spam-observer /usr/local/bin/spam-observer

RUN mkdir -p /data && chown appuser:appuser /data

USER appuser

WORKDIR /home/appuser

EXPOSE 8080

ENTRYPOINT ["spam-observer"]
