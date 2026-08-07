package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRiotVerificationRoute(t *testing.T) {
	app := &App{}
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/riot.txt", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != riotVerificationToken {
		t.Fatalf("GET /riot.txt: status=%d body=%q, want %q", rr.Code, rr.Body.String(), riotVerificationToken)
	}
}

func TestStaticAssetsRequireRevalidation(t *testing.T) {
	staticFiles, err := fs.Sub(webFiles, "web/static")
	if err != nil {
		t.Fatal(err)
	}
	app := &App{StaticFS: staticFiles}
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/style.css", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-cache, must-revalidate" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if !strings.Contains(rr.Body.String(), ":root") {
		t.Fatal("static response did not contain stylesheet")
	}
}

func TestParseRiotID(t *testing.T) {
	gameName, tagLine, err := parseRiotID("Hide on bush#KR1")
	if err != nil || gameName != "Hide on bush" || tagLine != "KR1" {
		t.Fatalf("parseRiotID() = %q, %q, %v", gameName, tagLine, err)
	}
	if _, _, err := parseRiotID("missing-tag"); err == nil {
		t.Fatal("parseRiotID() accepted an ID without #tag")
	}
}

func TestNewRiotClientRequestsTwentyMatches(t *testing.T) {
	client := NewRiotClient("key")
	if got := client.MatchCount; got != 20 {
		t.Fatalf("MatchCount = %d, want 20", got)
	}
	if client.MinRequestInterval < 50*time.Millisecond {
		t.Fatalf("MinRequestInterval = %v, want pacing below 20 requests/second", client.MinRequestInterval)
	}
}

