FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /claude-bridge ./cmd/claude-bridge

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /claude-bridge /claude-bridge
EXPOSE 8787
ENTRYPOINT ["/claude-bridge"]
