# LoL Match History Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build and verify the Go backend and container for the approved live Riot match-history page.

**Architecture:** A single dependency-free Go HTTP service calls Riot Account-V1, Summoner-V4, and Match-V5, converts responses into template-specific view models, and renders HTML. Tests replace Riot hosts with `httptest` servers.

**Tech Stack:** Go standard library, `html/template`, Docker, Riot API, Data Dragon.

## Global Constraints

- Keep the exact exported template view-model field names in the approved contract.
- Use `GET /`, `/static/`, `RIOT_API_KEY`, and `PORT`.
- No database, auth, frontend framework, JavaScript build, or non-standard Go dependency.
- Do not commit or perform any other git write operation; workspace instructions delegate those to Claude.

---

### Task 1: Riot client contracts and errors

**Files:** Create `go.mod`, `main_test.go`, and `main.go`.

**Interfaces:** Produce `RiotClient`, `LookupAccount`, `LookupSummoner`, `ListMatchIDs`, `LookupMatch`, typed upstream errors, platform-to-regional routing, and Riot DTOs.

- [ ] Write fake-server tests for escaped Riot IDs, correct regional/platform routing, `X-Riot-Token`, and 401/403/404/429 mapping.
- [ ] Run `go test ./...` and confirm failures because the client is absent.
- [ ] Implement the minimal client and DTOs.
- [ ] Run `go test ./...` and confirm the client tests pass.

### Task 2: View-model conversion

**Files:** Modify `main_test.go` and `main.go`.

**Interfaces:** Produce the approved `PageData`, `ProfileView`, `MatchView`, and `ParticipantView` types plus conversion helpers for duration, relative time, queue labels, KDA, teams, spells, and seven item slots.

- [ ] Add failing conversion tests using representative Match-V5 JSON.
- [ ] Verify the expected failures.
- [ ] Implement only the conversion behavior specified by the tests.
- [ ] Run the full suite and refactor while green.

### Task 3: HTTP application

**Files:** Modify `main_test.go` and `main.go`; consume `web/templates/layout.tmpl`, `web/templates/index.tmpl`, and `web/static/style.css` supplied by Claude.

**Interfaces:** Produce `App.Handler()` with `GET /` and `/static/`, input validation, live lookup orchestration, and template execution.

- [ ] Add failing handler tests for empty query, invalid Riot ID/region, success, and user-facing upstream errors.
- [ ] Verify the expected failures.
- [ ] Implement routing, orchestration, logging, and rendering.
- [ ] Run all tests and a local `curl` smoke test.

### Task 4: Container and final verification

**Files:** Create `Dockerfile` and `.dockerignore`; modify `README.md` if present.

**Interfaces:** Produce a multi-stage image that runs the single binary as a non-root user and exposes `PORT=8080`.

- [ ] Add the minimal multi-stage Dockerfile and ignore file.
- [ ] Run `gofmt`, `go test ./...`, `go vet ./...`, and `go build ./...`.
- [ ] Build and smoke-test the Docker image when Docker is available; otherwise report that exact environment limitation.
- [ ] Review `git diff` without committing and hand the changes to Claude for cross-review and git writes.

### Task 5: Full match scoreboard

**Files:** Modify `main_test.go` and `main.go`; consume `web/templates/match.tmpl` and updated `web/templates/index.tmpl` supplied by Claude.

**Interfaces:** Produce `MatchDetailView`, `TeamView`, `PlayerStatsView`, `RiotClient.MatchDetail`, and `GET /match/{id}`. `MatchDetailView` also carries `Query`, `Region`, and `Error` for the shared topbar. The raw ID prefix selects the regional route and optional `me` highlights an exact Riot ID match.

- [ ] Add failing tests for match-prefix routing, team ordering, all-player stat/asset conversion, optional highlight, invalid IDs, and handler template execution.
- [ ] Verify the expected compile failures.
- [ ] Implement the DTO additions, detail conversion, client method, and handler.
- [ ] Run formatting, all tests, vet, build, and local HTTP smoke checks.

### Task 6: Cached profile snapshots and ranked stats

**Files:** Modify `main_test.go` and `main.go`; consume updated `web/templates/index.tmpl` and CSS supplied by Claude.

**Interfaces:** Add `PageData.LastUpdatedLabel`, `ProfileView.SoloRank`, `ProfileView.FlexRank`, `RankView`, League-V4 DTO conversion, and a mutex-protected per-player cache owned by `App`. A normal cache hit performs no Riot calls; `refresh=1` bypasses it and atomically replaces the snapshot only after complete success.

- [ ] Add failing tests for League-V4 routing/filtering/win-rate conversion and unranked queues.
- [ ] Add failing handler tests for case-insensitive cache hits, refresh overwrite, and refresh failure preserving/rendering the last-good snapshot.
- [ ] Verify failures are caused by missing cache/rank behavior.
- [ ] Implement the cache, handler orchestration, League-V4 call, and view conversion.
- [ ] Run formatting, all tests, vet, build, and compiled-handler smoke checks.