func TestRequestPacingIsSharedPerHost(t *testing.T) {
	client := &RiotClient{MinRequestInterval: 15 * time.Millisecond}
	if err := client.waitForRequestSlot(context.Background(), "americas.api.riotgames.com"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := client.waitForRequestSlot(context.Background(), "americas.api.riotgames.com"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Fatalf("same-host request waited only %v", elapsed)
	}
	if err := client.waitForRequestSlot(context.Background(), "europe.api.riotgames.com"); err != nil {
		t.Fatal(err)
	}
	if len(client.nextRequest) != 2 {
		t.Fatalf("paced hosts = %d, want independent slots for two hosts", len(client.nextRequest))
	}
}

func TestRiotClientRoutesAndBuildsMatchView(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.Header.Get("X-Riot-Token") != "test-key" {
			t.Fatalf("X-Riot-Token = %q", r.Header.Get("X-Riot-Token"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/riot/account/v1/accounts/by-riot-id/"):
			w.Write([]byte(`{"puuid":"player-puuid","gameName":"Hide on bush","tagLine":"KR1"}`))
		case strings.HasPrefix(r.URL.Path, "/lol/summoner/v4/summoners/by-puuid/"):
			w.Write([]byte(`{"profileIconId":4568,"summonerLevel":777}`))
		case strings.HasPrefix(r.URL.Path, "/lol/league/v4/entries/by-puuid/"):
			w.Write([]byte(`[
				{"queueType":"RANKED_SOLO_5x5","tier":"GOLD","rank":"II","leaguePoints":54,"wins":13,"losses":7},
				{"queueType":"RANKED_FLEX_SR","tier":"MASTER","rank":"I","leaguePoints":102,"wins":2,"losses":1}
			]`))
		case strings.HasSuffix(r.URL.Path, "/ids"):
			w.Write([]byte(`["KR_1"]`))
		case strings.HasSuffix(r.URL.Path, "/timeline"):
			w.Write([]byte(timelineFixtureJSON))
		case strings.HasSuffix(r.URL.Path, "/KR_1"):
			w.Write([]byte(matchFixtureJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestRiotClient(server.URL)
	profile, matches, err := client.Search(context.Background(), "Hide on bush#KR1", "kr", time.UnixMilli(1_720_003_600_000))
	if err != nil {
		t.Fatal(err)
	}
	if profile.GameName != "Hide on bush" || profile.SummonerLevel != 777 {
		t.Fatalf("profile = %#v", profile)
	}
	if profile.PUUID != "player-puuid" {
		t.Fatalf("profile PUUID = %q", profile.PUUID)
	}
	if profile.SoloRank == nil || profile.SoloRank.Tier != "GOLD" || profile.SoloRank.Division != "II" || profile.SoloRank.LeaguePoints != 54 || profile.SoloRank.Wins != 13 || profile.SoloRank.Losses != 7 || profile.SoloRank.WinRatePercent != 65 {
		t.Fatalf("solo rank = %#v", profile.SoloRank)
	}
	if profile.FlexRank == nil || profile.FlexRank.Tier != "MASTER" || profile.FlexRank.Division != "" || profile.FlexRank.WinRatePercent != 67 {
		t.Fatalf("flex rank = %#v", profile.FlexRank)
	}
	if len(matches) != 1 {
		t.Fatalf("len(matches) = %d", len(matches))
	}
	m := matches[0]
	if !m.Win || m.GameModeLabel != "Ranked Solo/Duo" || m.DurationLabel != "32m 14s" || m.TimeAgoLabel != "1 hour ago" {
		t.Fatalf("match labels = %#v", m)
	}
	if m.ChampionName != "Ahri" || m.Kills != 10 || m.Deaths != 2 || m.Assists != 8 {
		t.Fatalf("player stats = %#v", m)
	}
	if m.RoleLabel != "Mid" {
		t.Fatalf("role = %q, want Mid", m.RoleLabel)
	}
	if m.CS != 201 || m.Gold != 12345 {
		t.Fatalf("list economy stats = CS %d, Gold %d", m.CS, m.Gold)
	}
	if m.LaneMinionsFirst10Minutes == nil || *m.LaneMinionsFirst10Minutes != 73 {
		t.Fatalf("list 10m CS = %v, want pointer to 73", m.LaneMinionsFirst10Minutes)
	}
	if m.CSPerMinute == nil || math.Abs(*m.CSPerMinute-6.2) > 0.001 {
		t.Fatalf("list CS/min = %v, want 6.2", m.CSPerMinute)
	}
	if m.KillParticipationPercent == nil || *m.KillParticipationPercent != 100 {
		t.Fatalf("list KP = %v, want capped 100%%", m.KillParticipationPercent)
	}
	if m.GoldDeltaAt15 == nil || *m.GoldDeltaAt15 != 500 || m.XPDeltaAt15 == nil || *m.XPDeltaAt15 != 500 || m.CSDeltaAt15 == nil || *m.CSDeltaAt15 != 15 {
		t.Fatalf("list @15 deltas = gold %v, XP %v, CS %v", m.GoldDeltaAt15, m.XPDeltaAt15, m.CSDeltaAt15)
	}
	if m.DurationSeconds != 1934 {
		t.Fatalf("DurationSeconds = %v, want 1934", m.DurationSeconds)
	}
	if len(m.ItemIconURLs) != 7 || m.ItemIconURLs[2] != "" || len(m.SummonerSpellIconURLs) != 2 {
		t.Fatalf("asset slots = %#v / %#v", m.ItemIconURLs, m.SummonerSpellIconURLs)
	}
	if _, err := client.MatchDetail(context.Background(), "KR_1", "Hide on bush#KR1", time.Now()); err != nil {
		t.Fatalf("cached MatchDetail failed: %v", err)
	}
	if len(paths) != 6 || !strings.Contains(paths[0], "Hide%20on%20bush/KR1") || !strings.Contains(paths[2], "/lol/league/v4/entries/by-puuid/") || !strings.Contains(paths[3], "start=0&count=10") || !strings.HasSuffix(paths[5], "/KR_1/timeline") {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestLiveGameUsesCachedSpectatorGameAndLoadsTenRankEntries(t *testing.T) {
	participants := make([]spectatorParticipantDTO, 10)
	for i := range participants {
		participants[i] = spectatorParticipantDTO{
			PUUID:      fmt.Sprintf("puuid-%d", i+1),
			RiotID:     fmt.Sprintf("Player %d#NA1", i+1),
			ChampionID: 103,
			TeamID:     100 + (i/5)*100,
		}
	}
	game := spectatorGameDTO{GameID: 987, GameLength: -4, GameQueueConfigID: 420, Participants: participants}
	var mu sync.Mutex
	spectatorCalls, leagueCalls, catalogCalls := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/lol/spectator/v5/active-games/by-summoner/"):
			mu.Lock()
			spectatorCalls++
			mu.Unlock()
			json.NewEncoder(w).Encode(game)
		case strings.Contains(r.URL.Path, "/lol/league/v4/entries/by-puuid/"):
			mu.Lock()
			leagueCalls++
			mu.Unlock()
			w.Write([]byte(`[
				{"queueType":"RANKED_SOLO_5x5","tier":"PLATINUM","rank":"III","leaguePoints":72,"wins":24,"losses":16},
				{"queueType":"RANKED_FLEX_SR","tier":"SILVER","rank":"I","leaguePoints":20,"wins":5,"losses":5}
			]`))
		case strings.HasSuffix(r.URL.Path, "/data/en_US/champion.json"):
			if token := r.Header.Get("X-Riot-Token"); token != "" {
				t.Errorf("Data Dragon request leaked Riot token %q", token)
			}
			mu.Lock()
			catalogCalls++
			mu.Unlock()
			w.Write([]byte(`{"data":{"Ahri":{"id":"Ahri","key":"103","name":"Ahri"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestRiotClient(server.URL)
	client.DataDragonBase = server.URL
	active, err := client.HasLiveGame(context.Background(), "na1", "puuid-1")
	if err != nil || !active {
		t.Fatalf("HasLiveGame() = %v, %v", active, err)
	}
	view, err := client.LoadLiveGame(context.Background(), "na1", "puuid-1")
	if err != nil {
		t.Fatal(err)
	}
	if view.GameID != 987 || view.QueueLabel != "Ranked Solo/Duo" || view.RankQueueLabel != "Solo/Duo" {
		t.Fatalf("live game labels = %#v", view)
	}
	if view.GameLengthSeconds != 0 || view.GameLengthLabel != "0m 00s" {
		t.Fatalf("clamped game length = %d / %q", view.GameLengthSeconds, view.GameLengthLabel)
	}
	if len(view.Team1.Players) != 5 || len(view.Team2.Players) != 5 {
		t.Fatalf("team sizes = %d / %d", len(view.Team1.Players), len(view.Team2.Players))
	}
	owner := view.Team1.Players[0]
	if !owner.IsSearchedPlayer || owner.RiotID != "Player 1#NA1" || owner.ChampionName != "Ahri" {
		t.Fatalf("owner = %#v", owner)
	}
	if owner.Rank == nil || owner.Rank.Tier != "PLATINUM" || owner.Rank.Wins != 24 || owner.Rank.Losses != 16 || owner.Rank.WinRatePercent != 60 {
		t.Fatalf("owner rank = %#v", owner.Rank)
	}
	mu.Lock()
	defer mu.Unlock()
	if spectatorCalls != 1 || leagueCalls != 10 || catalogCalls != 1 {
		t.Fatalf("request counts: spectator=%d league=%d catalog=%d", spectatorCalls, leagueCalls, catalogCalls)
	}
}

func TestLiveGame404MeansNotActive(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	client := newTestRiotClient(server.URL)

	active, err := client.HasLiveGame(context.Background(), "kr", "not-playing")
	if err != nil || active {
		t.Fatalf("HasLiveGame() = %v, %v, want false, nil", active, err)
	}
	if _, err := client.LoadLiveGame(context.Background(), "kr", "not-playing"); !errors.Is(err, errNotInLiveGame) {
		t.Fatalf("LoadLiveGame() error = %v, want %v", err, errNotInLiveGame)
	}
}

func TestLiveGameAlwaysUsesSoloQueueRanks(t *testing.T) {
	leagueCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/lol/spectator/"):
			w.Write([]byte(`{"gameId":1,"gameLength":30,"gameQueueConfigId":450,"participants":[{"puuid":"p1","riotId":"ARAM#NA1","championId":103,"teamId":100}]}`))
		case strings.Contains(r.URL.Path, "/lol/league/"):
			leagueCalls++
			w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/data/en_US/champion.json"):
			w.Write([]byte(`{"data":{"Ahri":{"id":"Ahri","key":"103","name":"Ahri"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestRiotClient(server.URL)
	client.DataDragonBase = server.URL

	view, err := client.LoadLiveGame(context.Background(), "na1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if view.QueueLabel != "ARAM" || view.RankQueueLabel != "Solo/Duo" || leagueCalls != 1 {
		t.Fatalf("live view = %#v, league calls = %d", view, leagueCalls)
	}
}

func TestRiotClientMapsUpstreamErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusNotFound, "No player found"},
		{http.StatusUnauthorized, "API key is invalid or expired"},
		{http.StatusForbidden, "API key is invalid or expired"},
		{http.StatusTooManyRequests, "rate limit"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "12")
				w.WriteHeader(tc.status)
			}))
			defer s.Close()
			client := newTestRiotClient(s.URL)
			_, _, err := client.Search(context.Background(), "Faker#KR1", "kr", time.Now())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestHandlerRendersEmptyAndSuccessfulSearch(t *testing.T) {
	tmpl := template.Must(template.New("layout").Parse(`{{define "layout"}}{{.Query}}|{{.Region}}|{{.Error}}{{if .Profile}}|{{.Profile.GameName}}|{{len .Matches}}{{end}}{{end}}`))
	app := &App{Templates: tmpl, Searcher: stubSearcher{}}

	for _, tc := range []struct {
		target, want string
	}{
		{"/", "|na1|"},
		{"/?q=Faker%23KR1&region=kr", "Faker#KR1|kr||Faker|1"},
		{"/?q=bad&region=kr", "bad|kr|Enter a Riot ID"},
		{"/?q=Faker%23KR1&region=bad", "Faker#KR1|bad|Choose a supported region"},
	} {
		rr := httptest.NewRecorder()
		app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.target, nil))
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), tc.want) {
			t.Fatalf("GET %s: status=%d body=%q, want %q", tc.target, rr.Code, rr.Body.String(), tc.want)
		}
	}
}

func TestHandlerCachesCaseInsensitivelyAndPreservesSnapshotOnRefreshFailure(t *testing.T) {
	tmpl := template.Must(template.New("layout").Parse(`{{define "layout"}}{{.Error}}|{{.LastUpdatedLabel}}|{{if .Profile}}{{.Profile.GameName}}{{end}}|{{len .Matches}}{{end}}`))
	searcher := &controlledSearcher{}
	clock := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	app := &App{
		Templates: tmpl,
		Searcher:  searcher,
		Cache:     NewSearchCache(),
		Now:       func() time.Time { return clock },
	}

	request := func(target string) string {
		rr := httptest.NewRecorder()
		app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d", target, rr.Code)
		}
		return rr.Body.String()
	}

	if body := request("/?q=faker%23kr1&region=kr"); body != "|Updated just now|Faker|1" || searcher.calls != 1 {
		t.Fatalf("cache miss: body=%q calls=%d", body, searcher.calls)
	}
	if body := request("/?q=Faker%23KR1&region=kr"); body != "|Updated just now|Faker|1" || searcher.calls != 1 {
		t.Fatalf("case-insensitive hit: body=%q calls=%d", body, searcher.calls)
	}

	clock = clock.Add(5 * time.Minute)
	searcher.err = errors.New("Riot API rate limit reached.")
	if body := request("/?q=Faker%23KR1&region=kr&refresh=1"); body != "Riot API rate limit reached.|Updated 5 minutes ago|Faker|1" || searcher.calls != 2 {
		t.Fatalf("failed refresh: body=%q calls=%d", body, searcher.calls)
	}
	if body := request("/?q=FAKER%23KR1&region=kr"); body != "|Updated 5 minutes ago|Faker|1" || searcher.calls != 2 {
		t.Fatalf("preserved hit: body=%q calls=%d", body, searcher.calls)
	}
}

func TestHandlerSuccessfulRefreshReplacesCachedSnapshot(t *testing.T) {
	tmpl := template.Must(template.New("layout").Parse(`{{define "layout"}}{{.LastUpdatedLabel}}|{{.Profile.GameName}}{{end}}`))
	searcher := &controlledSearcher{profileName: "Old"}
	clock := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	app := &App{Templates: tmpl, Searcher: searcher, Cache: NewSearchCache(), Now: func() time.Time { return clock }}

	request := func(target string) string {
		rr := httptest.NewRecorder()
		app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
		return rr.Body.String()
	}
	if body := request("/?q=Faker%23KR1&region=kr"); body != "Updated just now|Old" {
		t.Fatalf("initial body = %q", body)
	}
	clock = clock.Add(5 * time.Minute)
	searcher.profileName = "Fresh"
	if body := request("/?q=Faker%23KR1&region=kr&refresh=1"); body != "Updated just now|Fresh" || searcher.calls != 2 {
		t.Fatalf("refresh: body=%q calls=%d", body, searcher.calls)
	}
	clock = clock.Add(time.Minute)
	if body := request("/?q=faker%23kr1&region=kr"); body != "Updated 1 minute ago|Fresh" || searcher.calls != 2 {
		t.Fatalf("replacement hit: body=%q calls=%d", body, searcher.calls)
	}
}

func TestHandlerLoadsTenMoreMatchesAndCachesExpandedResult(t *testing.T) {
	tmpl := template.Must(template.New("layout").Parse(`{{define "layout"}}{{len .Matches}}|{{.RequestedMatches}}|{{.NextMatchCount}}|{{.CanLoadMore}}{{end}}`))
	searcher := &expandingSearcher{}
	app := &App{Templates: tmpl, Searcher: searcher, Cache: NewSearchCache()}

	request := func(target string) string {
		rr := httptest.NewRecorder()
		app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d", target, rr.Code)
		}
		return rr.Body.String()
	}
	if body := request("/?q=Faker%23KR1&region=kr"); body != "20|20|30|true" {
		t.Fatalf("initial body = %q", body)
	}
	if body := request("/?q=faker%23kr1&region=kr"); body != "20|20|30|true" {
		t.Fatalf("cached body = %q", body)
	}
	if body := request("/?q=Faker%23KR1&region=kr&count=30"); body != "30|30|40|true" {
		t.Fatalf("expanded body = %q", body)
	}
	if body := request("/?q=FAKER%23KR1&region=kr&count=30"); body != "30|30|40|true" {
		t.Fatalf("cached expanded body = %q", body)
	}
	if body := request("/?q=Faker%23KR1&region=kr"); body != "30|30|40|true" {
		t.Fatalf("expanded cache lost its requested count: %q", body)
	}
	if len(searcher.counts) != 2 || searcher.counts[0] != 20 || searcher.counts[1] != 30 {
		t.Fatalf("requested counts = %#v, want [20 30]", searcher.counts)
	}
}

func TestRequestedMatchCountDefaultsAndCaps(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{{"", 20}, {"bad", 20}, {"10", 20}, {"30", 30}, {"999", 100}} {
		if got := requestedMatchCount(tc.raw); got != tc.want {
			t.Fatalf("requestedMatchCount(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestRecentSummaryUsesAvailableLastTwentyMatchStats(t *testing.T) {
	csSix, csEight := 6.0, 8.0
	goldOne, goldTwo := 300, -100
	xpOne, xpTwo := 200, -50
	csOne, csTwo := 12, -4
	matches := []MatchView{
		{Win: true, ChampionName: "Ahri", ChampionIconURL: "ahri.png", RoleLabel: "Mid", Kills: 10, Deaths: 2, Assists: 8, CSPerMinute: &csSix, GoldDeltaAt15: &goldOne, XPDeltaAt15: &xpOne, CSDeltaAt15: &csOne, DurationSeconds: 1200},
		{Win: false, ChampionName: "Lux", RoleLabel: "Support", Kills: 2, Deaths: 4, Assists: 6, CSPerMinute: &csEight, GoldDeltaAt15: &goldTwo, XPDeltaAt15: &xpTwo, CSDeltaAt15: &csTwo, DurationSeconds: 1800},
		{Win: true, ChampionName: "Ahri", RoleLabel: "Mid", Kills: 6, Deaths: 0, Assists: 4, DurationSeconds: 600},
	}

	got := recentSummary(matches)
	if got == nil {
		t.Fatal("recentSummary returned nil")
	}
	if got.Games != 3 {
		t.Fatalf("Games = %d, want 3", got.Games)
	}
	if got.AverageGoldDeltaAt15 == nil || math.Abs(*got.AverageGoldDeltaAt15-100) > 0.001 {
		t.Fatalf("AverageGoldDeltaAt15 = %v, want 100", got.AverageGoldDeltaAt15)
	}
	if got.AverageXPDeltaAt15 == nil || math.Abs(*got.AverageXPDeltaAt15-75) > 0.001 {
		t.Fatalf("AverageXPDeltaAt15 = %v, want 75", got.AverageXPDeltaAt15)
	}
	if got.AverageCSDeltaAt15 == nil || math.Abs(*got.AverageCSDeltaAt15-4) > 0.001 {
		t.Fatalf("AverageCSDeltaAt15 = %v, want 4", got.AverageCSDeltaAt15)
	}
	if got.DeathsPer10Minutes == nil || math.Abs(*got.DeathsPer10Minutes-1) > 0.001 {
		t.Fatalf("DeathsPer10Minutes = %v, want 1", got.DeathsPer10Minutes)
	}
	if len(got.Champions) != 2 || got.Champions[0].ChampionName != "Ahri" || got.Champions[0].Games != 2 || got.Champions[0].WinRatePercent != 100 || got.Champions[0].ChampionIconURL != "ahri.png" {
		t.Fatalf("champion summaries = %#v", got.Champions)
	}
	if len(got.Roles) != 2 || got.Roles[0].Role != "Mid" || got.Roles[0].Games != 2 || got.Roles[0].WinRatePercent != 100 {
		t.Fatalf("role summaries = %#v", got.Roles)
	}
}

func TestDerivePerformanceLabelsReturnsEveryMatchingCategory(t *testing.T) {
	eighty, sixty := 80, 60
	for _, tc := range []struct {
		name         string
		player       participantDTO
		participants []participantDTO
		wantLabel    string
		wantTone     string
	}{
		{"lane bully", participantDTO{TeamID: 100, TeamPosition: "MIDDLE", Challenges: participantChallengesDTO{LaneMinionsFirst10Minutes: &eighty}}, []participantDTO{{TeamID: 200, TeamPosition: "MIDDLE", Challenges: participantChallengesDTO{LaneMinionsFirst10Minutes: &sixty}}}, "lane bully", "good"},
		{"farm machine", participantDTO{TeamID: 100, TeamPosition: "TOP", TotalMinionsKilled: 240}, nil, "farm machine", "good"},
		{"everywhere", participantDTO{PUUID: "me", TeamID: 100, Kills: 7, Assists: 1}, []participantDTO{{PUUID: "me", TeamID: 100, Kills: 7, Assists: 1}, {TeamID: 100, Kills: 3}}, "everywhere", "good"},
		{"rough game", participantDTO{TeamID: 100, Deaths: 10}, nil, "rough game", "bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			participants := tc.participants
			if len(participants) == 0 {
				participants = []participantDTO{tc.player}
			} else if tc.player.PUUID == "" {
				participants = append([]participantDTO{tc.player}, participants...)
			}
			labels := derivePerformanceLabels(tc.player, participants, 1800)
			if !hasPerformanceLabel(labels, tc.wantLabel, tc.wantTone) {
				t.Fatalf("derivePerformanceLabels() = %#v, want %q/%q", labels, tc.wantLabel, tc.wantTone)
			}
			for _, label := range labels {
				if label.Description == "" {
					t.Fatalf("label %q has no hover description", label.Text)
				}
			}
		})
	}
}

func hasPerformanceLabel(labels []PerformanceLabelView, text, tone string) bool {
	for _, label := range labels {
		if label.Text == text && label.Tone == tone {
			return true
		}
	}
	return false
}

func TestDerivePerformanceLabelsTreatsMissingOptionalSignalsAsUnknown(t *testing.T) {
	player := participantDTO{TeamID: 100, TeamPosition: "TOP", VisionScore: 0}
	labels := derivePerformanceLabels(player, []participantDTO{player}, 1800)
	for _, unwanted := range []string{"poor vision", "solo killer", "low damage"} {
		if hasPerformanceLabel(labels, unwanted, "bad") || hasPerformanceLabel(labels, unwanted, "good") {
			t.Fatalf("missing data produced %q in %#v", unwanted, labels)
		}
	}
}

func TestNoControlWardsLabelRequiresKnownSummonersRiftRole(t *testing.T) {
	knownRole := participantDTO{TeamID: 100, TeamPosition: "UTILITY"}
	if labels := derivePerformanceLabels(knownRole, []participantDTO{knownRole}, 1800); !hasPerformanceLabel(labels, "no control wards", "bad") {
		t.Fatalf("known-role labels = %#v, want no control wards", labels)
	}
	unknownRole := participantDTO{TeamID: 100}
	if labels := derivePerformanceLabels(unknownRole, []participantDTO{unknownRole}, 1800); hasPerformanceLabel(labels, "no control wards", "bad") {
		t.Fatalf("unknown-role labels = %#v, do not label modes without control wards", labels)
	}
}

func TestRecentSummaryHandlesMissingOptionalStatsAndTiesDeterministically(t *testing.T) {
	got := recentSummary([]MatchView{
		{ChampionName: "Lux", Kills: 1},
		{ChampionName: "Ahri", Assists: 1},
	})
	if got.AverageGoldDeltaAt15 != nil || got.AverageXPDeltaAt15 != nil || got.AverageCSDeltaAt15 != nil {
		t.Fatalf("missing @15 averages = %#v", got)
	}
	if got.DeathsPer10Minutes != nil {
		t.Fatalf("missing duration death rate = %v, want nil", got.DeathsPer10Minutes)
	}
	if len(got.Champions) != 2 || got.Champions[0].ChampionName != "Lux" {
		t.Fatalf("tie winner = %#v, want first-seen Lux", got.Champions)
	}
	if recentSummary(nil) != nil {
		t.Fatal("empty summary should be nil")
	}
}

func TestEmbeddedIndexTemplateRendersRecentSummary(t *testing.T) {
	tmpl := template.Must(parseTemplates())
	cs := 7.2
	gold, xp, csDelta, deaths := 234.5, -87.5, 6.5, 1.2
	data := PageData{
		Profile: &ProfileView{},
		RecentSummary: &RecentSummaryView{
			Games: 20, AverageGoldDeltaAt15: &gold, AverageXPDeltaAt15: &xp, AverageCSDeltaAt15: &csDelta, DeathsPer10Minutes: &deaths,
			Champions: []ChampionSummaryView{{ChampionName: "Ahri", Games: 6, WinRatePercent: 67, AverageKDA: 4.2, AverageCSPerMinute: &cs}},
			Roles:     []RoleSummaryView{{Role: "Mid", Games: 15, WinRatePercent: 60}},
		},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "content", data); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"last 20 matches", "&#43;234.5", "-87.5", "&#43;6.5", "1.2", "gold diff @15", "xp diff @15", "cs diff @15", "deaths / 10m", "Ahri", "recent champions", "6 games · 67% wr", "recent roles", "15 games"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("summary does not contain %q: %s", want, buf.String())
		}
	}
	for _, old := range []string{"record", "win rate", "avg kda", "avg cs/min", "most played"} {
		if strings.Contains(buf.String(), old) {
			t.Fatalf("summary still renders old stat %q: %s", old, buf.String())
		}
	}
}

func TestEmbeddedIndexTemplateRendersLoadMoreButton(t *testing.T) {
	tmpl := template.Must(parseTemplates())
	data := PageData{Profile: &ProfileView{}, Query: "Faker#KR1", Region: "kr", RequestedMatches: 20, NextMatchCount: 30, CanLoadMore: true, SupportsExpansion: true}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "content", data); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`class="load-more-form"`, `name="count" value="30"`, `Load 10 More`} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("load-more markup does not contain %q: %s", want, buf.String())
		}
	}
}

