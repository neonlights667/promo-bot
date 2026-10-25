FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bot ./cmd/bot

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 bot && mkdir -p /app/data && chown bot:bot /app/data
WORKDIR /app
COPY --from=build /bot /app/bot
USER bot
EXPOSE 8080
ENTRYPOINT ["/app/bot"]
