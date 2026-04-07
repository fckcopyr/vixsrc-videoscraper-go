# Build stage
FROM golang:1.25.5-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the binary with optimizations
RUN go build -ldflags="-s -w" -o server.out

# Runtime stage
FROM alpine:latest

WORKDIR /app

# Install ca-certificates for HTTPS requests
RUN apk --no-cache add ca-certificates

# Copy the binary from builder
COPY --from=builder /app/server.out .
# Copy .env file if it exists
COPY --from=builder /app/.env* ./

# Expose the port (default 5000 for Gin according to main.go)
EXPOSE 5000

# Run the binary
CMD ["./server.out"]
