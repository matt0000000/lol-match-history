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
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultDataDragonVersion = "16.14.1"

// riotVerificationToken proves domain ownership for the Riot Developer Portal
// API key application. Safe to remove once the application is approved.
const riotVerificationToken = "78f3e35f-b152-4401-b2bb-1d2ffecdc690"

//go:embed web/templates/*.tmpl web/static/*
var webFiles embed.FS

type PageData struct {
	Query            string
	Region           string
	Error            string
	LastUpdatedLabel string
	Profile          *ProfileView
	Matches          []MatchView
}

type ProfileView struct {
	GameName       string
	TagLine        string
	ProfileIconURL string
	SummonerLevel  int
	SoloRank       *RankView
	FlexRank       *RankView
}

type RankView struct {
	Tier           string
	Division       string
	LeaguePoints   int
	Wins           int
	Losses         int
	WinRatePercent int
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
	CS                    int
	Gold                  int
	ItemIconURLs          []string
	SummonerSpellIconURLs []string
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
	Win          bool
	TotalKills   int
	TotalDeaths  int
	TotalAssists int
	TotalGold    int
	Players      []PlayerStatsView
}

type PlayerStatsView struct {
	RiotID                string
	Region                string
	ChampionName          string
	ChampionIconURL       string
	Kills                 int
	Deaths                int
	Assists               int
	CS                    int
	Gold                  int
	Damage                int
	DamagePercent         int
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

type SearchSnapshot struct {
	Profile   *ProfileView
	Matches   []MatchView
	UpdatedAt time.Time
}

type SearchCache struct {
	mu      sync.RWMutex
	entries map[string]SearchSnapshot
}

func NewSearchCache() *SearchCache {
	return &SearchCache{entries: make(map[string]SearchSnapshot)}
}

func (c *SearchCache) Get(riotID, region string) (SearchSnapshot, bool) {
	if c == nil {
		return SearchSnapshot{}, false
	}
	c.mu.RLock()
	snapshot, ok := c.entries[searchCacheKey(riotID, region)]
	c.mu.RUnlock()
	return snapshot, ok
}

func (c *SearchCache) Set(riotID, region string, snapshot SearchSnapshot) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]SearchSnapshot)
	}
	c.entries[searchCacheKey(riotID, region)] = snapshot
	c.mu.Unlock()
}

func searchCacheKey(riotID, region string) string {
	return strings.ToLower(strings.TrimSpace(riotID)) + "\x00" + strings.ToLower(strings.TrimSpace(region))
}

type App struct {
	Templates   *template.Template
	Searcher    Searcher
	MatchLoader MatchLoader
	Cache       *SearchCache
	Now         func() time.Time
	StaticFS    fs.FS
	Logger      *log.Logger
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	if a.StaticFS != nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(a.StaticFS)))
	}
	mux.HandleFunc("GET /riot.txt", handleRiotVerification)
	mux.HandleFunc("GET /match/{id}", a.handleMatchDetail)
	mux.HandleFunc("GET /", a.handleIndex)
	return mux
}

