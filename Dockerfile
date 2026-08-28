FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /letters2my-sync .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
RUN addgroup -S letterstomy && adduser -S -G letterstomy letterstomy \
    && mkdir -p /data \
    && chown -R letterstomy:letterstomy /data
COPY --from=builder /letters2my-sync /letters2my-sync
USER letterstomy
EXPOSE 8080
ENTRYPOINT ["/letters2my-sync"]
