FROM golang:alpine AS builder
WORKDIR /app
COPY go.mod go.sum index.html main.go ./
RUN go mod download

RUN CGO_ENABLED=0 GOOS=linux go build -o /app/server ./main.go
FROM alpine:latest
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
USER appuser
WORKDIR /app
COPY --from=builder /app/server .
COPY --from=builder /app/index.html .
EXPOSE 8080
CMD ["./server"]