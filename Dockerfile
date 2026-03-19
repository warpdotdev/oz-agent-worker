# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o oz-agent-worker .

# Runtime stage
FROM alpine:3.22

# Install ca-certificates for HTTPS connections and create a non-root runtime user
RUN apk --no-cache add ca-certificates \
    && addgroup -S oz \
    && adduser -S -D -u 10001 -G oz oz

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/oz-agent-worker .
USER oz

ENTRYPOINT ["./oz-agent-worker"]
