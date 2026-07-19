package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultDataDragonVersion = "16.14.1"

//go:embed web/templates/*.tmpl web/static/*
var webFiles embed.FS

type PageData struct {
	Query   string
	Region  string
	Error   string
	Profile *ProfileView
	Matches []MatchView
}

type ProfileView struct {
	GameName       string
	TagLine        string
	ProfileIconURL string
	SummonerLevel  int
}

type MatchView struct {
	MatchID               string
	Win                   bool
	GameModeLabel         string
	DurationLabel         string
	TimeAgoLabel          string
	ChampionName          string
	ChampionIconURL       string
	Kills                 int
	Deaths                int
	Assists               int
	ItemIconURLs          []string
	SummonerSpellIconURLs []string
	Team1                 []ParticipantView
	Team2                 []ParticipantView
}

type ParticipantView struct {
	RiotID           string
	ChampionIconURL  string
	IsSearchedPlayer bool
}

type MatchDetailView struct {
	Query         string
	Region        string
	Error         string
	MatchID       string
	GameModeLabel string
	DurationLabel string
	TimeAgoLabel  string
	Team1         TeamView
	Team2         TeamView
}

type TeamView struct {
	Win     bool
	Players []PlayerStatsView
}

type PlayerStatsView struct {
	RiotID                string
	ChampionName          string
	ChampionIconURL       string
	Kills                 int
	Deaths                int
	Assists               int
	CS                    int
	Gold                  int
	Damage                int
	ItemIconURLs          []string
	SummonerSpellIconURLs []string
	IsHighlighted         bool
}

type Searcher interface {
	Search(context.Context, string, string, time.Time) (*ProfileView, []MatchView, error)
}

type MatchLoader interface {
	MatchDetail(context.Context, string, string, time.Time) (*MatchDetailView, error)
}

type App struct {
	Templates   *template.Template
	Searcher    Searcher
	MatchLoader MatchLoader
	StaticFS    fs.FS
	Logger      *log.Logger
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	if a.StaticFS != nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(a.StaticFS)))
	}
	mux.HandleFunc("GET /match/{id}", a.handleMatchDetail)
	mux.HandleFunc("GET /", a.handleIndex)
	return mux
}

