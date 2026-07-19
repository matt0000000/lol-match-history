FROM golang:1.26.5-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY web ./web
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/lol-history .

FROM alpine:3.23

RUN apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S -G app app
COPY --from=build /out/lol-history /usr/local/bin/lol-history

USER app
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/lol-history"]
