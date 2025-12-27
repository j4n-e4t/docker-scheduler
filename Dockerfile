FROM golang:1.25-alpine AS builder

# Install tzdata for timezone support
RUN apk add --no-cache tzdata

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum* ./
RUN go mod download

# Copy source code
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /docker-scheduler

# Final minimal image
FROM scratch

# Copy timezone data for timezone support
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Environment variables with defaults
ENV TZ=UTC
ENV POLL_INTERVAL=30s
ENV DOCKER_HOST=unix:///var/run/docker.sock

# Copy binary
COPY --from=builder /docker-scheduler /docker-scheduler

ENTRYPOINT ["/docker-scheduler"]