func TestRiotClientBuildsMatchDetailFromIDPrefix(t *testing.T) {
	var requestURI string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI = r.URL.RequestURI()
		if r.Header.Get("X-Riot-Token") != "test-key" {
			t.Fatalf("X-Riot-Token = %q", r.Header.Get("X-Riot-Token"))
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/timeline") {
			w.Write([]byte(timelineFixtureJSON))
			return
		}
		w.Write([]byte(matchFixtureJSON))
	}))
	defer server.Close()

	client := newTestRiotClient(server.URL)
	detail, err := client.MatchDetail(context.Background(), "KR_1", "Hide on bush#KR1", time.UnixMilli(1_720_003_600_000))
	if err != nil {
		t.Fatal(err)
	}
	if requestURI != "/lol/match/v5/matches/KR_1/timeline" {
		t.Fatalf("request URI = %q", requestURI)
	}
	if detail.MatchID != "KR_1" || detail.GameModeLabel != "Ranked Solo/Duo" || detail.DurationLabel != "32m 14s" || detail.TimeAgoLabel != "1 hour ago" {
		t.Fatalf("detail labels = %#v", detail)
	}
	if !detail.Team1.Win || detail.Team2.Win || len(detail.Team1.Players) != 2 || len(detail.Team2.Players) != 1 {
		t.Fatalf("teams = %#v / %#v", detail.Team1, detail.Team2)
	}
	if detail.Team1.TotalKills != 11 || detail.Team1.TotalDeaths != 5 || detail.Team1.TotalAssists != 12 || detail.Team1.TotalGold != 20345 {
		t.Fatalf("team 1 totals = %#v", detail.Team1)
	}
	if detail.Team2.TotalKills != 5 || detail.Team2.TotalDeaths != 5 || detail.Team2.TotalAssists != 2 || detail.Team2.TotalGold != 11000 {
		t.Fatalf("team 2 totals = %#v", detail.Team2)
	}
	if detail.Team1.Objectives != (ObjectiveView{Towers: 9, Dragons: 3, Barons: 1, Heralds: 1, Grubs: 4}) || detail.Team2.Objectives != (ObjectiveView{Towers: 2, Dragons: 1}) {
		t.Fatalf("objectives = %#v / %#v", detail.Team1.Objectives, detail.Team2.Objectives)
	}
	p := detail.Team1.Players[0]
	if p.RiotID != "Hide on bush#KR1" || p.Region != "kr" || p.ChampionName != "Ahri" || p.ChampionLevel != 18 || p.Kills != 10 || p.Deaths != 2 || p.Assists != 8 || p.CS != 201 || p.Gold != 12345 || p.Damage != 23456 || p.DamagePercent != 59 || !p.IsHighlighted {
		t.Fatalf("player = %#v", p)
	}
	if p.LaneMinionsFirst10Minutes == nil || *p.LaneMinionsFirst10Minutes != 73 {
		t.Fatalf("searched player 10m CS = %v, want pointer to 73", p.LaneMinionsFirst10Minutes)
	}
	if p.CSPerMinute == nil || math.Abs(*p.CSPerMinute-6.2) > 0.001 {
		t.Fatalf("searched player CS/min = %v, want 6.2", p.CSPerMinute)
	}
	if p.KillParticipationPercent == nil || *p.KillParticipationPercent != 100 {
		t.Fatalf("searched player KP = %v, want capped 100%%", p.KillParticipationPercent)
	}
	if p.DamageSharePercent == nil || *p.DamageSharePercent != 70 || p.VisionScore != 50 || p.ControlWards != 3 || p.ObjectiveDamage != 12000 || p.TurretDamage != 6000 {
		t.Fatalf("searched player advanced stats = %#v", p)
	}
	for _, label := range []string{"lane bully", "everywhere", "damage carry", "objective focused", "tower pusher", "control ward buyer", "first blood", "solo killer", "triple kill"} {
		tone := "good"
		if label == "first blood" {
			tone = "neutral"
		}
		if !hasPerformanceLabel(p.PerformanceLabels, label, tone) {
			t.Fatalf("searched player labels = %#v, missing %q", p.PerformanceLabels, label)
		}
	}
	if len(p.PerformanceLabels) < 9 {
		t.Fatalf("labels were unexpectedly capped: %#v", p.PerformanceLabels)
	}
	if detail.Team1.Players[1].LaneMinionsFirst10Minutes != nil {
		t.Fatalf("ally 10m CS = %v, want nil", detail.Team1.Players[1].LaneMinionsFirst10Minutes)
	}
	if detail.Team2.Players[0].LaneMinionsFirst10Minutes == nil || *detail.Team2.Players[0].LaneMinionsFirst10Minutes != 0 {
		t.Fatalf("enemy 10m CS = %v, want pointer to 0", detail.Team2.Players[0].LaneMinionsFirst10Minutes)
	}
	if detail.Team2.Players[0].DamagePercent != 100 {
		t.Fatalf("highest damage percent = %d", detail.Team2.Players[0].DamagePercent)
	}
	if !hasPerformanceLabel(detail.Team2.Players[0].PerformanceLabels, "gave first blood", "bad") {
		t.Fatalf("first-blood victim labels = %#v", detail.Team2.Players[0].PerformanceLabels)
	}
	if !hasPerformanceLabel(detail.Team2.Players[0].PerformanceLabels, "no control wards", "bad") {
		t.Fatalf("zero-control-ward labels = %#v", detail.Team2.Players[0].PerformanceLabels)
	}
	if len(p.ItemIconURLs) != 7 || p.ItemIconURLs[2] != "" || len(p.SummonerSpellIconURLs) != 2 {
		t.Fatalf("asset slots = %#v / %#v", p.ItemIconURLs, p.SummonerSpellIconURLs)
	}
}