func handleRiotVerification(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(riotVerificationToken))
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
	now := a.currentTime()
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
		} else {
			cached, hasCached := a.Cache.Get(data.Query, data.Region)
			refresh := r.URL.Query().Get("refresh") == "1"
			if hasCached && !refresh {
				applySnapshot(&data, cached, now)
			} else if a.Searcher == nil {
				data.Error = "Search is temporarily unavailable."
				if hasCached {
					applySnapshot(&data, cached, now)
				}
			} else {
				profile, matches, err := a.Searcher.Search(r.Context(), data.Query, data.Region, now)
				if err != nil {
					data.Error = err.Error()
					if hasCached {
						applySnapshot(&data, cached, now)
					}
					if a.Logger != nil {
						a.Logger.Printf("search %q in %s: %v", data.Query, data.Region, err)
					}
				} else {
					completedAt := a.currentTime()
					snapshot := SearchSnapshot{Profile: profile, Matches: matches, UpdatedAt: completedAt}
					a.Cache.Set(data.Query, data.Region, snapshot)
					applySnapshot(&data, snapshot, completedAt)
				}
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.Templates.ExecuteTemplate(w, "layout", data); err != nil && a.Logger != nil {
		a.Logger.Printf("render index: %v", err)
	}
}

func (a *App) currentTime() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func applySnapshot(data *PageData, snapshot SearchSnapshot, now time.Time) {
	data.Profile = snapshot.Profile
	data.Matches = snapshot.Matches
	data.LastUpdatedLabel = "Updated " + timeAgoLabel(snapshot.UpdatedAt, now)
}

type RiotClient struct {
	APIKey             string
	HTTPClient         *http.Client
	RegionalBaseURL    func(string) string
	PlatformBaseURL    func(string) string
	DataDragonBase     string
	DataDragonVer      string
	MatchCount         int
	MinRequestInterval time.Duration
	requestMu          sync.Mutex
	nextRequest        map[string]time.Time
	matchCacheMu       sync.RWMutex
	matchCache         map[string]matchDTO
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

type leagueEntryDTO struct {
	QueueType    string `json:"queueType"`
	Tier         string `json:"tier"`
	Rank         string `json:"rank"`
	LeaguePoints int    `json:"leaguePoints"`
	Wins         int    `json:"wins"`
	Losses       int    `json:"losses"`
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
	dto, err = c.lookupMatchDTO(ctx, region, matchID)
	if err != nil {
		return nil, err
	}
	view := c.matchDetailView(dto, me, region, now)
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
		DataDragonBase:     "https://ddragon.leagueoflegends.com",
		DataDragonVer:      defaultDataDragonVersion,
		MatchCount:         20,
		MinRequestInterval: 60 * time.Millisecond,
		nextRequest:        make(map[string]time.Time),
		matchCache:         make(map[string]matchDTO),
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
	ranks, err := c.lookupRanks(ctx, region, account.PUUID)
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
	for _, entry := range ranks {
		switch entry.QueueType {
		case "RANKED_SOLO_5x5":
			profile.SoloRank = rankView(entry)
		case "RANKED_FLEX_SR":
			profile.FlexRank = rankView(entry)
		}
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

func (c *RiotClient) lookupRanks(ctx context.Context, region, puuid string) ([]leagueEntryDTO, error) {
	var out []leagueEntryDTO
	err := c.getJSON(ctx, c.PlatformBaseURL(region)+"/lol/league/v4/entries/by-puuid/"+url.PathEscape(puuid), &out)
	return out, err
}

func rankView(entry leagueEntryDTO) *RankView {
	division := entry.Rank
	switch strings.ToUpper(entry.Tier) {
	case "MASTER", "GRANDMASTER", "CHALLENGER":
		division = ""
	}
	games := entry.Wins + entry.Losses
	winRate := 0
	if games > 0 {
		winRate = int(math.Round(float64(entry.Wins) * 100 / float64(games)))
	}
	return &RankView{
		Tier:           entry.Tier,
		Division:       division,
		LeaguePoints:   entry.LeaguePoints,
		Wins:           entry.Wins,
		Losses:         entry.Losses,
		WinRatePercent: winRate,
	}
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
			dto, err := c.lookupMatchDTO(ctx, region, id)
			if err != nil {
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

func (c *RiotClient) lookupMatchDTO(ctx context.Context, region, matchID string) (matchDTO, error) {
	key := strings.ToUpper(strings.TrimSpace(matchID))
	c.matchCacheMu.RLock()
	dto, ok := c.matchCache[key]
	c.matchCacheMu.RUnlock()
	if ok {
		return dto, nil
	}

	endpoint := c.RegionalBaseURL(region) + "/lol/match/v5/matches/" + url.PathEscape(matchID)
	if err := c.getJSON(ctx, endpoint, &dto); err != nil {
		return matchDTO{}, err
	}
	if dto.Metadata.MatchID == "" {
		dto.Metadata.MatchID = matchID
	}
	c.matchCacheMu.Lock()
	if c.matchCache == nil {
		c.matchCache = make(map[string]matchDTO)
	}
	c.matchCache[key] = dto
	c.matchCacheMu.Unlock()
	return dto, nil
}

func (c *RiotClient) getJSON(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("Riot services are temporarily unavailable.")
	}
	req.Header.Set("X-Riot-Token", c.APIKey)
	if err := c.waitForRequestSlot(ctx, req.URL.Host); err != nil {
		return err
	}
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

func (c *RiotClient) waitForRequestSlot(ctx context.Context, host string) error {
	if c.MinRequestInterval <= 0 {
		return nil
	}
	now := time.Now()
	c.requestMu.Lock()
	if c.nextRequest == nil {
		c.nextRequest = make(map[string]time.Time)
	}
	slot := c.nextRequest[host]
	if slot.Before(now) {
		slot = now
	}
	c.nextRequest[host] = slot.Add(c.MinRequestInterval)
	c.requestMu.Unlock()

	wait := time.Until(slot)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *RiotClient) matchView(dto matchDTO, searchedPUUID string, now time.Time) MatchView {
	version := majorMinorVersion(dto.Info.GameVersion)
	if version == "" {
		version = c.DataDragonVer
	}
	var player participantDTO
	for _, p := range dto.Info.Participants {
		if p.PUUID == searchedPUUID {
			player = p
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
		CS:                    player.TotalMinionsKilled + player.NeutralMinionsKilled,
		Gold:                  player.GoldEarned,
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
	return view
}

func (c *RiotClient) matchDetailView(dto matchDTO, me, region string, now time.Time) MatchDetailView {
	version := majorMinorVersion(dto.Info.GameVersion)
	if version == "" {
		version = c.DataDragonVer
	}
	team1ID := 100
	maxDamage := 0
	for _, p := range dto.Info.Participants {
		if strings.EqualFold(displayRiotID(p), me) {
			team1ID = p.TeamID
		}
		if p.TotalDamageDealtToChampions > maxDamage {
			maxDamage = p.TotalDamageDealtToChampions
		}
	}
	view := MatchDetailView{
		MatchID:       dto.Metadata.MatchID,
		GameModeLabel: queueLabel(dto.Info.QueueID),
		DurationLabel: durationLabel(dto.Info.GameDuration),
		TimeAgoLabel:  timeAgoLabel(time.UnixMilli(dto.Info.GameCreation), now),
	}
	for _, p := range dto.Info.Participants {
		player := c.playerStatsView(version, p, me, region, maxDamage)
		if p.TeamID == team1ID {
			if len(view.Team1.Players) == 0 {
				view.Team1.Win = p.Win
			}
			view.Team1.TotalKills += player.Kills
			view.Team1.TotalDeaths += player.Deaths
			view.Team1.TotalAssists += player.Assists
			view.Team1.TotalGold += player.Gold
			view.Team1.Players = append(view.Team1.Players, player)
		} else {
			if len(view.Team2.Players) == 0 {
				view.Team2.Win = p.Win
			}
			view.Team2.TotalKills += player.Kills
			view.Team2.TotalDeaths += player.Deaths
			view.Team2.TotalAssists += player.Assists
			view.Team2.TotalGold += player.Gold
			view.Team2.Players = append(view.Team2.Players, player)
		}
	}
	return view
}

func (c *RiotClient) playerStatsView(version string, p participantDTO, me, region string, maxDamage int) PlayerStatsView {
	damagePercent := 0
	if maxDamage > 0 {
		damagePercent = int(math.Round(float64(p.TotalDamageDealtToChampions) * 100 / float64(maxDamage)))
		if damagePercent < 0 {
			damagePercent = 0
		}
		if damagePercent > 100 {
			damagePercent = 100
		}
	}
	view := PlayerStatsView{
		RiotID:                displayRiotID(p),
		Region:                region,
		ChampionName:          p.ChampionName,
		ChampionIconURL:       c.championURL(version, p.ChampionName),
		Kills:                 p.Kills,
		Deaths:                p.Deaths,
		Assists:               p.Assists,
		CS:                    p.TotalMinionsKilled + p.NeutralMinionsKilled,
		Gold:                  p.GoldEarned,
		Damage:                p.TotalDamageDealtToChampions,
		DamagePercent:         damagePercent,
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
		Cache:       NewSearchCache(),
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
