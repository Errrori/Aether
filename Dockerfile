# Stage 1: Build
FROM golang:1.25-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o aether ./cmd/aether

# Stage 2: Runtime
FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /build/aether /usr/local/bin/aether

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/aether", "-config", "/etc/aether/config.yaml"]