func TestMatchDetailCachesRawMatchButRebuildsViewerSpecificView(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/timeline") {
			w.Write([]byte(timelineFixtureJSON))
			return
		}
		w.Write([]byte(matchFixtureJSON))
	}))
	defer server.Close()
	client := newTestRiotClient(server.URL)

	first, err := client.MatchDetail(context.Background(), "KR_1", "Hide on bush#KR1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.MatchDetail(context.Background(), "kr_1", "Enemy#KR1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("Match-V5 requests = %d, want match + timeline once", requests)
	}
	if first.Team1.Players[0].RiotID != "Hide on bush#KR1" || !first.Team1.Players[0].IsHighlighted {
		t.Fatalf("first viewer Team1 = %#v", first.Team1)
	}
	if second.Team1.Players[0].RiotID != "Enemy#KR1" || !second.Team1.Players[0].IsHighlighted {
		t.Fatalf("second viewer Team1 = %#v", second.Team1)
	}
	if second.Team1.Objectives.Towers != 2 || second.Team2.Objectives.Towers != 9 {
		t.Fatalf("viewer-relative objectives = %#v / %#v", second.Team1.Objectives, second.Team2.Objectives)
	}
}

func TestLaneDeltasAt15UsesRoleAppropriateCreepScore(t *testing.T) {
	participants := []participantDTO{
		{ParticipantID: 1, TeamID: 100, TeamPosition: "MIDDLE"},
		{ParticipantID: 2, TeamID: 100, TeamPosition: "JUNGLE"},
		{ParticipantID: 6, TeamID: 200, TeamPosition: "MIDDLE"},
		{ParticipantID: 7, TeamID: 200, TeamPosition: "JUNGLE"},
	}
	var timeline timelineDTO
	frame := timelineFrameDTO{
		Timestamp: 900_000,
		ParticipantFrames: map[string]timelineParticipantFrameDTO{
			"1": {TotalGold: 6000, XP: 7000, MinionsKilled: 120, JungleMinionsKilled: 3},
			"2": {TotalGold: 5900, XP: 6800, MinionsKilled: 15, JungleMinionsKilled: 82},
			"6": {TotalGold: 5500, XP: 6500, MinionsKilled: 105, JungleMinionsKilled: 4},
			"7": {TotalGold: 6100, XP: 7000, MinionsKilled: 8, JungleMinionsKilled: 74},
		},
	}
	timeline.Info.Frames = append(timeline.Info.Frames, frame)

	gold, xp, cs := laneDeltasAt15(participants[0], participants, timeline)
	if gold == nil || *gold != 500 || xp == nil || *xp != 500 || cs == nil || *cs != 15 {
		t.Fatalf("mid deltas = gold %v, XP %v, CS %v", gold, xp, cs)
	}
	gold, xp, cs = laneDeltasAt15(participants[1], participants, timeline)
	if gold == nil || *gold != -200 || xp == nil || *xp != -200 || cs == nil || *cs != 8 {
		t.Fatalf("jungle deltas = gold %v, XP %v, jungle CS %v", gold, xp, cs)
	}
}

