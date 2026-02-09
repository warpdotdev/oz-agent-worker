# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o oz-agent-worker .

# Runtime stage
FROM alpine:latest

# Install ca-certificates for HTTPS connections
RUN apk --no-cache add ca-certificates

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/oz-agent-worker .

ENTRYPOINT ["./oz-agent-worker"]
