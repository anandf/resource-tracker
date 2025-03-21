# Build stage
FROM golang:1.23 AS builder


RUN mkdir -p /src/argocd-resource-tracker
# Set working directory
WORKDIR /src/argocd-resource-tracker

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download
# Copy source code
COPY . .

RUN mkdir -p dist && \
	make build

# Final runtime stage
FROM alpine:3.21

# Install necessary dependencies
RUN apk update && \
    apk upgrade && \
    apk add --no-cache tini

# Create necessary directories and user
RUN mkdir -p /usr/local/bin /app/config && \
    adduser --home "/app" --disabled-password --uid 1000 argocd

# Copy built binary from builder stage
COPY --from=builder /src/argocd-resource-tracker/dist/argocd-resource-tracker /usr/local/bin/

# Set user permissions
USER 1000

# Set entrypoint
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/argocd-resource-tracker"]