func TestLaneDeltasAt15RequiresAValidFrameAndUniqueOpponent(t *testing.T) {
	player := participantDTO{ParticipantID: 1, TeamID: 100, TeamPosition: "TOP"}
	opponent := participantDTO{ParticipantID: 6, TeamID: 200, TeamPosition: "TOP"}
	for _, tc := range []struct {
		name         string
		participants []participantDTO
		timestamp    int64
		frames       map[string]timelineParticipantFrameDTO
	}{
		{name: "no opponent", participants: []participantDTO{player}, timestamp: 900_000, frames: map[string]timelineParticipantFrameDTO{"1": {}}},
		{name: "ambiguous opponent", participants: []participantDTO{player, opponent, {ParticipantID: 7, TeamID: 200, TeamPosition: "TOP"}}, timestamp: 900_000, frames: map[string]timelineParticipantFrameDTO{"1": {}, "6": {}, "7": {}}},
		{name: "frame before fifteen", participants: []participantDTO{player, opponent}, timestamp: 899_999, frames: map[string]timelineParticipantFrameDTO{"1": {}, "6": {}}},
		{name: "missing participant frame", participants: []participantDTO{player, opponent}, timestamp: 900_000, frames: map[string]timelineParticipantFrameDTO{"1": {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var timeline timelineDTO
			timeline.Info.Frames = append(timeline.Info.Frames, timelineFrameDTO{Timestamp: tc.timestamp, ParticipantFrames: tc.frames})
			gold, xp, cs := laneDeltasAt15(player, tc.participants, timeline)
			if gold != nil || xp != nil || cs != nil {
				t.Fatalf("laneDeltasAt15() = %v/%v/%v, want unknown", gold, xp, cs)
			}
		})
	}
	unknown := participantDTO{ParticipantID: 1, TeamID: 100, TeamPosition: "INVALID"}
	if gold, xp, cs := laneDeltasAt15(unknown, []participantDTO{unknown, {ParticipantID: 6, TeamID: 200, TeamPosition: "INVALID"}}, timelineDTO{}); gold != nil || xp != nil || cs != nil {
		t.Fatalf("unknown-role deltas = %v/%v/%v, want unknown", gold, xp, cs)
	}
}

func TestFailedMatchDetailIsNotCached(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(matchFixtureJSON))
	}))
	defer server.Close()
	client := newTestRiotClient(server.URL)

	if _, err := client.MatchDetail(context.Background(), "KR_1", "", time.Now()); err == nil {
		t.Fatal("first MatchDetail unexpectedly succeeded")
	}
	if _, err := client.MatchDetail(context.Background(), "KR_1", "", time.Now()); err != nil {
		t.Fatalf("second MatchDetail failed: %v", err)
	}
	if requests != 3 {
		t.Fatalf("Match-V5 requests = %d, want failed match + successful match and timeline", requests)
	}
}