func (a *App) handleMatchDetail(w http.ResponseWriter, r *http.Request) {
	matchID := r.PathValue("id")
	region, err := regionFromMatchID(matchID)
	if region == "" {
		region = "na1"
	}
	data := MatchDetailView{
		Query:   strings.TrimSpace(r.URL.Query().Get("me")),
		Region:  region,
		MatchID: matchID,
	}
	if err != nil {
		data.Error = "Invalid match ID."
	} else if a.MatchLoader == nil {
		data.Error = "Match details are temporarily unavailable."
	} else {
		detail, loadErr := a.MatchLoader.MatchDetail(r.Context(), matchID, data.Query, time.Now())
		if loadErr != nil {
			data.Error = loadErr.Error()
			if a.Logger != nil {
				a.Logger.Printf("match %q: %v", matchID, loadErr)
			}
		} else {
			data = *detail
			data.Query = strings.TrimSpace(r.URL.Query().Get("me"))
			data.Region = region
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.Templates.ExecuteTemplate(w, "matchLayout", data); err != nil && a.Logger != nil {
		a.Logger.Printf("render match %q: %v", matchID, err)
	}
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	data := PageData{
		Query:  strings.TrimSpace(r.URL.Query().Get("q")),
		Region: strings.ToLower(strings.TrimSpace(r.URL.Query().Get("region"))),
	}
	if data.Region == "" {
		data.Region = "na1"
	}
	if data.Query != "" {
		if !supportedRegion(data.Region) {
			data.Error = "Choose a supported region."
		} else if _, _, err := parseRiotID(data.Query); err != nil {
			data.Error = "Enter a Riot ID in the form Name#Tag."
		} else if a.Searcher == nil {
			data.Error = "Search is temporarily unavailable."
		} else {
			profile, matches, err := a.Searcher.Search(r.Context(), data.Query, data.Region, time.Now())
			if err != nil {
				data.Error = err.Error()
				if a.Logger != nil {
					a.Logger.Printf("search %q in %s: %v", data.Query, data.Region, err)
				}
			} else {
				data.Profile, data.Matches = profile, matches
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.Templates.ExecuteTemplate(w, "layout", data); err != nil && a.Logger != nil {
		a.Logger.Printf("render index: %v", err)
	}
}

type RiotClient struct {
	APIKey          string
	HTTPClient      *http.Client
	RegionalBaseURL func(string) string
	PlatformBaseURL func(string) string
	DataDragonBase  string
	DataDragonVer   string
	MatchCount      int
}

type accountDTO struct {
	PUUID    string `json:"puuid"`
	GameName string `json:"gameName"`
	TagLine  string `json:"tagLine"`
}

type summonerDTO struct {
	ProfileIconID int `json:"profileIconId"`
	SummonerLevel int `json:"summonerLevel"`
}

type matchDTO struct {
	Metadata struct {
		MatchID string `json:"matchId"`
	} `json:"metadata"`
	Info struct {
		GameCreation int64            `json:"gameCreation"`
		GameDuration int              `json:"gameDuration"`
		GameVersion  string           `json:"gameVersion"`
		QueueID      int              `json:"queueId"`
		Participants []participantDTO `json:"participants"`
	} `json:"info"`
}

type participantDTO struct {
	PUUID                       string `json:"puuid"`
	TeamID                      int    `json:"teamId"`
	Win                         bool   `json:"win"`
	ChampionName                string `json:"championName"`
	Kills                       int    `json:"kills"`
	Deaths                      int    `json:"deaths"`
	Assists                     int    `json:"assists"`
	TotalMinionsKilled          int    `json:"totalMinionsKilled"`
	NeutralMinionsKilled        int    `json:"neutralMinionsKilled"`
	GoldEarned                  int    `json:"goldEarned"`
	TotalDamageDealtToChampions int    `json:"totalDamageDealtToChampions"`
	Item0                       int    `json:"item0"`
	Item1                       int    `json:"item1"`
	Item2                       int    `json:"item2"`
	Item3                       int    `json:"item3"`
	Item4                       int    `json:"item4"`
	Item5                       int    `json:"item5"`
	Item6                       int    `json:"item6"`
	Summoner1ID                 int    `json:"summoner1Id"`
	Summoner2ID                 int    `json:"summoner2Id"`
	RiotIDGameName              string `json:"riotIdGameName"`
	RiotIDTagLine               string `json:"riotIdTagline"`
}

func (c *RiotClient) MatchDetail(ctx context.Context, matchID, me string, now time.Time) (*MatchDetailView, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, errors.New("Riot API key is not configured.")
	}
	region, err := regionFromMatchID(matchID)
	if err != nil {
		return nil, err
	}
	var dto matchDTO
	endpoint := c.RegionalBaseURL(region) + "/lol/match/v5/matches/" + url.PathEscape(matchID)
	if err := c.getJSON(ctx, endpoint, &dto); err != nil {
		return nil, err
	}
	if dto.Metadata.MatchID == "" {
		dto.Metadata.MatchID = matchID
	}
	view := c.matchDetailView(dto, me, now)
	view.Query = strings.TrimSpace(me)
	view.Region = region
	return &view, nil
}

func NewRiotClient(apiKey string) *RiotClient {
	return &RiotClient{
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: 12 * time.Second},
		RegionalBaseURL: func(region string) string {
			return "https://" + regionalRoute(region) + ".api.riotgames.com"
		},
		PlatformBaseURL: func(region string) string {
			return "https://" + region + ".api.riotgames.com"
		},
		DataDragonBase: "https://ddragon.leagueoflegends.com",
		DataDragonVer:  defaultDataDragonVersion,
		MatchCount:     10,
	}
}

func (c *RiotClient) Search(ctx context.Context, riotID, region string, now time.Time) (*ProfileView, []MatchView, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, nil, errors.New("Riot API key is not configured.")
	}
	gameName, tagLine, err := parseRiotID(riotID)
	if err != nil {
		return nil, nil, err
	}
	account, err := c.lookupAccount(ctx, region, gameName, tagLine)
	if err != nil {
		return nil, nil, err
	}
	summoner, err := c.lookupSummoner(ctx, region, account.PUUID)
	if err != nil {
		return nil, nil, err
	}
	matchIDs, err := c.listMatchIDs(ctx, region, account.PUUID)
	if err != nil {
		return nil, nil, err
	}
	matches, err := c.lookupMatches(ctx, region, account.PUUID, matchIDs, now)
	if err != nil {
		return nil, nil, err
	}
	profile := &ProfileView{
		GameName:       account.GameName,
		TagLine:        account.TagLine,
		ProfileIconURL: fmt.Sprintf("%s/cdn/%s/img/profileicon/%d.png", c.DataDragonBase, c.DataDragonVer, summoner.ProfileIconID),
		SummonerLevel:  summoner.SummonerLevel,
	}
	return profile, matches, nil
}

func (c *RiotClient) lookupAccount(ctx context.Context, region, gameName, tagLine string) (accountDTO, error) {
	var out accountDTO
	path := "/riot/account/v1/accounts/by-riot-id/" + url.PathEscape(gameName) + "/" + url.PathEscape(tagLine)
	err := c.getJSON(ctx, c.RegionalBaseURL(region)+path, &out)
	return out, err
}

func (c *RiotClient) lookupSummoner(ctx context.Context, region, puuid string) (summonerDTO, error) {
	var out summonerDTO
	err := c.getJSON(ctx, c.PlatformBaseURL(region)+"/lol/summoner/v4/summoners/by-puuid/"+url.PathEscape(puuid), &out)
	return out, err
}

func (c *RiotClient) listMatchIDs(ctx context.Context, region, puuid string) ([]string, error) {
	count := c.MatchCount
	if count <= 0 {
		count = 10
	}
	endpoint := c.RegionalBaseURL(region) + "/lol/match/v5/matches/by-puuid/" + url.PathEscape(puuid) + "/ids?start=0&count=" + strconv.Itoa(count)
	var out []string
	err := c.getJSON(ctx, endpoint, &out)
	return out, err
}

func (c *RiotClient) lookupMatches(ctx context.Context, region, puuid string, ids []string, now time.Time) ([]MatchView, error) {
	views := make([]MatchView, len(ids))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for i, id := range ids {
		i, id := i, id
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			var dto matchDTO
			endpoint := c.RegionalBaseURL(region) + "/lol/match/v5/matches/" + url.PathEscape(id)
			if err := c.getJSON(ctx, endpoint, &dto); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				errMu.Unlock()
				return
			}
			views[i] = c.matchView(dto, puuid, now)
		}()
	}
	wg.Wait()
	return views, firstErr
}

