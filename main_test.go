package main

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"io/fs"
	"math"
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
	if m.RoleLabel != "Mid" || !hasPerformanceLabel(m.PerformanceLabels, "lane bully", "good") || !hasPerformanceLabel(m.PerformanceLabels, "damage carry", "good") {
		t.Fatalf("role/performance = %q/%#v", m.RoleLabel, m.PerformanceLabels)
	}
	if m.CS != 201 || m.Gold != 12345 {
		t.Fatalf("list economy stats = CS %d, Gold %d", m.CS, m.Gold)
	}
	if m.LaneMinionsFirst10Minutes == nil || *m.LaneMinionsFirst10Minutes != 73 {
		t.Fatalf("list 10m CS = %v, want pointer to 73", m.LaneMinionsFirst10Minutes)
	}
	if m.CSDeltaFirst10Minutes == nil || *m.CSDeltaFirst10Minutes != 73 {
		t.Fatalf("list 10m CS delta = %v, want pointer to 73", m.CSDeltaFirst10Minutes)
	}
	if m.CSPerMinute == nil || math.Abs(*m.CSPerMinute-6.2) > 0.001 {
		t.Fatalf("list CS/min = %v, want 6.2", m.CSPerMinute)
	}
	if m.KillParticipationPercent == nil || *m.KillParticipationPercent != 100 {
		t.Fatalf("list KP = %v, want capped 100%%", m.KillParticipationPercent)
	}
	if m.DamageSharePercent == nil || *m.DamageSharePercent != 70 || m.VisionScore != 50 || m.ControlWards != 3 || m.ObjectiveDamage != 12000 || m.TurretDamage != 6000 {
		t.Fatalf("list advanced stats = %#v", m)
	}
	if m.GoldPerMinute == nil || math.Abs(*m.GoldPerMinute-383) > 0.1 || m.DamagePerMinute == nil || math.Abs(*m.DamagePerMinute-727.8) > 0.1 || m.VisionPerMinute == nil || math.Abs(*m.VisionPerMinute-1.6) > 0.1 {
		t.Fatalf("list per-minute stats = gold %v damage %v vision %v", m.GoldPerMinute, m.DamagePerMinute, m.VisionPerMinute)
	}
	if len(m.ItemIconURLs) != 7 || m.ItemIconURLs[2] != "" || len(m.SummonerSpellIconURLs) != 2 {
		t.Fatalf("asset slots = %#v / %#v", m.ItemIconURLs, m.SummonerSpellIconURLs)
	}
	if _, err := client.MatchDetail(context.Background(), "KR_1", "Hide on bush#KR1", time.Now()); err != nil {
		t.Fatalf("cached MatchDetail failed: %v", err)
	}
	if len(paths) != 5 || !strings.Contains(paths[0], "Hide%20on%20bush/KR1") || !strings.Contains(paths[2], "/lol/league/v4/entries/by-puuid/") || !strings.Contains(paths[3], "start=0&count=10") {
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

func TestRecentSummaryUsesAvailableLastTwentyMatchStats(t *testing.T) {
	csSix, csEight := 6.0, 8.0
	deltaTen, deltaNegativeFour := 10, -4
	matches := []MatchView{
		{Win: true, ChampionName: "Ahri", ChampionIconURL: "ahri.png", RoleLabel: "Mid", Kills: 10, Deaths: 2, Assists: 8, CSPerMinute: &csSix, CSDeltaFirst10Minutes: &deltaTen},
		{Win: false, ChampionName: "Lux", RoleLabel: "Support", Kills: 2, Deaths: 4, Assists: 6, CSPerMinute: &csEight},
		{Win: true, ChampionName: "Ahri", RoleLabel: "Mid", Kills: 6, Deaths: 0, Assists: 4, CSDeltaFirst10Minutes: &deltaNegativeFour},
	}

	got := recentSummary(matches)
	if got == nil {
		t.Fatal("recentSummary returned nil")
	}
	if got.Games != 3 || got.Wins != 2 || got.Losses != 1 || got.WinRatePercent != 67 {
		t.Fatalf("record = %#v", got)
	}
	if math.Abs(got.AverageKDA-6) > 0.001 {
		t.Fatalf("AverageKDA = %v, want 6.0", got.AverageKDA)
	}
	if got.AverageCSPerMinute == nil || math.Abs(*got.AverageCSPerMinute-7) > 0.001 {
		t.Fatalf("AverageCSPerMinute = %v, want 7.0", got.AverageCSPerMinute)
	}
	if got.AverageCSDeltaFirst10 == nil || math.Abs(*got.AverageCSDeltaFirst10-3) > 0.001 {
		t.Fatalf("AverageCSDeltaFirst10 = %v, want 3.0", got.AverageCSDeltaFirst10)
	}
	if got.MostPlayedChampion != "Ahri" || got.MostPlayedChampionGames != 2 {
		t.Fatalf("most played = %q/%d, want Ahri/2", got.MostPlayedChampion, got.MostPlayedChampionGames)
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

func TestRecentSummaryHandlesMissingOptionalStatsAndTiesDeterministically(t *testing.T) {
	got := recentSummary([]MatchView{
		{ChampionName: "Lux", Kills: 1},
		{ChampionName: "Ahri", Assists: 1},
	})
	if got.AverageCSPerMinute != nil || got.AverageCSDeltaFirst10 != nil {
		t.Fatalf("missing averages = %v/%v, want nil", got.AverageCSPerMinute, got.AverageCSDeltaFirst10)
	}
	if got.MostPlayedChampion != "Lux" {
		t.Fatalf("tie winner = %q, want first-seen Lux", got.MostPlayedChampion)
	}
	if got.AverageKDA != 2 {
		t.Fatalf("zero-death KDA = %v, want 2", got.AverageKDA)
	}
	if recentSummary(nil) != nil {
		t.Fatal("empty summary should be nil")
	}
}

func TestEmbeddedIndexTemplateRendersRecentSummary(t *testing.T) {
	tmpl := template.Must(parseTemplates())
	cs, delta := 7.2, 3.0
	data := PageData{
		Profile: &ProfileView{},
		RecentSummary: &RecentSummaryView{
			Games: 20, Wins: 12, Losses: 8, WinRatePercent: 60,
			AverageKDA: 3.45, AverageCSPerMinute: &cs, AverageCSDeltaFirst10: &delta,
			MostPlayedChampion: "Ahri", MostPlayedChampionGames: 6,
			Champions: []ChampionSummaryView{{ChampionName: "Ahri", Games: 6, WinRatePercent: 67, AverageKDA: 4.2, AverageCSPerMinute: &cs, AverageCSDeltaFirst10: &delta}},
			Roles:     []RoleSummaryView{{Role: "Mid", Games: 15, WinRatePercent: 60}},
		},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "content", data); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"last 20 matches", "12w 8l", "60%", "3.45", "7.2", "&#43;3.0", "Ahri", "most played · 6 games", "recent champions", "6 games · 67% wr", "recent roles", "15 games"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("summary does not contain %q: %s", want, buf.String())
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
	if detail.Team1.Objectives != (ObjectiveView{Towers: 9, Dragons: 3, Barons: 1, Heralds: 1, Grubs: 4}) || detail.Team2.Objectives != (ObjectiveView{Towers: 2, Dragons: 1}) {
		t.Fatalf("objectives = %#v / %#v", detail.Team1.Objectives, detail.Team2.Objectives)
	}
	p := detail.Team1.Players[0]
	if p.RiotID != "Hide on bush#KR1" || p.Region != "kr" || p.ChampionName != "Ahri" || p.Kills != 10 || p.Deaths != 2 || p.Assists != 8 || p.CS != 201 || p.Gold != 12345 || p.Damage != 23456 || p.DamagePercent != 59 || !p.IsHighlighted {
		t.Fatalf("player = %#v", p)
	}
	if p.LaneMinionsFirst10Minutes == nil || *p.LaneMinionsFirst10Minutes != 73 {
		t.Fatalf("searched player 10m CS = %v, want pointer to 73", p.LaneMinionsFirst10Minutes)
	}
	if p.CSDeltaFirst10Minutes == nil || *p.CSDeltaFirst10Minutes != 73 {
		t.Fatalf("searched player 10m CS delta = %v, want pointer to 73", p.CSDeltaFirst10Minutes)
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
	if got := detail.Team2.Players[0].CSDeltaFirst10Minutes; got == nil || *got != -73 {
		t.Fatalf("enemy 10m CS delta = %v, want pointer to -73", got)
	}
	if detail.Team1.Players[1].CSDeltaFirst10Minutes != nil {
		t.Fatalf("unpaired ally 10m CS delta = %v, want nil", detail.Team1.Players[1].CSDeltaFirst10Minutes)
	}
	if detail.Team2.Players[0].DamagePercent != 100 {
		t.Fatalf("highest damage percent = %d", detail.Team2.Players[0].DamagePercent)
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
	if requests != 1 {
		t.Fatalf("Match-V5 requests = %d, want 1", requests)
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
	if requests != 2 {
		t.Fatalf("Match-V5 requests = %d, want 2", requests)
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
	tmpl := template.Must(parseTemplates())
	app := &App{Templates: tmpl, MatchLoader: stubMatchLoader{}}
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/match/KR_1?me=Faker%23KR1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	for _, want := range []string{"match-history", "back to match-history", "Faker#KR1", `href="/?q=Faker%23KR1&amp;region=kr"`, "[ copy link ]", "navigator.clipboard"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("body does not contain %q: %s", want, rr.Body.String())
		}
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
	for _, want := range []string{"123", "cs", "cs/min", "cs@10m", "csΔ@10", "kp"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("body does not contain %q: %s", want, rr.Body.String())
		}
	}
	if strings.Contains(rr.Body.String(), `<span class="k">gold</span>`) {
		t.Fatalf("compact history row still renders raw gold: %s", rr.Body.String())
	}
}

func TestEmbeddedIndexTemplateRendersLaneMinionsFirst10Minutes(t *testing.T) {
	tmpl := template.Must(parseTemplates())
	seventyThree, twelve, negativeEight, zero, ninety := 73, 12, -8, 0, 90
	sixPointTwo := 6.2
	data := PageData{
		Profile: &ProfileView{},
		Matches: []MatchView{
			{LaneMinionsFirst10Minutes: &seventyThree, CSDeltaFirst10Minutes: &twelve, CSPerMinute: &sixPointTwo, KillParticipationPercent: &ninety, PerformanceLabels: []PerformanceLabelView{{Text: "strong lane", Tone: "good"}, {Text: "everywhere", Tone: "good"}}},
			{LaneMinionsFirst10Minutes: &zero, CSDeltaFirst10Minutes: &negativeEight},
			{CSDeltaFirst10Minutes: &zero},
			{},
		},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "content", data); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, want := range []string{
		`<span class="v">73</span><br><span class="k">cs@10m</span>`,
		`<span class="v">0</span><br><span class="k">cs@10m</span>`,
		`<span class="v">—</span><br><span class="k">cs@10m</span>`,
		`<span class="v">&#43;12</span><br><span class="k">csΔ@10</span>`,
		`<span class="v">-8</span><br><span class="k">csΔ@10</span>`,
		`<span class="v">0</span><br><span class="k">csΔ@10</span>`,
		`<span class="v">—</span><br><span class="k">csΔ@10</span>`,
		`<span class="v">6.2</span><br><span class="k">cs/min</span>`,
		`<span class="v">—</span><br><span class="k">cs/min</span>`,
		`<span class="v">90%</span><br><span class="k">kp</span>`,
		`<span class="v">—</span><br><span class="k">kp</span>`,
		`<span class="performance-tag good">strong lane</span>`,
		`<span class="performance-tag good">everywhere</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("index body does not contain %q: %s", want, body)
		}
	}
}

func TestEmbeddedMatchTemplateRendersLaneMinionsFirst10Minutes(t *testing.T) {
	tmpl := template.Must(parseTemplates())
	seventyThree, twelve, negativeEight, zero, ninety, thirtyFive := 73, 12, -8, 0, 90, 35
	sixPointTwo := 6.2
	data := MatchDetailView{
		GameModeLabel: "Ranked Solo/Duo",
		Team1: TeamView{Win: true, Objectives: ObjectiveView{Towers: 9, Dragons: 3, Barons: 1, Heralds: 1, Grubs: 4}, Players: []PlayerStatsView{
			{LaneMinionsFirst10Minutes: &seventyThree, CSDeltaFirst10Minutes: &twelve, CSPerMinute: &sixPointTwo, KillParticipationPercent: &ninety, Gold: 12000, GoldPerMinute: &sixPointTwo, Damage: 24000, DamageSharePercent: &thirtyFive, DamagePerMinute: &sixPointTwo, VisionScore: 42, VisionPerMinute: &sixPointTwo, ControlWards: 3, ObjectiveDamage: 9000, TurretDamage: 4000, PerformanceLabels: []PerformanceLabelView{{Text: "damage carry", Tone: "good"}}},
			{LaneMinionsFirst10Minutes: &zero, CSDeltaFirst10Minutes: &negativeEight},
			{CSDeltaFirst10Minutes: &zero},
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
		`<th>csΔ@10</th>`,
		`<th>cs/min</th>`,
		`<th>kp</th>`,
		`<th>vision</th>`,
		`<th>objectives</th>`,
		`<td class="num-cell">73</td>`,
		`<td class="num-cell">&#43;12</td>`,
		`<td class="num-cell">-8</td>`,
		`<td class="num-cell">0</td>`,
		`<td class="num-cell">—</td>`,
		`<td class="num-cell">6.2</td>`,
		`<td class="num-cell">90%</td>`,
		`<small>35% · 6.2/min</small>`,
		`<small>6.2/min · 3 cw</small>`,
		`<span>9000 obj</span><small>4000 turret</small>`,
		`<span class="performance-tag good">damage carry</span>`,
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

type controlledSearcher struct {
	calls       int
	err         error
	profileName string
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
      {"puuid":"player-puuid","teamId":100,"win":true,"championName":"Ahri","teamPosition":"MIDDLE","kills":10,"deaths":2,"assists":8,"totalMinionsKilled":180,"neutralMinionsKilled":21,"goldEarned":12345,"totalDamageDealtToChampions":23456,"damageDealtToObjectives":12000,"damageDealtToTurrets":6000,"visionScore":50,"visionWardsBoughtInGame":3,"turretTakedowns":3,"firstBloodKill":true,"tripleKills":1,"challenges":{"laneMinionsFirst10Minutes":73,"soloKills":2},"item0":3089,"item1":3020,"item2":0,"item3":3135,"item4":1058,"item5":4645,"item6":3364,"summoner1Id":4,"summoner2Id":14,"riotIdGameName":"Hide on bush","riotIdTagline":"KR1"},
      {"puuid":"ally","teamId":100,"win":true,"championName":"LeeSin","teamPosition":"JUNGLE","kills":1,"deaths":3,"assists":4,"goldEarned":8000,"totalDamageDealtToChampions":10000,"riotIdGameName":"Ally","riotIdTagline":"KR1"},
      {"puuid":"enemy","teamId":200,"win":false,"championName":"Garen","teamPosition":"MIDDLE","kills":5,"deaths":5,"assists":2,"goldEarned":11000,"totalDamageDealtToChampions":40000,"challenges":{"laneMinionsFirst10Minutes":0},"riotIdGameName":"Enemy","riotIdTagline":"KR1"}
    ],
    "teams":[
      {"teamId":100,"objectives":{"tower":{"kills":9},"dragon":{"kills":3},"baron":{"kills":1},"riftHerald":{"kills":1},"horde":{"kills":4}}},
      {"teamId":200,"objectives":{"tower":{"kills":2},"dragon":{"kills":1},"baron":{"kills":0},"riftHerald":{"kills":0},"horde":{"kills":0}}}
    ]
  }
}`