func TestMatchDetailStillRendersWhenTimelineIsUnavailable(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if strings.HasSuffix(r.URL.Path, "/timeline") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(matchFixtureJSON))
	}))
	defer server.Close()

	detail, err := newTestRiotClient(server.URL).MatchDetail(context.Background(), "KR_1", "Hide on bush#KR1", time.Now())
	if err != nil {
		t.Fatalf("MatchDetail failed because timeline was unavailable: %v", err)
	}
	if requests != 2 || len(detail.Team1.Players) == 0 {
		t.Fatalf("requests/detail = %d/%#v", requests, detail)
	}
}

func TestSearchStillReturnsMatchesWhenTimelineIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/riot/account/v1/accounts/by-riot-id/"):
			w.Write([]byte(`{"puuid":"player-puuid","gameName":"Hide on bush","tagLine":"KR1"}`))
		case strings.HasPrefix(r.URL.Path, "/lol/summoner/v4/summoners/by-puuid/"):
			w.Write([]byte(`{"profileIconId":4568,"summonerLevel":777}`))
		case strings.HasPrefix(r.URL.Path, "/lol/league/v4/entries/by-puuid/"):
			w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/ids"):
			w.Write([]byte(`["KR_1"]`))
		case strings.HasSuffix(r.URL.Path, "/timeline"):
			w.WriteHeader(http.StatusServiceUnavailable)
		case strings.HasSuffix(r.URL.Path, "/KR_1"):
			w.Write([]byte(matchFixtureJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, matches, err := newTestRiotClient(server.URL).Search(context.Background(), "Hide on bush#KR1", "kr", time.Now())
	if err != nil {
		t.Fatalf("Search failed because timeline was unavailable: %v", err)
	}
	if len(matches) != 1 || matches[0].GoldDeltaAt15 != nil || matches[0].XPDeltaAt15 != nil || matches[0].CSDeltaAt15 != nil {
		t.Fatalf("matches = %#v, want one match with unknown @15 stats", matches)
	}
}

func TestMatchDetailDefaultsToTeam100AndRejectsInvalidPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(matchFixtureJSON))
	}))
	defer server.Close()
	client := newTestRiotClient(server.URL)

	detail, err := client.MatchDetail(context.Background(), "KR_1", "Absent#TAG", time.UnixMilli(1_720_003_600_000))
	if err != nil {
		t.Fatal(err)
	}
	if detail.Team1.Players[0].RiotID != "Hide on bush#KR1" || detail.Team1.Players[0].IsHighlighted {
		t.Fatalf("default team/highlight = %#v", detail.Team1)
	}
	if _, err := client.MatchDetail(context.Background(), "NOPE_1", "", time.Now()); err == nil {
		t.Fatal("MatchDetail accepted unsupported match prefix")
	}
}

func TestMatchDetailMatchesRiotIDCaseInsensitively(t *testing.T) {
	var dto matchDTO
	dto.Metadata.MatchID = "KR_2"
	dto.Info.GameVersion = "16.14.1.123"
	dto.Info.Participants = []participantDTO{
		{TeamID: 100, RiotIDGameName: "Enemy", RiotIDTagLine: "KR1"},
		{TeamID: 200, Win: true, RiotIDGameName: "Hide on bush", RiotIDTagLine: "KR1"},
	}

	detail := newTestRiotClient("https://riot.test").matchDetailView(dto, "hide on bush#kr1", "kr", time.Now(), 0)
	if len(detail.Team1.Players) != 1 || detail.Team1.Players[0].RiotID != "Hide on bush#KR1" {
		t.Fatalf("Team1 = %#v, want searched player's team 200", detail.Team1)
	}
	if !detail.Team1.Players[0].IsHighlighted {
		t.Fatalf("searched player was not highlighted: %#v", detail.Team1.Players[0])
	}
}

func TestMatchDetailDamagePercentIsZeroWhenAllDamageIsZero(t *testing.T) {
	var dto matchDTO
	dto.Info.Participants = []participantDTO{
		{TeamID: 100, RiotIDGameName: "One", RiotIDTagLine: "NA1"},
		{TeamID: 200, RiotIDGameName: "Two", RiotIDTagLine: "NA1"},
	}
	detail := newTestRiotClient("https://riot.test").matchDetailView(dto, "", "na1", time.Now(), 0)
	if detail.Team1.Players[0].DamagePercent != 0 || detail.Team2.Players[0].DamagePercent != 0 {
		t.Fatalf("zero-damage percents = %d, %d", detail.Team1.Players[0].DamagePercent, detail.Team2.Players[0].DamagePercent)
	}
}

func TestMatchDetailHandler(t *testing.T) {
	tmpl := template.Must(template.New("matchLayout").Parse(`{{define "matchLayout"}}{{.MatchID}}|{{.Query}}|{{.Region}}|{{.Error}}|{{.Team1.Players  | len}}{{end}}`))
	app := &App{Templates: tmpl, MatchLoader: stubMatchLoader{}}

	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/match/KR_1?me=Faker%23KR1", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "KR_1|Faker#KR1|kr||1" {
		t.Fatalf("valid detail: status=%d body=%q", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/match/not-a-match", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "not-a-match||na1|Invalid match ID.|0" {
		t.Fatalf("invalid detail: status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestEmbeddedMatchTemplateRendersDetailHandler(t *testing.T) {
	tmpl := template.Must(parseTemplates())
	app := &App{Templates: tmpl, MatchLoader: stubMatchLoader{}}
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/match/KR_1?me=Faker%23KR1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	for _, want := range []string{"match-history", "Match History", "Faker#KR1", `href="/?q=Faker%23KR1&amp;region=kr"`, "Copy Link", "navigator.clipboard", "window.location.origin + window.location.pathname", "writeText(matchURL)"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("body does not contain %q: %s", want, rr.Body.String())
		}
	}
	if strings.Contains(rr.Body.String(), "writeText(window.location.href)") {
		t.Fatalf("copy script still copies the viewer-specific query string: %s", rr.Body.String())
	}
}

func TestEmbeddedIndexTemplateRendersRedesignedMatchStats(t *testing.T) {
	tmpl := template.Must(parseTemplates())
	app := &App{Templates: tmpl, Searcher: stubSearcher{}}
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/?q=Faker%23KR1&region=kr", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	for _, want := range []string{"123", `<span class="k">kda</span>`, `<span class="k">cs</span>`} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("body does not contain %q: %s", want, rr.Body.String())
		}
	}
	for _, unwanted := range []string{`<span class="k">gold</span>`, `<span class="k">cs/min</span>`, `<span class="k">cs@10m</span>`, `<span class="k">kp</span>`} {
		if strings.Contains(rr.Body.String(), unwanted) {
			t.Fatalf("compact history row still renders %q: %s", unwanted, rr.Body.String())
		}
	}
}