func (c *RiotClient) getJSON(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("Riot services are temporarily unavailable.")
	}
	req.Header.Set("X-Riot-Token", c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Riot services are temporarily unavailable.")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case http.StatusNotFound:
			return errors.New("No player found for that Riot ID and region.")
		case http.StatusUnauthorized, http.StatusForbidden:
			return errors.New("Riot API key is invalid or expired. Replace RIOT_API_KEY and restart the server.")
		case http.StatusTooManyRequests:
			if retry := resp.Header.Get("Retry-After"); retry != "" {
				return fmt.Errorf("Riot API rate limit reached. Try again in %s seconds", retry)
			}
			return errors.New("Riot API rate limit reached. Try again shortly.")
		default:
			return errors.New("Riot services are temporarily unavailable.")
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return errors.New("Riot services returned an unexpected response.")
	}
	return nil
}

func (c *RiotClient) matchView(dto matchDTO, searchedPUUID string, now time.Time) MatchView {
	version := majorMinorVersion(dto.Info.GameVersion)
	if version == "" {
		version = c.DataDragonVer
	}
	var player participantDTO
	found := false
	for _, p := range dto.Info.Participants {
		if p.PUUID == searchedPUUID {
			player, found = p, true
			break
		}
	}
	view := MatchView{
		MatchID:               dto.Metadata.MatchID,
		Win:                   player.Win,
		GameModeLabel:         queueLabel(dto.Info.QueueID),
		DurationLabel:         durationLabel(dto.Info.GameDuration),
		TimeAgoLabel:          timeAgoLabel(time.UnixMilli(dto.Info.GameCreation), now),
		ChampionName:          player.ChampionName,
		ChampionIconURL:       c.championURL(version, player.ChampionName),
		Kills:                 player.Kills,
		Deaths:                player.Deaths,
		Assists:               player.Assists,
		ItemIconURLs:          make([]string, 7),
		SummonerSpellIconURLs: make([]string, 2),
	}
	items := []int{player.Item0, player.Item1, player.Item2, player.Item3, player.Item4, player.Item5, player.Item6}
	for i, item := range items {
		if item != 0 {
			view.ItemIconURLs[i] = fmt.Sprintf("%s/cdn/%s/img/item/%d.png", c.DataDragonBase, version, item)
		}
	}
	view.SummonerSpellIconURLs[0] = c.spellURL(version, player.Summoner1ID)
	view.SummonerSpellIconURLs[1] = c.spellURL(version, player.Summoner2ID)
	if found {
		for _, p := range dto.Info.Participants {
			participant := ParticipantView{
				RiotID:           displayRiotID(p),
				ChampionIconURL:  c.championURL(version, p.ChampionName),
				IsSearchedPlayer: p.PUUID == searchedPUUID,
			}
			if p.TeamID == player.TeamID {
				view.Team1 = append(view.Team1, participant)
			} else {
				view.Team2 = append(view.Team2, participant)
			}
		}
	}
	return view
}

