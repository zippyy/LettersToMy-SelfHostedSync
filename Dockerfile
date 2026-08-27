FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /letters2my-sync

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /letters2my-sync /letters2my-sync
EXPOSE 8080
ENTRYPOINT ["/letters2my-sync"]