func TestProfileShowsLiveGameButtonOnlyWhenActive(t *testing.T) {
	tmpl := template.Must(parseTemplates())
	checker := &stubLiveChecker{active: true}
	app := &App{Templates: tmpl, Searcher: liveStubSearcher{}, LiveChecker: checker}
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/?q=Faker%23KR1&region=kr", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Live Game") || !strings.Contains(body, `/live/kr/player-puuid?me=Faker%23KR1`) {
		t.Fatalf("active profile missing live link: %s", body)
	}
	if checker.calls != 1 || checker.region != "kr" || checker.puuid != "player-puuid" {
		t.Fatalf("live checker = %#v", checker)
	}

	checker.active = false
	rr = httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/?q=Faker%23KR1&region=kr", nil))
	if strings.Contains(rr.Body.String(), "Live Game") {
		t.Fatalf("inactive profile rendered live link: %s", rr.Body.String())
	}
}

func TestLiveGameHandlerRendersRankedPlayers(t *testing.T) {
	tmpl := template.Must(parseTemplates())
	loader := &stubLiveLoader{view: &LiveGameView{
		QueueLabel:        "Normal Draft",
		RankQueueLabel:    "Solo/Duo",
		GameLengthSeconds: 125,
		GameLengthLabel:   "2m 05s",
		Team1: LiveTeamView{TeamID: 100, Players: []LivePlayerView{{
			RiotID: "Faker#KR1", ChampionName: "Ahri", IsSearchedPlayer: true,
			Rank: &RankView{Tier: "DIAMOND", Division: "II", LeaguePoints: 40, Wins: 60, Losses: 40, WinRatePercent: 60},
		}}},
		Team2: LiveTeamView{TeamID: 200},
	}}
	app := &App{Templates: tmpl, LiveLoader: loader}
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/live/kr/player-puuid?me=Faker%23KR1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	for _, want := range []string{"Live match", "Normal Draft", "Every player’s current", "Solo/Duo</b> season record", "blue team", "Faker#KR1", "DIAMOND II", "40 LP", `<b>60</b><small>wins</small>`, `<b>40</b><small>losses</small>`, `<b>60%</b><small>win rate</small>`, `data-live-seconds="125"`} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("live page missing %q: %s", want, rr.Body.String())
		}
	}
	if loader.region != "kr" || loader.puuid != "player-puuid" {
		t.Fatalf("loader got region=%q puuid=%q", loader.region, loader.puuid)
	}
}

func TestEmbeddedIndexTemplateOmitsAdvancedMatchStats(t *testing.T) {
	tmpl := template.Must(parseTemplates())
	seventyThree, zero, ninety := 73, 0, 90
	sixPointTwo := 6.2
	data := PageData{
		Profile: &ProfileView{},
		Matches: []MatchView{
			{LaneMinionsFirst10Minutes: &seventyThree, CSPerMinute: &sixPointTwo, KillParticipationPercent: &ninety},
			{LaneMinionsFirst10Minutes: &zero},
			{},
			{},
		},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "content", data); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, unwanted := range []string{
		`<span class="v">73</span><br><span class="k">cs@10m</span>`,
		`<span class="v">0</span><br><span class="k">cs@10m</span>`,
		`<span class="v">—</span><br><span class="k">cs@10m</span>`,
		`<span class="v">6.2</span><br><span class="k">cs/min</span>`,
		`<span class="v">—</span><br><span class="k">cs/min</span>`,
		`<span class="v">90%</span><br><span class="k">kp</span>`,
		`<span class="v">—</span><br><span class="k">kp</span>`,
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("index body unexpectedly contains %q: %s", unwanted, body)
		}
	}
	if strings.Contains(body, "performance-tag") {
		t.Fatalf("match history unexpectedly renders performance labels: %s", body)
	}
	if strings.Contains(body, "csΔ@10") {
		t.Fatalf("match history unexpectedly renders CS delta: %s", body)
	}
}