func (c *RiotClient) matchDetailView(dto matchDTO, me string, now time.Time) MatchDetailView {
	version := majorMinorVersion(dto.Info.GameVersion)
	if version == "" {
		version = c.DataDragonVer
	}
	team1ID := 100
	for _, p := range dto.Info.Participants {
		if strings.EqualFold(displayRiotID(p), me) {
			team1ID = p.TeamID
			break
		}
	}
	view := MatchDetailView{
		MatchID:       dto.Metadata.MatchID,
		GameModeLabel: queueLabel(dto.Info.QueueID),
		DurationLabel: durationLabel(dto.Info.GameDuration),
		TimeAgoLabel:  timeAgoLabel(time.UnixMilli(dto.Info.GameCreation), now),
	}
	for _, p := range dto.Info.Participants {
		player := c.playerStatsView(version, p, me)
		if p.TeamID == team1ID {
			if len(view.Team1.Players) == 0 {
				view.Team1.Win = p.Win
			}
			view.Team1.Players = append(view.Team1.Players, player)
		} else {
			if len(view.Team2.Players) == 0 {
				view.Team2.Win = p.Win
			}
			view.Team2.Players = append(view.Team2.Players, player)
		}
	}
	return view
}

func (c *RiotClient) playerStatsView(version string, p participantDTO, me string) PlayerStatsView {
	view := PlayerStatsView{
		RiotID:                displayRiotID(p),
		ChampionName:          p.ChampionName,
		ChampionIconURL:       c.championURL(version, p.ChampionName),
		Kills:                 p.Kills,
		Deaths:                p.Deaths,
		Assists:               p.Assists,
		CS:                    p.TotalMinionsKilled + p.NeutralMinionsKilled,
		Gold:                  p.GoldEarned,
		Damage:                p.TotalDamageDealtToChampions,
		ItemIconURLs:          make([]string, 7),
		SummonerSpellIconURLs: make([]string, 2),
		IsHighlighted:         me != "" && strings.EqualFold(displayRiotID(p), me),
	}
	items := []int{p.Item0, p.Item1, p.Item2, p.Item3, p.Item4, p.Item5, p.Item6}
	for i, item := range items {
		if item != 0 {
			view.ItemIconURLs[i] = fmt.Sprintf("%s/cdn/%s/img/item/%d.png", c.DataDragonBase, version, item)
		}
	}
	view.SummonerSpellIconURLs[0] = c.spellURL(version, p.Summoner1ID)
	view.SummonerSpellIconURLs[1] = c.spellURL(version, p.Summoner2ID)
	return view
}

func (c *RiotClient) championURL(version, name string) string {
	if name == "" {
		return ""
	}
	return fmt.Sprintf("%s/cdn/%s/img/champion/%s.png", c.DataDragonBase, version, url.PathEscape(name))
}

