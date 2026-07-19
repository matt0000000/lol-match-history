package main

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
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
		case strings.HasSuffix(r.URL.Path, "/ids"):
			w.Write([]byte(`["KR_1"]`))
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
	if m.CS != 201 || m.Gold != 12345 {
		t.Fatalf("list economy stats = CS %d, Gold %d", m.CS, m.Gold)
	}
	if len(m.ItemIconURLs) != 7 || m.ItemIconURLs[2] != "" || len(m.SummonerSpellIconURLs) != 2 {
		t.Fatalf("asset slots = %#v / %#v", m.ItemIconURLs, m.SummonerSpellIconURLs)
	}
	if len(paths) != 4 || !strings.Contains(paths[0], "Hide%20on%20bush/KR1") || !strings.Contains(paths[2], "start=0&count=10") {
		t.Fatalf("paths = %#v", paths)
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

func TestRiotClientBuildsMatchDetailFromIDPrefix(t *testing.T) {
	var requestURI string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI = r.URL.RequestURI()
		if r.Header.Get("X-Riot-Token") != "test-key" {
			t.Fatalf("X-Riot-Token = %q", r.Header.Get("X-Riot-Token"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(matchFixtureJSON))
	}))
	defer server.Close()

	client := newTestRiotClient(server.URL)
	detail, err := client.MatchDetail(context.Background(), "KR_1", "Hide on bush#KR1", time.UnixMilli(1_720_003_600_000))
	if err != nil {
		t.Fatal(err)
	}
	if requestURI != "/lol/match/v5/matches/KR_1" {
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
	p := detail.Team1.Players[0]
	if p.RiotID != "Hide on bush#KR1" || p.Region != "kr" || p.ChampionName != "Ahri" || p.Kills != 10 || p.Deaths != 2 || p.Assists != 8 || p.CS != 201 || p.Gold != 12345 || p.Damage != 23456 || p.DamagePercent != 59 || !p.IsHighlighted {
		t.Fatalf("player = %#v", p)
	}
	if detail.Team2.Players[0].DamagePercent != 100 {
		t.Fatalf("highest damage percent = %d", detail.Team2.Players[0].DamagePercent)
	}
	if len(p.ItemIconURLs) != 7 || p.ItemIconURLs[2] != "" || len(p.SummonerSpellIconURLs) != 2 {
		t.Fatalf("asset slots = %#v / %#v", p.ItemIconURLs, p.SummonerSpellIconURLs)
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

	detail := newTestRiotClient("https://riot.test").matchDetailView(dto, "hide on bush#kr1", "kr", time.Now())
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
	detail := newTestRiotClient("https://riot.test").matchDetailView(dto, "", "na1", time.Now())
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
	tmpl := template.Must(template.ParseFS(webFiles, "web/templates/*.tmpl"))
	app := &App{Templates: tmpl, MatchLoader: stubMatchLoader{}}
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/match/KR_1?me=Faker%23KR1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	for _, want := range []string{"MATCH HISTORY", "Back to Match History", "Faker#KR1", `href="/?q=Faker%23KR1&amp;region=kr"`} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("body does not contain %q: %s", want, rr.Body.String())
		}
	}
}

func TestEmbeddedIndexTemplateRendersRedesignedMatchStats(t *testing.T) {
	tmpl := template.Must(template.ParseFS(webFiles, "web/templates/*.tmpl"))
	app := &App{Templates: tmpl, Searcher: stubSearcher{}}
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/?q=Faker%23KR1&region=kr", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	for _, want := range []string{"123", "CS", "456", "Gold"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("body does not contain %q: %s", want, rr.Body.String())
		}
	}
}

type stubSearcher struct{}

func (stubSearcher) Search(_ context.Context, riotID, region string, _ time.Time) (*ProfileView, []MatchView, error) {
	return &ProfileView{GameName: "Faker", TagLine: "KR1"}, []MatchView{{MatchID: "KR_1", CS: 123, Gold: 456}}, nil
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
      {"puuid":"player-puuid","teamId":100,"win":true,"championName":"Ahri","kills":10,"deaths":2,"assists":8,"totalMinionsKilled":180,"neutralMinionsKilled":21,"goldEarned":12345,"totalDamageDealtToChampions":23456,"item0":3089,"item1":3020,"item2":0,"item3":3135,"item4":1058,"item5":4645,"item6":3364,"summoner1Id":4,"summoner2Id":14,"riotIdGameName":"Hide on bush","riotIdTagline":"KR1"},
      {"puuid":"ally","teamId":100,"win":true,"championName":"LeeSin","kills":1,"deaths":3,"assists":4,"goldEarned":8000,"totalDamageDealtToChampions":10000,"riotIdGameName":"Ally","riotIdTagline":"KR1"},
      {"puuid":"enemy","teamId":200,"win":false,"championName":"Garen","kills":5,"deaths":5,"assists":2,"goldEarned":11000,"totalDamageDealtToChampions":40000,"riotIdGameName":"Enemy","riotIdTagline":"KR1"}
    ]
  }
}`
