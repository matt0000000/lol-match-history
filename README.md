# LoL Match History

A small, server-rendered League of Legends match-history site. It fetches live data from Riot on every search and stores nothing.

## Run with Docker

Get a development API key from the [Riot Developer Portal](https://developer.riotgames.com/), then run:

```sh
docker build -t lol-history .
docker run --rm -p 8080:8080 -e RIOT_API_KEY=RGAPI-your-key lol-history
```

Open <http://localhost:8080> and search using a Riot ID such as `Name#Tag`.

Development keys expire every 24 hours. When Riot reports that the key is invalid or expired, generate another key and restart the container with the new `RIOT_API_KEY` value.

Set `PORT` to change the listen port inside the container.

## Run from source

Go 1.26 or newer is required.

```sh
RIOT_API_KEY=RGAPI-your-key go run .
```

Tests use local fake Riot endpoints and do not need an API key:

```sh
go test ./...
```
