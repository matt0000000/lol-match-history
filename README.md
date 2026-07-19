# LoL Match History

A small, server-rendered League of Legends match-history site. It fetches live data from Riot on every search and stores nothing.

## Run with Docker Compose

Get a development API key from the [Riot Developer Portal](https://developer.riotgames.com/), then:

```sh
cp .env.example .env   # edit .env and paste your key in
docker compose up --build
```

Open <http://localhost:8080> and search using a Riot ID such as `Name#Tag`.

Development keys expire every 24 hours. When Riot reports that the key is invalid or expired, generate another key, update `.env`, and run `docker compose up --build` again (or `docker compose restart` if you only changed the key and not the code).

## Run from source

Go 1.26 or newer is required.

```sh
RIOT_API_KEY=RGAPI-your-key go run .
```

Tests use local fake Riot endpoints and do not need an API key:

```sh
go test ./...
```
