# Build stage
FROM golang:alpine AS builder

WORKDIR /app

# Install gcc and musl-dev for CGO (required by go-sqlite3)
RUN apk add --no-cache gcc musl-dev

# Copy dependency manifests
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build a statically linked binary
RUN CGO_ENABLED=1 GOOS=linux go build -a -o openclaw ./cmd/openclaw

# Final minimal image
FROM alpine:latest

WORKDIR /app

# Install only the runtime libraries required by the Go binary.
RUN apk --no-cache add ca-certificates sqlite-libs

# Copy the compiled binary from the builder
COPY --from=builder /app/openclaw .

# The sqlite databases and config files should be mounted here
VOLUME /app/data

EXPOSE 18789

CMD ["./openclaw"]