func (c *RiotClient) spellURL(version string, id int) string {
	name := map[int]string{1: "SummonerBoost", 3: "SummonerExhaust", 4: "SummonerFlash", 6: "SummonerHaste", 7: "SummonerHeal", 11: "SummonerSmite", 12: "SummonerTeleport", 13: "SummonerMana", 14: "SummonerDot", 21: "SummonerBarrier", 32: "SummonerSnowball"}[id]
	if name == "" {
		return ""
	}
	return fmt.Sprintf("%s/cdn/%s/img/spell/%s.png", c.DataDragonBase, version, name)
}

func parseRiotID(input string) (string, string, error) {
	input = strings.TrimSpace(input)
	i := strings.LastIndex(input, "#")
	if i <= 0 || i == len(input)-1 {
		return "", "", errors.New("Riot ID must be in the form Name#Tag")
	}
	gameName, tagLine := strings.TrimSpace(input[:i]), strings.TrimSpace(input[i+1:])
	if gameName == "" || tagLine == "" {
		return "", "", errors.New("Riot ID must be in the form Name#Tag")
	}
	return gameName, tagLine, nil
}

func supportedRegion(region string) bool {
	_, ok := regionRoutes[region]
	return ok
}

func regionalRoute(region string) string {
	return regionRoutes[region]
}

func regionFromMatchID(matchID string) (string, error) {
	i := strings.Index(matchID, "_")
	if i <= 0 || i == len(matchID)-1 {
		return "", errors.New("invalid match ID")
	}
	region := strings.ToLower(matchID[:i])
	if !supportedRegion(region) {
		return "", errors.New("unsupported match region")
	}
	return region, nil
}

var regionRoutes = map[string]string{
	"na1": "americas", "br1": "americas", "la1": "americas", "la2": "americas",
	"euw1": "europe", "eun1": "europe", "tr1": "europe", "ru": "europe",
	"kr": "asia", "jp1": "asia", "oc1": "sea",
}

func majorMinorVersion(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1] + ".1"
}

func queueLabel(queueID int) string {
	labels := map[int]string{
		400: "Normal Draft", 420: "Ranked Solo/Duo", 430: "Normal Blind", 440: "Ranked Flex",
		450: "ARAM", 490: "Quickplay", 900: "ARURF", 1020: "One for All", 1700: "Arena", 1710: "Arena", 1810: "Swarm",
	}
	if label := labels[queueID]; label != "" {
		return label
	}
	return "League of Legends"
}

func durationLabel(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	return fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
}

func timeAgoLabel(start, now time.Time) string {
	d := now.Sub(start)
	if d < 0 || d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		minutes := int(d / time.Minute)
		return plural(minutes, "minute") + " ago"
	}
	if d < 24*time.Hour {
		hours := int(d / time.Hour)
		return plural(hours, "hour") + " ago"
	}
	if d < 30*24*time.Hour {
		days := int(d / (24 * time.Hour))
		return plural(days, "day") + " ago"
	}
	if d < 365*24*time.Hour {
		months := int(d / (30 * 24 * time.Hour))
		return plural(months, "month") + " ago"
	}
	years := int(d / (365 * 24 * time.Hour))
	return plural(years, "year") + " ago"
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

func displayRiotID(p participantDTO) string {
	if p.RiotIDGameName == "" {
		return "Unknown"
	}
	if p.RiotIDTagLine == "" {
		return p.RiotIDGameName
	}
	return p.RiotIDGameName + "#" + p.RiotIDTagLine
}

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags)
	tmpl, err := template.ParseFS(webFiles, "web/templates/*.tmpl")
	if err != nil {
		logger.Fatal(err)
	}
	staticFiles, err := fs.Sub(webFiles, "web/static")
	if err != nil {
		logger.Fatal(err)
	}
	client := NewRiotClient(os.Getenv("RIOT_API_KEY"))
	app := &App{
		Templates:   tmpl,
		Searcher:    client,
		MatchLoader: client,
		StaticFS:    staticFiles,
		Logger:      logger,
	}
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	logger.Printf("listening on http://localhost:%s", port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal(err)
	}
}
