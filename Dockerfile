# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o bin/conversation-chat ./cmd/server/main.go

# Run stage
FROM alpine:3.20

WORKDIR /app

COPY --from=builder /app/bin/conversation-chat .

EXPOSE 8082

CMD ["./conversation-chat"]
