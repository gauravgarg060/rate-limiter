FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ratelimiter .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates curl && addgroup -S app && adduser -S -G app app
COPY --from=build /out/ratelimiter /usr/local/bin/ratelimiter
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/ratelimiter"]