func TestEmbeddedMatchTemplateRendersLaneMinionsFirst10Minutes(t *testing.T) {
	tmpl := template.Must(parseTemplates())
	seventyThree, zero, ninety, thirtyFive := 73, 0, 90, 35
	sixPointTwo := 6.2
	data := MatchDetailView{
		GameModeLabel: "Ranked Solo/Duo",
		Team1: TeamView{Win: true, Objectives: ObjectiveView{Towers: 9, Dragons: 3, Barons: 1, Heralds: 1, Grubs: 4}, Players: []PlayerStatsView{
			{ChampionLevel: 18, LaneMinionsFirst10Minutes: &seventyThree, CSPerMinute: &sixPointTwo, KillParticipationPercent: &ninety, Gold: 12000, GoldPerMinute: &sixPointTwo, Damage: 24000, DamageSharePercent: &thirtyFive, DamagePerMinute: &sixPointTwo, VisionScore: 42, VisionPerMinute: &sixPointTwo, ControlWards: 3, ObjectiveDamage: 9000, TurretDamage: 4000, PerformanceLabels: []PerformanceLabelView{{Text: "damage carry", Tone: "good", Description: performanceLabelDescription("damage carry")}}},
			{LaneMinionsFirst10Minutes: &zero},
			{},
			{},
		}},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "matchContent", data); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, want := range []string{
		`<th>cs@10m</th>`,
		`<th>vision</th>`,
		`<span class="champ-level">18</span>`,
		`<td class="num-cell" data-label="cs@10m">73</td>`,
		`<td class="num-cell" data-label="cs@10m">0</td>`,
		`<td class="num-cell" data-label="cs@10m">—</td>`,
		`<span>0</span><small>6.2/min</small>`,
		`<span>24000</span><small>6.2/min</small>`,
		`<small>6.2/min · 3 cw</small>`,
		`title="Dealt at least 30% of the team’s champion damage.">damage carry</span>`,
		`towers <b>9</b>`,
		`dragons <b>3</b>`,
		`barons <b>1</b>`,
		`heralds <b>1</b>`,
		`grubs <b>4</b>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail body does not contain %q: %s", want, body)
		}
	}
	for _, unwanted := range []string{`<th>cs/min</th>`, `<th>csΔ@10</th>`, `<th>kp</th>`, `data-label="kp"`, `<th>objectives</th>`, `9000 obj`, `4000 turret`, `35%`} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("detail body unexpectedly contains %q: %s", unwanted, body)
		}
	}
}

func TestCSDeltaFirst10MinutesRequiresOneReportedLaneOpponent(t *testing.T) {
	seventy, sixty := 70, 60
	player := participantDTO{
		PUUID: "me", TeamID: 100, TeamPosition: "MIDDLE",
		Challenges: participantChallengesDTO{LaneMinionsFirst10Minutes: &seventy},
	}
	tests := []struct {
		name         string
		participants []participantDTO
		want         *int
	}{
		{
			name: "same position on enemy team",
			participants: []participantDTO{
				player,
				{PUUID: "enemy", TeamID: 200, TeamPosition: "middle", Challenges: participantChallengesDTO{LaneMinionsFirst10Minutes: &sixty}},
			},
			want: func() *int { value := 10; return &value }(),
		},
		{
			name: "opponent stat omitted",
			participants: []participantDTO{
				player,
				{PUUID: "enemy", TeamID: 200, TeamPosition: "MIDDLE"},
			},
		},
		{
			name: "position missing",
			participants: []participantDTO{
				{PUUID: "me", TeamID: 100, Challenges: participantChallengesDTO{LaneMinionsFirst10Minutes: &seventy}},
			},
		},
		{
			name: "opponent position ambiguous",
			participants: []participantDTO{
				player,
				{PUUID: "enemy-1", TeamID: 200, TeamPosition: "MIDDLE", Challenges: participantChallengesDTO{LaneMinionsFirst10Minutes: &sixty}},
				{PUUID: "enemy-2", TeamID: 200, TeamPosition: "MIDDLE", Challenges: participantChallengesDTO{LaneMinionsFirst10Minutes: &sixty}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := csDeltaFirst10Minutes(tt.participants[0], tt.participants)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("csDeltaFirst10Minutes() = %d, want nil", *got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Fatalf("csDeltaFirst10Minutes() = %v, want %d", got, *tt.want)
			}
		})
	}
}

func TestCSPerMinute(t *testing.T) {
	tests := []struct {
		name       string
		cs         int
		duration   int
		want       float64
		wantAbsent bool
	}{
		{name: "seconds", cs: 201, duration: 1934, want: 6.2},
		{name: "legacy milliseconds", cs: 180, duration: 1_800_000, want: 6.0},
		{name: "zero duration", cs: 100, duration: 0, wantAbsent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := csPerMinute(tt.cs, tt.duration)
			if tt.wantAbsent {
				if got != nil {
					t.Fatalf("csPerMinute() = %v, want nil", *got)
				}
				return
			}
			if got == nil || math.Abs(*got-tt.want) > 0.001 {
				t.Fatalf("csPerMinute() = %v, want %.1f", got, tt.want)
			}
		})
	}
}

func TestKillParticipationPercent(t *testing.T) {
	players := []participantDTO{
		{PUUID: "me", TeamID: 100, Kills: 3, Assists: 6},
		{PUUID: "ally", TeamID: 100, Kills: 7},
		{PUUID: "enemy", TeamID: 200, Kills: 4},
	}
	got := killParticipationPercent(players[0], players)
	if got == nil || *got != 90 {
		t.Fatalf("killParticipationPercent() = %v, want 90", got)
	}
	if got := killParticipationPercent(participantDTO{TeamID: 300}, players); got != nil {
		t.Fatalf("zero-team-kill participation = %v, want nil", *got)
	}
}

type stubSearcher struct{}

func (stubSearcher) Search(_ context.Context, riotID, region string, _ time.Time) (*ProfileView, []MatchView, error) {
	return &ProfileView{GameName: "Faker", TagLine: "KR1"}, []MatchView{{MatchID: "KR_1", CS: 123, Gold: 456}}, nil
}

type liveStubSearcher struct{}

func (liveStubSearcher) Search(_ context.Context, _, _ string, _ time.Time) (*ProfileView, []MatchView, error) {
	return &ProfileView{PUUID: "player-puuid", GameName: "Faker", TagLine: "KR1"}, []MatchView{{MatchID: "KR_1"}}, nil
}

type stubLiveChecker struct {
	active        bool
	calls         int
	region, puuid string
}

func (s *stubLiveChecker) HasLiveGame(_ context.Context, region, puuid string) (bool, error) {
	s.calls++
	s.region, s.puuid = region, puuid
	return s.active, nil
}

type stubLiveLoader struct {
	view          *LiveGameView
	err           error
	region, puuid string
}

func (s *stubLiveLoader) LoadLiveGame(_ context.Context, region, puuid string) (*LiveGameView, error) {
	s.region, s.puuid = region, puuid
	return s.view, s.err
}

type controlledSearcher struct {
	calls       int
	err         error
	profileName string
}

type expandingSearcher struct {
	counts []int
}

func (s *expandingSearcher) Search(ctx context.Context, riotID, region string, now time.Time) (*ProfileView, []MatchView, error) {
	return s.SearchCount(ctx, riotID, region, now, defaultMatchCount)
}

func (s *expandingSearcher) SearchCount(_ context.Context, _, _ string, _ time.Time, count int) (*ProfileView, []MatchView, error) {
	s.counts = append(s.counts, count)
	matches := make([]MatchView, count)
	for i := range matches {
		matches[i].MatchID = fmt.Sprintf("KR_%d", i+1)
	}
	return &ProfileView{GameName: "Faker", TagLine: "KR1"}, matches, nil
}

func (s *controlledSearcher) Search(_ context.Context, _, _ string, _ time.Time) (*ProfileView, []MatchView, error) {
	s.calls++
	if s.err != nil {
		return nil, nil, s.err
	}
	name := s.profileName
	if name == "" {
		name = "Faker"
	}
	return &ProfileView{GameName: name, TagLine: "KR1"}, []MatchView{{MatchID: "KR_1"}}, nil
}

type stubMatchLoader struct{}

func (stubMatchLoader) MatchDetail(_ context.Context, matchID, me string, _ time.Time) (*MatchDetailView, error) {
	return &MatchDetailView{MatchID: matchID, Team1: TeamView{Players: []PlayerStatsView{{RiotID: me, Region: "kr"}}}}, nil
}

func newTestRiotClient(baseURL string) *RiotClient {
	return &RiotClient{
		APIKey:          "test-key",
		HTTPClient:      http.DefaultClient,
		RegionalBaseURL: func(string) string { return baseURL },
		PlatformBaseURL: func(string) string { return baseURL },
		DataDragonBase:  "https://ddragon.test",
		DataDragonVer:   "16.14.1",
		MatchCount:      10,
	}
}

const matchFixtureJSON = `{
  "metadata":{"matchId":"KR_1"},
  "info":{
    "gameCreation":1720000000000,
    "gameDuration":1934,
    "gameVersion":"16.14.1.123",
    "queueId":420,
    "participants":[
      {"participantId":1,"puuid":"player-puuid","teamId":100,"win":true,"championName":"Ahri","champLevel":18,"teamPosition":"MIDDLE","kills":10,"deaths":2,"assists":8,"totalMinionsKilled":180,"neutralMinionsKilled":21,"goldEarned":12345,"totalDamageDealtToChampions":23456,"damageDealtToObjectives":12000,"damageDealtToTurrets":6000,"visionScore":50,"visionWardsBoughtInGame":3,"turretTakedowns":3,"firstBloodKill":true,"tripleKills":1,"challenges":{"laneMinionsFirst10Minutes":73,"soloKills":2},"item0":3089,"item1":3020,"item2":0,"item3":3135,"item4":1058,"item5":4645,"item6":3364,"summoner1Id":4,"summoner2Id":14,"riotIdGameName":"Hide on bush","riotIdTagline":"KR1"},
      {"participantId":2,"puuid":"ally","teamId":100,"win":true,"championName":"LeeSin","teamPosition":"JUNGLE","kills":1,"deaths":3,"assists":4,"goldEarned":8000,"totalDamageDealtToChampions":10000,"riotIdGameName":"Ally","riotIdTagline":"KR1"},
      {"participantId":3,"puuid":"enemy","teamId":200,"win":false,"championName":"Garen","teamPosition":"MIDDLE","kills":5,"deaths":5,"assists":2,"goldEarned":11000,"totalDamageDealtToChampions":40000,"challenges":{"laneMinionsFirst10Minutes":0},"riotIdGameName":"Enemy","riotIdTagline":"KR1"}
    ],
    "teams":[
      {"teamId":100,"objectives":{"tower":{"kills":9},"dragon":{"kills":3},"baron":{"kills":1},"riftHerald":{"kills":1},"horde":{"kills":4}}},
      {"teamId":200,"objectives":{"tower":{"kills":2},"dragon":{"kills":1},"baron":{"kills":0},"riftHerald":{"kills":0},"horde":{"kills":0}}}
    ]
  }
}`

const timelineFixtureJSON = `{
  "info":{"frames":[
    {"timestamp":0,"events":[]},
    {"timestamp":900000,"participantFrames":{
      "1":{"totalGold":6000,"xp":7000,"minionsKilled":120,"jungleMinionsKilled":0},
      "2":{"totalGold":5200,"xp":6100,"minionsKilled":10,"jungleMinionsKilled":80},
      "3":{"totalGold":5500,"xp":6500,"minionsKilled":105,"jungleMinionsKilled":0}
    },"events":[
      {"type":"CHAMPION_KILL","timestamp":125000,"victimId":3},
      {"type":"CHAMPION_KILL","timestamp":250000,"victimId":1}
    ]}
  ]}
}`
