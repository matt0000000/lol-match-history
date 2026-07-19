# LoL Match History Design

## Goal

Serve a small public League of Legends match-history page for Riot IDs, with live Riot API data and a deliberately old-school server-rendered interface.

## Architecture

One Go binary owns configuration, Riot API access, view-model conversion, HTML rendering, and static-file serving. There is no database, authentication, client-side application, or build-time frontend dependency. `GET /?q=GameName%23Tag&region=na1` performs a live lookup; an empty query renders only the search page.

The Riot client converts the selected platform route (for example `na1`) to its regional route (`americas`, `europe`, `asia`, or `sea`). Account-V1 and Match-V5 use the regional route; Summoner-V4 uses the platform route. Requests carry `X-Riot-Token` and use a bounded HTTP timeout.

## Data flow

1. Validate the region allowlist and split the Riot ID at its final `#`.
2. Resolve Riot ID to PUUID with Account-V1.
3. Fetch the Summoner-V4 profile and recent Match-V5 IDs.
4. Fetch match details concurrently with a small fixed concurrency limit while retaining API order.
5. Convert Riot payloads into the agreed `PageData`, `ProfileView`, `MatchView`, and `ParticipantView` types.
6. Render `web/templates/layout.tmpl` using `ExecuteTemplate` and serve `web/static/` under `/static/`.

Participant Riot IDs come directly from Match-V5's `riotIdGameName` and `riotIdTagline` fields. Asset URLs use Data Dragon. The match's `gameVersion` selects champion, item, and summoner-spell asset versions; profile icons use the current configured Data Dragon version because Summoner-V4 does not provide a game version.

## Errors

Malformed Riot IDs and unsupported regions produce actionable form errors without calling Riot. Riot 404 maps to “player not found”; 401/403 maps to an expired or invalid API key; 429 maps to a rate-limit message and honors `Retry-After` in the text when supplied. Other upstream failures produce a generic temporary error and are logged server-side without exposing the API key or response internals.

## Verification

`httptest` fake Riot endpoints verify routing, escaping, authentication headers, payload conversion, match ordering, and error mapping. Handler tests verify empty searches and successful/error page data. `go test ./...`, `go vet ./...`, a local server smoke test, and a Docker build (when Docker is available) are the completion checks.

## Scope

No persistence, caching, authentication, pagination, JavaScript framework, ranked-league lookup, or production Riot API key is included.
