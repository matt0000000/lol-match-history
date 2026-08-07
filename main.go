package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultDataDragonVersion = "16.14.1"
const defaultMatchCount = 20
const maxMatchCount = 100
const activeGameCacheTTL = 30 * time.Second

var errNotInLiveGame = errors.New("This player is no longer in a live game.")

// riotVerificationToken proves domain ownership for the Riot Developer Portal
// API key application. Safe to remove once the application is approved.
const riotVerificationToken = "78f3e35f-b152-4401-b2bb-1d2ffecdc690"

//go:embed web/templates/*.tmpl web/static/*
var webFiles embed.FS

// styleVersion is a content hash of the stylesheet, appended to its URL so a
// changed stylesheet always gets a URL no browser or CDN has cached before.
// Without it, clients keep serving a stale stylesheet after a redeploy.
var styleVersion = hashEmbeddedFile("web/static/style.css")

func hashEmbeddedFile(name string) string {
	data, err := webFiles.ReadFile(name)
	if err != nil {
		return "dev"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:4])
}

// parseTemplates parses the embedded templates with the helpers they rely on.
// Tests use it too, so they exercise the same template set the server does.
func parseTemplates() (*template.Template, error) {
	return template.New("").Funcs(template.FuncMap{
		"styleURL":            func() string { return "/static/style.css?v=" + styleVersion },
		"formatDecimal":       formatDecimal,
		"formatSignedDecimal": formatSignedDecimal,
	}).ParseFS(webFiles, "web/templates/*.tmpl")
}

func formatDecimal(value *float64) string {
	if value == nil {
		return "—"
	}
	return strconv.FormatFloat(*value, 'f', 1, 64)
}

func formatSignedDecimal(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%+.1f", *value)
}

type PageData struct {
	Query             string
	Region            string
	Error             string
	LastUpdatedLabel  string
	Profile           *ProfileView
	RecentSummary     *RecentSummaryView
	Matches           []MatchView
	RequestedMatches  int
	NextMatchCount    int
	CanLoadMore       bool
	SupportsExpansion bool
	LiveGameURL       string
}

type RecentSummaryView struct {
	Games                int
	AverageGoldDeltaAt15 *float64
	AverageXPDeltaAt15   *float64
	AverageCSDeltaAt15   *float64
	DeathsPer10Minutes   *float64
	Champions            []ChampionSummaryView
	Roles                []RoleSummaryView
}

type ChampionSummaryView struct {
	ChampionName       string
	ChampionIconURL    string
	Games              int
	Wins               int
	WinRatePercent     int
	AverageKDA         float64
	AverageCSPerMinute *float64
}

type RoleSummaryView struct {
	Role           string
	Games          int
	Wins           int
	WinRatePercent int
}

type ProfileView struct {
	PUUID          string
	GameName       string
	TagLine        string
	ProfileIconURL string
	SummonerLevel  int
	SoloRank       *RankView
	FlexRank       *RankView
}

type LivePageData struct {
	Query  string
	Region string
	Error  string
	Game   *LiveGameView
}

type LiveGameView struct {
	GameID            int64
	QueueLabel        string
	RankQueueLabel    string
	GameLengthSeconds int
	GameLengthLabel   string
	Ranked            bool
	Team1             LiveTeamView
	Team2             LiveTeamView
}

type LiveTeamView struct {
	TeamID  int
	Ranked  bool
	Players []LivePlayerView
}

type LivePlayerView struct {
	PUUID            string
	RiotID           string
	ChampionName     string
	ChampionIconURL  string
	IsSearchedPlayer bool
	Rank             *RankView
	RankError        string
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
	MatchID                   string
	Win                       bool
	GameModeLabel             string
	DurationLabel             string
	TimeAgoLabel              string
	ChampionName              string
	ChampionIconURL           string
	RoleLabel                 string
	Kills                     int
	Deaths                    int
	Assists                   int
	CS                        int
	CSPerMinute               *float64
	LaneMinionsFirst10Minutes *int
	KillParticipationPercent  *int
	Gold                      int
	GoldDeltaAt15             *int
	XPDeltaAt15               *int
	CSDeltaAt15               *int
	DurationSeconds           float64
	ItemIconURLs              []string
	SummonerSpellIconURLs     []string
}

type PerformanceLabelView struct {
	Text        string
	Tone        string
	Description string
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
	Objectives   ObjectiveView
	Players      []PlayerStatsView
}

type ObjectiveView struct {
	Towers  int
	Dragons int
	Barons  int
	Heralds int
	Grubs   int
}

type PlayerStatsView struct {
	RiotID                    string
	Region                    string
	ChampionName              string
	ChampionIconURL           string
	ChampionLevel             int
	Kills                     int
	Deaths                    int
	Assists                   int
	CS                        int
	CSPerMinute               *float64
	LaneMinionsFirst10Minutes *int
	KillParticipationPercent  *int
	Gold                      int
	GoldPerMinute             *float64
	Damage                    int
	DamagePercent             int
	DamageSharePercent        *int
	DamagePerMinute           *float64
	VisionScore               int
	VisionPerMinute           *float64
	ControlWards              int
	ObjectiveDamage           int
	TurretDamage              int
	PerformanceLabels         []PerformanceLabelView
	ItemIconURLs              []string
	SummonerSpellIconURLs     []string
	IsHighlighted             bool
}

type Searcher interface {
	Search(context.Context, string, string, time.Time) (*ProfileView, []MatchView, error)
}

type MatchCountSearcher interface {
	SearchCount(context.Context, string, string, time.Time, int) (*ProfileView, []MatchView, error)
}

type MatchLoader interface {
	MatchDetail(context.Context, string, string, time.Time) (*MatchDetailView, error)
}

type LiveGameChecker interface {
	HasLiveGame(context.Context, string, string) (bool, error)
}

type LiveGameLoader interface {
	LoadLiveGame(context.Context, string, string) (*LiveGameView, error)
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
	LiveChecker LiveGameChecker
	LiveLoader  LiveGameLoader
	Cache       *SearchCache
	Now         func() time.Time
	StaticFS    fs.FS
	Logger      *log.Logger
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	if a.StaticFS != nil {
		staticHandler := http.StripPrefix("/static/", http.FileServerFS(a.StaticFS))
		mux.Handle("GET /static/", requireStaticRevalidation(staticHandler))
	}
	mux.HandleFunc("GET /riot.txt", handleRiotVerification)
	mux.HandleFunc("GET /live/{region}/{puuid}", a.handleLiveGame)
	mux.HandleFunc("GET /match/{id}", a.handleMatchDetail)
	mux.HandleFunc("GET /", a.handleIndex)
	return mux
}

func (a *App) handleLiveGame(w http.ResponseWriter, r *http.Request) {
	data := LivePageData{
		Query:  strings.TrimSpace(r.URL.Query().Get("me")),
		Region: strings.ToLower(strings.TrimSpace(r.PathValue("region"))),
	}
	puuid := strings.TrimSpace(r.PathValue("puuid"))
	if !supportedRegion(data.Region) || puuid == "" {
		data.Error = "Invalid live-game link."
	} else if a.LiveLoader == nil {
		data.Error = "Live-game details are temporarily unavailable."
	} else {
		game, err := a.LiveLoader.LoadLiveGame(r.Context(), data.Region, puuid)
		if err != nil {
			data.Error = err.Error()
			if a.Logger != nil {
				a.Logger.Printf("live game for %q in %s: %v", puuid, data.Region, err)
			}
		} else {
			data.Game = game
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.Templates.ExecuteTemplate(w, "liveLayout", data); err != nil && a.Logger != nil {
		a.Logger.Printf("render live game for %q: %v", puuid, err)
	}
}

func requireStaticRevalidation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		next.ServeHTTP(w, r)
	})
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
		Query:            strings.TrimSpace(r.URL.Query().Get("q")),
		Region:           strings.ToLower(strings.TrimSpace(r.URL.Query().Get("region"))),
		RequestedMatches: requestedMatchCount(r.URL.Query().Get("count")),
	}
	if data.Region == "" {
		data.Region = "na1"
	}
	_, data.SupportsExpansion = a.Searcher.(MatchCountSearcher)
	if data.Query != "" {
		if !supportedRegion(data.Region) {
			data.Error = "Choose a supported region."
		} else if _, _, err := parseRiotID(data.Query); err != nil {
			data.Error = "Enter a Riot ID in the form Name#Tag."
		} else {
			cached, hasCached := a.Cache.Get(data.Query, data.Region)
			refresh := r.URL.Query().Get("refresh") == "1"
			needsMore := data.SupportsExpansion && hasCached && len(cached.Matches) < data.RequestedMatches
			if hasCached && !refresh && !needsMore {
				applySnapshot(&data, cached, now)
			} else if a.Searcher == nil {
				data.Error = "Search is temporarily unavailable."
				if hasCached {
					applySnapshot(&data, cached, now)
				}
			} else {
				profile, matches, err := searchWithCount(r.Context(), a.Searcher, data.Query, data.Region, now, data.RequestedMatches)
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
	if data.Profile != nil && data.Profile.PUUID != "" && a.LiveChecker != nil {
		active, err := a.LiveChecker.HasLiveGame(r.Context(), data.Region, data.Profile.PUUID)
		if err != nil {
			if a.Logger != nil {
				a.Logger.Printf("check live game for %q in %s: %v", data.Profile.PUUID, data.Region, err)
			}
		} else if active {
			me := data.Profile.GameName + "#" + data.Profile.TagLine
			data.LiveGameURL = "/live/" + url.PathEscape(data.Region) + "/" + url.PathEscape(data.Profile.PUUID) + "?me=" + url.QueryEscape(me)
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
	data.RecentSummary = recentSummary(snapshot.Matches)
	data.LastUpdatedLabel = "Updated " + timeAgoLabel(snapshot.UpdatedAt, now)
	loaded := len(snapshot.Matches)
	data.RequestedMatches = max(data.RequestedMatches, loaded)
	if data.SupportsExpansion && loaded < maxMatchCount && loaded > 0 {
		data.CanLoadMore = true
		if data.RequestedMatches > loaded {
			data.NextMatchCount = data.RequestedMatches
		} else {
			data.NextMatchCount = min(maxMatchCount, loaded+10)
		}
	}
}

func requestedMatchCount(raw string) int {
	count, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || count < defaultMatchCount {
		return defaultMatchCount
	}
	return min(count, maxMatchCount)
}

func searchWithCount(ctx context.Context, searcher Searcher, riotID, region string, now time.Time, count int) (*ProfileView, []MatchView, error) {
	if expanded, ok := searcher.(MatchCountSearcher); ok {
		return expanded.SearchCount(ctx, riotID, region, now, count)
	}
	return searcher.Search(ctx, riotID, region, now)
}

func recentSummary(matches []MatchView) *RecentSummaryView {
	if len(matches) == 0 {
		return nil
	}

	summary := &RecentSummaryView{Games: len(matches)}
	champions := make(map[string]*championSummaryAggregate)
	roles := make(map[string]*roleSummaryAggregate)
	var deaths int
	var durationSeconds float64
	var goldDeltaTotal, xpDeltaTotal, csDeltaTotal int
	var goldDeltaGames, xpDeltaGames, csDeltaGames int

	for i, match := range matches {
		if match.DurationSeconds > 0 {
			deaths += match.Deaths
			durationSeconds += match.DurationSeconds
		}
		if match.GoldDeltaAt15 != nil {
			goldDeltaTotal += *match.GoldDeltaAt15
			goldDeltaGames++
		}
		if match.XPDeltaAt15 != nil {
			xpDeltaTotal += *match.XPDeltaAt15
			xpDeltaGames++
		}
		if match.CSDeltaAt15 != nil {
			csDeltaTotal += *match.CSDeltaAt15
			csDeltaGames++
		}
		if match.ChampionName != "" {
			agg := champions[match.ChampionName]
			if agg == nil {
				agg = &championSummaryAggregate{ChampionName: match.ChampionName, ChampionIconURL: match.ChampionIconURL, FirstSeen: i}
				champions[match.ChampionName] = agg
			}
			agg.Games++
			if match.Win {
				agg.Wins++
			}
			agg.Kills += match.Kills
			agg.Deaths += match.Deaths
			agg.Assists += match.Assists
			if match.CSPerMinute != nil {
				agg.CSPerMinuteTotal += *match.CSPerMinute
				agg.CSPerMinuteGames++
			}
		}
		if match.RoleLabel != "" && match.RoleLabel != "Unknown" {
			agg := roles[match.RoleLabel]
			if agg == nil {
				agg = &roleSummaryAggregate{Role: match.RoleLabel, FirstSeen: i}
				roles[match.RoleLabel] = agg
			}
			agg.Games++
			if match.Win {
				agg.Wins++
			}
		}
	}

	if goldDeltaGames > 0 {
		average := float64(goldDeltaTotal) / float64(goldDeltaGames)
		summary.AverageGoldDeltaAt15 = &average
	}
	if xpDeltaGames > 0 {
		average := float64(xpDeltaTotal) / float64(xpDeltaGames)
		summary.AverageXPDeltaAt15 = &average
	}
	if csDeltaGames > 0 {
		average := float64(csDeltaTotal) / float64(csDeltaGames)
		summary.AverageCSDeltaAt15 = &average
	}
	if durationSeconds > 0 {
		rate := float64(deaths) * 10 * 60 / float64(durationSeconds)
		summary.DeathsPer10Minutes = &rate
	}
	summary.Champions = championSummaryViews(champions)
	summary.Roles = roleSummaryViews(roles)
	return summary
}

type championSummaryAggregate struct {
	ChampionName, ChampionIconURL                  string
	Games, Wins, Kills, Deaths, Assists, FirstSeen int
	CSPerMinuteTotal                               float64
	CSPerMinuteGames                               int
}

func championSummaryViews(aggregates map[string]*championSummaryAggregate) []ChampionSummaryView {
	ordered := make([]*championSummaryAggregate, 0, len(aggregates))
	for _, aggregate := range aggregates {
		ordered = append(ordered, aggregate)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Games != ordered[j].Games {
			return ordered[i].Games > ordered[j].Games
		}
		return ordered[i].FirstSeen < ordered[j].FirstSeen
	})
	if len(ordered) > 3 {
		ordered = ordered[:3]
	}
	views := make([]ChampionSummaryView, 0, len(ordered))
	for _, aggregate := range ordered {
		view := ChampionSummaryView{
			ChampionName: aggregate.ChampionName, ChampionIconURL: aggregate.ChampionIconURL,
			Games: aggregate.Games, Wins: aggregate.Wins,
			WinRatePercent: int(math.Round(float64(aggregate.Wins) * 100 / float64(aggregate.Games))),
			AverageKDA:     float64(aggregate.Kills+aggregate.Assists) / float64(max(aggregate.Deaths, 1)),
		}
		if aggregate.CSPerMinuteGames > 0 {
			average := aggregate.CSPerMinuteTotal / float64(aggregate.CSPerMinuteGames)
			view.AverageCSPerMinute = &average
		}
		views = append(views, view)
	}
	return views
}

type roleSummaryAggregate struct {
	Role                   string
	Games, Wins, FirstSeen int
}

func roleSummaryViews(aggregates map[string]*roleSummaryAggregate) []RoleSummaryView {
	ordered := make([]*roleSummaryAggregate, 0, len(aggregates))
	for _, aggregate := range aggregates {
		ordered = append(ordered, aggregate)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Games != ordered[j].Games {
			return ordered[i].Games > ordered[j].Games
		}
		return ordered[i].FirstSeen < ordered[j].FirstSeen
	})
	views := make([]RoleSummaryView, 0, len(ordered))
	for _, aggregate := range ordered {
		views = append(views, RoleSummaryView{
			Role: aggregate.Role, Games: aggregate.Games, Wins: aggregate.Wins,
			WinRatePercent: int(math.Round(float64(aggregate.Wins) * 100 / float64(aggregate.Games))),
		})
	}
	return views
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
	timelineCacheMu    sync.RWMutex
	timelineCache      map[string]timelineDTO
	activeGameCacheMu  sync.RWMutex
	activeGameCache    map[string]activeGameCacheEntry
	championCatalogMu  sync.RWMutex
	championCatalog    map[int]championCatalogEntry
}

type activeGameCacheEntry struct {
	Game      spectatorGameDTO
	Active    bool
	ExpiresAt time.Time
}

type spectatorGameDTO struct {
	GameID            int64                     `json:"gameId"`
	GameLength        int                       `json:"gameLength"`
	GameQueueConfigID int                       `json:"gameQueueConfigId"`
	Participants      []spectatorParticipantDTO `json:"participants"`
}

type spectatorParticipantDTO struct {
	PUUID        string `json:"puuid"`
	RiotID       string `json:"riotId"`
	SummonerName string `json:"summonerName"`
	ChampionID   int    `json:"championId"`
	TeamID       int    `json:"teamId"`
}

type championCatalogEntry struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
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
		Teams        []teamDTO        `json:"teams"`
	} `json:"info"`
}

type teamDTO struct {
	TeamID     int `json:"teamId"`
	Objectives struct {
		Tower      objectiveDTO `json:"tower"`
		Dragon     objectiveDTO `json:"dragon"`
		Baron      objectiveDTO `json:"baron"`
		RiftHerald objectiveDTO `json:"riftHerald"`
		Horde      objectiveDTO `json:"horde"`
	} `json:"objectives"`
}

type objectiveDTO struct {
	Kills int `json:"kills"`
}

type participantDTO struct {
	PUUID                       string                   `json:"puuid"`
	TeamID                      int                      `json:"teamId"`
	Win                         bool                     `json:"win"`
	ChampionName                string                   `json:"championName"`
	ChampionLevel               int                      `json:"champLevel"`
	TeamPosition                string                   `json:"teamPosition"`
	Kills                       int                      `json:"kills"`
	Deaths                      int                      `json:"deaths"`
	Assists                     int                      `json:"assists"`
	TotalMinionsKilled          int                      `json:"totalMinionsKilled"`
	NeutralMinionsKilled        int                      `json:"neutralMinionsKilled"`
	GoldEarned                  int                      `json:"goldEarned"`
	TotalDamageDealtToChampions int                      `json:"totalDamageDealtToChampions"`
	DamageDealtToObjectives     int                      `json:"damageDealtToObjectives"`
	DamageDealtToTurrets        int                      `json:"damageDealtToTurrets"`
	VisionScore                 int                      `json:"visionScore"`
	VisionWardsBoughtInGame     int                      `json:"visionWardsBoughtInGame"`
	TurretTakedowns             int                      `json:"turretTakedowns"`
	FirstBloodKill              bool                     `json:"firstBloodKill"`
	FirstBloodAssist            bool                     `json:"firstBloodAssist"`
	ParticipantID               int                      `json:"participantId"`
	TripleKills                 int                      `json:"tripleKills"`
	QuadraKills                 int                      `json:"quadraKills"`
	PentaKills                  int                      `json:"pentaKills"`
	Item0                       int                      `json:"item0"`
	Item1                       int                      `json:"item1"`
	Item2                       int                      `json:"item2"`
	Item3                       int                      `json:"item3"`
	Item4                       int                      `json:"item4"`
	Item5                       int                      `json:"item5"`
	Item6                       int                      `json:"item6"`
	Summoner1ID                 int                      `json:"summoner1Id"`
	Summoner2ID                 int                      `json:"summoner2Id"`
	RiotIDGameName              string                   `json:"riotIdGameName"`
	RiotIDTagLine               string                   `json:"riotIdTagline"`
	Challenges                  participantChallengesDTO `json:"challenges"`
	GaveFirstBlood              bool                     `json:"-"`
}

type timelineDTO struct {
	Info struct {
		Frames []timelineFrameDTO `json:"frames"`
	} `json:"info"`
}

type timelineFrameDTO struct {
	Timestamp         int64                                  `json:"timestamp"`
	ParticipantFrames map[string]timelineParticipantFrameDTO `json:"participantFrames"`
	Events            []timelineEventDTO                     `json:"events"`
}

type timelineParticipantFrameDTO struct {
	TotalGold           int `json:"totalGold"`
	XP                  int `json:"xp"`
	MinionsKilled       int `json:"minionsKilled"`
	JungleMinionsKilled int `json:"jungleMinionsKilled"`
}

type timelineEventDTO struct {
	Type      string `json:"type"`
	Timestamp int64  `json:"timestamp"`
	VictimID  int    `json:"victimId"`
}

type participantChallengesDTO struct {
	LaneMinionsFirst10Minutes *int `json:"laneMinionsFirst10Minutes"`
	SoloKills                 *int `json:"soloKills"`
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
	timeline, _ := c.lookupTimelineDTO(ctx, region, matchID)
	view := c.matchDetailView(dto, me, region, now, firstBloodVictimID(timeline))
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
		MatchCount:         defaultMatchCount,
		MinRequestInterval: 60 * time.Millisecond,
		nextRequest:        make(map[string]time.Time),
		matchCache:         make(map[string]matchDTO),
		timelineCache:      make(map[string]timelineDTO),
		activeGameCache:    make(map[string]activeGameCacheEntry),
	}
}

func (c *RiotClient) Search(ctx context.Context, riotID, region string, now time.Time) (*ProfileView, []MatchView, error) {
	return c.SearchCount(ctx, riotID, region, now, c.MatchCount)
}

func (c *RiotClient) SearchCount(ctx context.Context, riotID, region string, now time.Time, count int) (*ProfileView, []MatchView, error) {
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
	matchIDs, err := c.listMatchIDs(ctx, region, account.PUUID, count)
	if err != nil {
		return nil, nil, err
	}
	matches, err := c.lookupMatches(ctx, region, account.PUUID, matchIDs, now)
	if err != nil {
		return nil, nil, err
	}
	profile := &ProfileView{
		PUUID:          account.PUUID,
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

func (c *RiotClient) HasLiveGame(ctx context.Context, region, puuid string) (bool, error) {
	_, active, err := c.activeGame(ctx, region, puuid)
	return active, err
}

func (c *RiotClient) LoadLiveGame(ctx context.Context, region, searchedPUUID string) (*LiveGameView, error) {
	game, active, err := c.activeGame(ctx, region, searchedPUUID)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, errNotInLiveGame
	}

	queueType, rankLabel, ranked := liveRankQueue(game.GameQueueConfigID)
	length := max(game.GameLength, 0)
	view := &LiveGameView{
		GameID:            game.GameID,
		QueueLabel:        queueLabel(game.GameQueueConfigID),
		RankQueueLabel:    rankLabel,
		GameLengthSeconds: length,
		GameLengthLabel:   durationLabel(length),
		Ranked:            ranked,
		Team1:             LiveTeamView{TeamID: 100, Ranked: ranked, Players: make([]LivePlayerView, 0, 5)},
		Team2:             LiveTeamView{TeamID: 200, Ranked: ranked, Players: make([]LivePlayerView, 0, 5)},
	}

	catalog, _ := c.loadChampionCatalog(ctx)
	players := make([]LivePlayerView, len(game.Participants))
	for i, participant := range game.Participants {
		champion := catalog[participant.ChampionID]
		championName := champion.Name
		if championName == "" {
			championName = fmt.Sprintf("Champion %d", participant.ChampionID)
		}
		championURL := ""
		if champion.ID != "" {
			championURL = c.championURL(c.DataDragonVer, champion.ID)
		}
		players[i] = LivePlayerView{
			PUUID:            participant.PUUID,
			RiotID:           spectatorRiotID(participant),
			ChampionName:     championName,
			ChampionIconURL:  championURL,
			IsSearchedPlayer: participant.PUUID != "" && participant.PUUID == searchedPUUID,
		}
	}

	if ranked {
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		for i, participant := range game.Participants {
			if participant.PUUID == "" {
				players[i].RankError = "Player identity hidden"
				continue
			}
			i, participant := i, participant
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					players[i].RankError = "Rank unavailable"
					return
				}
				entries, lookupErr := c.lookupRanks(ctx, region, participant.PUUID)
				if lookupErr != nil {
					players[i].RankError = "Rank unavailable"
					return
				}
				for _, entry := range entries {
					if entry.QueueType == queueType {
						players[i].Rank = rankView(entry)
						break
					}
				}
			}()
		}
		wg.Wait()
	}

	for i, participant := range game.Participants {
		if participant.TeamID == 200 {
			view.Team2.Players = append(view.Team2.Players, players[i])
		} else {
			view.Team1.Players = append(view.Team1.Players, players[i])
		}
	}
	return view, nil
}

func liveRankQueue(queueID int) (queueType, label string, ranked bool) {
	switch queueID {
	case 420:
		return "RANKED_SOLO_5x5", "Solo/Duo", true
	case 440:
		return "RANKED_FLEX_SR", "Flex 5v5", true
	default:
		return "", "", false
	}
}

func spectatorRiotID(participant spectatorParticipantDTO) string {
	if riotID := strings.TrimSpace(participant.RiotID); riotID != "" {
		return riotID
	}
	if name := strings.TrimSpace(participant.SummonerName); name != "" {
		return name
	}
	return "Hidden player"
}

func (c *RiotClient) activeGame(ctx context.Context, region, puuid string) (spectatorGameDTO, bool, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return spectatorGameDTO{}, false, errors.New("Riot API key is not configured.")
	}
	key := strings.ToLower(strings.TrimSpace(region)) + "\x00" + strings.TrimSpace(puuid)
	now := time.Now()
	c.activeGameCacheMu.RLock()
	cached, ok := c.activeGameCache[key]
	c.activeGameCacheMu.RUnlock()
	if ok && now.Before(cached.ExpiresAt) {
		return cached.Game, cached.Active, nil
	}

	endpoint := c.PlatformBaseURL(region) + "/lol/spectator/v5/active-games/by-summoner/" + url.PathEscape(puuid)
	var game spectatorGameDTO
	found, err := c.getJSONMaybeNotFound(ctx, endpoint, &game)
	if err != nil {
		return spectatorGameDTO{}, false, err
	}
	entry := activeGameCacheEntry{Game: game, Active: found, ExpiresAt: now.Add(activeGameCacheTTL)}
	c.activeGameCacheMu.Lock()
	if c.activeGameCache == nil {
		c.activeGameCache = make(map[string]activeGameCacheEntry)
	}
	c.activeGameCache[key] = entry
	c.activeGameCacheMu.Unlock()
	return game, found, nil
}

func (c *RiotClient) loadChampionCatalog(ctx context.Context) (map[int]championCatalogEntry, error) {
	c.championCatalogMu.RLock()
	if c.championCatalog != nil {
		catalog := c.championCatalog
		c.championCatalogMu.RUnlock()
		return catalog, nil
	}
	c.championCatalogMu.RUnlock()

	var response struct {
		Data map[string]championCatalogEntry `json:"data"`
	}
	endpoint := c.DataDragonBase + "/cdn/" + c.DataDragonVer + "/data/en_US/champion.json"
	if err := c.getPublicJSON(ctx, endpoint, &response); err != nil {
		return nil, err
	}
	catalog := make(map[int]championCatalogEntry, len(response.Data))
	for _, champion := range response.Data {
		id, err := strconv.Atoi(champion.Key)
		if err == nil {
			catalog[id] = champion
		}
	}
	c.championCatalogMu.Lock()
	if c.championCatalog == nil {
		c.championCatalog = catalog
	}
	catalog = c.championCatalog
	c.championCatalogMu.Unlock()
	return catalog, nil
}

func (c *RiotClient) listMatchIDs(ctx context.Context, region, puuid string, count int) ([]string, error) {
	if count <= 0 {
		count = defaultMatchCount
	}
	count = min(count, maxMatchCount)
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
			timeline, _ := c.lookupTimelineDTO(ctx, region, id)
			views[i] = c.matchView(dto, timeline, puuid, now)
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

func (c *RiotClient) lookupTimelineDTO(ctx context.Context, region, matchID string) (timelineDTO, error) {
	key := strings.ToUpper(strings.TrimSpace(matchID))
	c.timelineCacheMu.RLock()
	timeline, ok := c.timelineCache[key]
	c.timelineCacheMu.RUnlock()
	if ok {
		return timeline, nil
	}

	endpoint := c.RegionalBaseURL(region) + "/lol/match/v5/matches/" + url.PathEscape(matchID) + "/timeline"
	if err := c.getJSON(ctx, endpoint, &timeline); err != nil {
		return timelineDTO{}, err
	}
	c.timelineCacheMu.Lock()
	if c.timelineCache == nil {
		c.timelineCache = make(map[string]timelineDTO)
	}
	c.timelineCache[key] = timeline
	c.timelineCacheMu.Unlock()
	return timeline, nil
}

func firstBloodVictimID(timeline timelineDTO) int {
	var victimID int
	var firstTimestamp int64
	for _, frame := range timeline.Info.Frames {
		for _, event := range frame.Events {
			if event.Type != "CHAMPION_KILL" || event.VictimID <= 0 {
				continue
			}
			if victimID == 0 || event.Timestamp < firstTimestamp {
				victimID = event.VictimID
				firstTimestamp = event.Timestamp
			}
		}
	}
	return victimID
}

func (c *RiotClient) getJSON(ctx context.Context, endpoint string, dst any) error {
	found, err := c.getJSONMaybeNotFound(ctx, endpoint, dst)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("No player found for that Riot ID and region.")
	}
	return nil
}

func (c *RiotClient) getJSONMaybeNotFound(ctx context.Context, endpoint string, dst any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, errors.New("Riot services are temporarily unavailable.")
	}
	req.Header.Set("X-Riot-Token", c.APIKey)
	if err := c.waitForRequestSlot(ctx, req.URL.Host); err != nil {
		return false, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, errors.New("Riot services are temporarily unavailable.")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case http.StatusNotFound:
			return false, nil
		case http.StatusUnauthorized, http.StatusForbidden:
			return false, errors.New("Riot API key is invalid or expired. Replace RIOT_API_KEY and restart the server.")
		case http.StatusTooManyRequests:
			if retry := resp.Header.Get("Retry-After"); retry != "" {
				return false, fmt.Errorf("Riot API rate limit reached. Try again in %s seconds", retry)
			}
			return false, errors.New("Riot API rate limit reached. Try again shortly.")
		default:
			return false, errors.New("Riot services are temporarily unavailable.")
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return false, errors.New("Riot services returned an unexpected response.")
	}
	return true, nil
}

func (c *RiotClient) getPublicJSON(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("Data Dragon is temporarily unavailable.")
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return errors.New("Data Dragon is temporarily unavailable.")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("Data Dragon is temporarily unavailable.")
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return errors.New("Data Dragon returned an unexpected response.")
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

func (c *RiotClient) matchView(dto matchDTO, timeline timelineDTO, searchedPUUID string, now time.Time) MatchView {
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
		MatchID:                   dto.Metadata.MatchID,
		Win:                       player.Win,
		GameModeLabel:             queueLabel(dto.Info.QueueID),
		DurationLabel:             durationLabel(dto.Info.GameDuration),
		TimeAgoLabel:              timeAgoLabel(time.UnixMilli(dto.Info.GameCreation), now),
		ChampionName:              player.ChampionName,
		ChampionIconURL:           c.championURL(version, player.ChampionName),
		RoleLabel:                 roleLabel(player.TeamPosition),
		Kills:                     player.Kills,
		Deaths:                    player.Deaths,
		Assists:                   player.Assists,
		CS:                        player.TotalMinionsKilled + player.NeutralMinionsKilled,
		CSPerMinute:               csPerMinute(player.TotalMinionsKilled+player.NeutralMinionsKilled, dto.Info.GameDuration),
		LaneMinionsFirst10Minutes: player.Challenges.LaneMinionsFirst10Minutes,
		KillParticipationPercent:  killParticipationPercent(player, dto.Info.Participants),
		Gold:                      player.GoldEarned,
		DurationSeconds:           durationSeconds(dto.Info.GameDuration),
		ItemIconURLs:              make([]string, 7),
		SummonerSpellIconURLs:     make([]string, 2),
	}
	view.GoldDeltaAt15, view.XPDeltaAt15, view.CSDeltaAt15 = laneDeltasAt15(player, dto.Info.Participants, timeline)
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

func laneDeltasAt15(player participantDTO, participants []participantDTO, timeline timelineDTO) (*int, *int, *int) {
	position := strings.ToUpper(strings.TrimSpace(player.TeamPosition))
	if player.ParticipantID <= 0 || !isLanePosition(position) {
		return nil, nil, nil
	}
	var opponent participantDTO
	matches := 0
	for _, candidate := range participants {
		if candidate.TeamID != player.TeamID && candidate.ParticipantID > 0 && strings.EqualFold(candidate.TeamPosition, position) {
			opponent = candidate
			matches++
		}
	}
	if matches != 1 {
		return nil, nil, nil
	}

	const target = int64(15 * time.Minute / time.Millisecond)
	const latest = int64(16 * time.Minute / time.Millisecond)
	for _, frame := range timeline.Info.Frames {
		if frame.Timestamp < target {
			continue
		}
		if frame.Timestamp > latest {
			break
		}
		playerFrame, playerOK := frame.ParticipantFrames[strconv.Itoa(player.ParticipantID)]
		opponentFrame, opponentOK := frame.ParticipantFrames[strconv.Itoa(opponent.ParticipantID)]
		if !playerOK || !opponentOK {
			return nil, nil, nil
		}
		gold := playerFrame.TotalGold - opponentFrame.TotalGold
		xp := playerFrame.XP - opponentFrame.XP
		cs := playerFrame.MinionsKilled - opponentFrame.MinionsKilled
		if position == "JUNGLE" {
			cs = playerFrame.JungleMinionsKilled - opponentFrame.JungleMinionsKilled
		}
		return &gold, &xp, &cs
	}
	return nil, nil, nil
}

func isLanePosition(position string) bool {
	switch position {
	case "TOP", "JUNGLE", "MIDDLE", "BOTTOM", "UTILITY":
		return true
	default:
		return false
	}
}

func (c *RiotClient) matchDetailView(dto matchDTO, me, region string, now time.Time, firstBloodVictimID int) MatchDetailView {
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
		p.GaveFirstBlood = firstBloodVictimID > 0 && p.ParticipantID == firstBloodVictimID
		player := c.playerStatsView(version, p, dto.Info.Participants, dto.Info.GameDuration, me, region, maxDamage)
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
	for _, team := range dto.Info.Teams {
		objectives := ObjectiveView{
			Towers: team.Objectives.Tower.Kills, Dragons: team.Objectives.Dragon.Kills,
			Barons: team.Objectives.Baron.Kills, Heralds: team.Objectives.RiftHerald.Kills,
			Grubs: team.Objectives.Horde.Kills,
		}
		if team.TeamID == team1ID {
			view.Team1.Objectives = objectives
		} else {
			view.Team2.Objectives = objectives
		}
	}
	return view
}

func roleLabel(position string) string {
	switch strings.ToUpper(strings.TrimSpace(position)) {
	case "TOP":
		return "Top"
	case "JUNGLE":
		return "Jungle"
	case "MIDDLE":
		return "Mid"
	case "BOTTOM":
		return "AD Carry"
	case "UTILITY":
		return "Support"
	default:
		return "Unknown"
	}
}

func derivePerformanceLabels(player participantDTO, participants []participantDTO, duration int) []PerformanceLabelView {
	labels := make([]PerformanceLabelView, 0)
	add := func(text, tone string) {
		labels = append(labels, PerformanceLabelView{Text: text, Tone: tone, Description: performanceLabelDescription(text)})
	}
	role := roleLabel(player.TeamPosition)

	if delta := csDeltaFirst10Minutes(player, participants); delta != nil {
		switch {
		case *delta >= 20:
			add("lane bully", "good")
		case *delta >= 10:
			add("strong lane", "good")
		case *delta <= -20:
			add("crushed in lane", "bad")
		case *delta <= -10:
			add("weak lane", "bad")
		}
	}
	csMinute := csPerMinute(player.TotalMinionsKilled+player.NeutralMinionsKilled, duration)
	if csMinute != nil {
		switch {
		case *csMinute >= 8:
			add("farm machine", "good")
		case *csMinute >= 7:
			add("good farming", "good")
		case *csMinute < 5 && durationSeconds(duration) >= 15*60 && role != "Support" && role != "Unknown":
			add("low farm", "bad")
		}
	}
	if player.Challenges.LaneMinionsFirst10Minutes != nil && *player.Challenges.LaneMinionsFirst10Minutes >= 80 {
		add("early farmer", "good")
	}

	kp := killParticipationPercent(player, participants)
	if kp != nil {
		switch {
		case *kp >= 80:
			add("everywhere", "good")
		case *kp >= 70:
			add("high participation", "good")
		case *kp <= 35:
			add("low participation", "bad")
		}
	}
	kda := float64(player.Kills+player.Assists) / float64(max(player.Deaths, 1))
	if player.Kills >= 10 && kda >= 4 && kp != nil && *kp >= 60 {
		add("carry performance", "good")
	}
	if player.Deaths <= 1 && kp != nil && *kp >= 50 {
		add("untouchable", "good")
	}
	if kda >= 5 && player.Kills+player.Assists >= 10 {
		add("high kda", "good")
	}
	if player.Kills >= 12 {
		add("bloodthirsty", "good")
	}
	if player.Assists >= 15 {
		add("team player", "good")
	}
	if player.Deaths == 0 && player.Kills+player.Assists > 0 {
		add("deathless", "good")
	}
	if player.Deaths >= 10 {
		add("rough game", "bad")
	}

	damageShare := teamDamageSharePercent(player, participants)
	if damageShare != nil {
		switch {
		case *damageShare >= 30:
			add("damage carry", "good")
		case *damageShare >= 25:
			add("heavy hitter", "good")
		case *damageShare < 10 && durationSeconds(duration) >= 15*60 && role != "Support" && role != "Unknown":
			add("low damage", "bad")
		}
	}
	if player.GoldEarned > 0 && player.TotalDamageDealtToChampions >= 15000 && float64(player.TotalDamageDealtToChampions)/float64(player.GoldEarned) >= 1.8 {
		add("gold efficient", "good")
	}
	if player.DamageDealtToObjectives >= 10000 {
		add("objective focused", "good")
	}
	if player.DamageDealtToTurrets >= 5000 || player.TurretTakedowns >= 3 {
		add("tower pusher", "good")
	}

	if good, bad, ok := visionThresholds(role); ok && player.VisionScore > 0 && durationSeconds(duration) >= 15*60 {
		if visionMinute := statPerMinute(player.VisionScore, duration); visionMinute != nil {
			if *visionMinute >= good {
				add("visionary", "good")
			} else if *visionMinute <= bad {
				add("poor vision", "bad")
			}
		}
	}
	if player.VisionWardsBoughtInGame >= 3 {
		add("control ward buyer", "good")
	}
	if player.VisionWardsBoughtInGame == 0 && role != "Unknown" {
		add("no control wards", "bad")
	}
	if player.FirstBloodKill || player.FirstBloodAssist {
		add("first blood", "neutral")
	}
	if player.GaveFirstBlood {
		add("gave first blood", "bad")
	}
	if player.Challenges.SoloKills != nil && *player.Challenges.SoloKills >= 2 {
		add("solo killer", "good")
	}
	switch {
	case player.PentaKills > 0:
		add("pentakill", "good")
	case player.QuadraKills > 0:
		add("quadra kill", "good")
	case player.TripleKills > 0:
		add("triple kill", "good")
	}
	return labels
}

var performanceLabelDescriptions = map[string]string{
	"lane bully":         "Finished 10 minutes at least 20 CS ahead of the lane opponent.",
	"strong lane":        "Finished 10 minutes 10–19 CS ahead of the lane opponent.",
	"crushed in lane":    "Finished 10 minutes at least 20 CS behind the lane opponent.",
	"weak lane":          "Finished 10 minutes 10–19 CS behind the lane opponent.",
	"farm machine":       "Averaged at least 8 CS per minute.",
	"good farming":       "Averaged between 7 and 8 CS per minute.",
	"low farm":           "Averaged fewer than 5 CS per minute outside the support role.",
	"early farmer":       "Collected at least 80 lane minions in the first 10 minutes.",
	"everywhere":         "Participated in at least 80% of the team’s champion kills.",
	"high participation": "Participated in 70–79% of the team’s champion kills.",
	"low participation":  "Participated in no more than 35% of the team’s champion kills.",
	"carry performance":  "Had at least 10 kills, 4 KDA, and 60% kill participation.",
	"untouchable":        "Died at most once while participating in at least half of the team’s kills.",
	"high kda":           "Finished with at least 5 KDA and 10 combined kills and assists.",
	"bloodthirsty":       "Finished with at least 12 kills.",
	"team player":        "Finished with at least 15 assists.",
	"deathless":          "Finished the match without dying.",
	"rough game":         "Died at least 10 times.",
	"damage carry":       "Dealt at least 30% of the team’s champion damage.",
	"heavy hitter":       "Dealt 25–29% of the team’s champion damage.",
	"low damage":         "Dealt under 10% of team champion damage outside support or unknown roles.",
	"gold efficient":     "Dealt at least 15,000 champion damage at 1.8 damage per gold earned.",
	"objective focused":  "Dealt at least 10,000 damage to neutral and structure objectives.",
	"tower pusher":       "Dealt at least 5,000 turret damage or participated in three turret takedowns.",
	"visionary":          "Recorded strong vision per minute for the assigned role.",
	"poor vision":        "Recorded low vision per minute for the assigned role.",
	"control ward buyer": "Bought at least three control wards.",
	"no control wards":   "Finished the match without buying a control ward.",
	"first blood":        "Participated in the match’s first champion kill.",
	"gave first blood":   "Was the first player killed in the match.",
	"solo killer":        "Recorded at least two solo kills.",
	"pentakill":          "Recorded a pentakill.",
	"quadra kill":        "Recorded a quadra kill.",
	"triple kill":        "Recorded a triple kill.",
}

func performanceLabelDescription(label string) string {
	return performanceLabelDescriptions[label]
}

func visionThresholds(role string) (good, bad float64, ok bool) {
	switch role {
	case "Support":
		return 1.5, 0.8, true
	case "Jungle":
		return 1.0, 0.5, true
	case "Top", "Mid", "AD Carry":
		return 0.7, 0.3, true
	default:
		return 0, 0, false
	}
}

func (c *RiotClient) playerStatsView(version string, p participantDTO, participants []participantDTO, duration int, me, region string, maxDamage int) PlayerStatsView {
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
		RiotID:                    displayRiotID(p),
		Region:                    region,
		ChampionName:              p.ChampionName,
		ChampionIconURL:           c.championURL(version, p.ChampionName),
		ChampionLevel:             p.ChampionLevel,
		Kills:                     p.Kills,
		Deaths:                    p.Deaths,
		Assists:                   p.Assists,
		CS:                        p.TotalMinionsKilled + p.NeutralMinionsKilled,
		CSPerMinute:               csPerMinute(p.TotalMinionsKilled+p.NeutralMinionsKilled, duration),
		LaneMinionsFirst10Minutes: p.Challenges.LaneMinionsFirst10Minutes,
		KillParticipationPercent:  killParticipationPercent(p, participants),
		Gold:                      p.GoldEarned,
		GoldPerMinute:             statPerMinute(p.GoldEarned, duration),
		Damage:                    p.TotalDamageDealtToChampions,
		DamagePercent:             damagePercent,
		DamageSharePercent:        teamDamageSharePercent(p, participants),
		DamagePerMinute:           statPerMinute(p.TotalDamageDealtToChampions, duration),
		VisionScore:               p.VisionScore,
		VisionPerMinute:           statPerMinute(p.VisionScore, duration),
		ControlWards:              p.VisionWardsBoughtInGame,
		ObjectiveDamage:           p.DamageDealtToObjectives,
		TurretDamage:              p.DamageDealtToTurrets,
		PerformanceLabels:         derivePerformanceLabels(p, participants, duration),
		ItemIconURLs:              make([]string, 7),
		SummonerSpellIconURLs:     make([]string, 2),
		IsHighlighted:             me != "" && strings.EqualFold(displayRiotID(p), me),
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

func csDeltaFirst10Minutes(player participantDTO, participants []participantDTO) *int {
	if player.Challenges.LaneMinionsFirst10Minutes == nil || strings.TrimSpace(player.TeamPosition) == "" {
		return nil
	}

	var opponent *participantDTO
	for i := range participants {
		candidate := &participants[i]
		if candidate.TeamID == player.TeamID ||
			!strings.EqualFold(strings.TrimSpace(candidate.TeamPosition), strings.TrimSpace(player.TeamPosition)) {
			continue
		}
		if opponent != nil {
			return nil
		}
		opponent = candidate
	}
	if opponent == nil || opponent.Challenges.LaneMinionsFirst10Minutes == nil {
		return nil
	}

	delta := *player.Challenges.LaneMinionsFirst10Minutes - *opponent.Challenges.LaneMinionsFirst10Minutes
	return &delta
}

func csPerMinute(cs, duration int) *float64 {
	return statPerMinute(cs, duration)
}

func statPerMinute(value, duration int) *float64 {
	if duration <= 0 {
		return nil
	}
	seconds := durationSeconds(duration)
	if seconds <= 0 {
		return nil
	}
	rate := math.Round(float64(value)/(seconds/60)*10) / 10
	return &rate
}

func durationSeconds(duration int) float64 {
	seconds := float64(duration)
	if seconds > 12*60*60 {
		seconds /= 1000
	}
	return seconds
}

func teamDamageSharePercent(player participantDTO, participants []participantDTO) *int {
	total := 0
	for _, participant := range participants {
		if participant.TeamID == player.TeamID {
			total += participant.TotalDamageDealtToChampions
		}
	}
	if total <= 0 {
		return nil
	}
	share := int(math.Round(float64(player.TotalDamageDealtToChampions) * 100 / float64(total)))
	share = max(0, min(100, share))
	return &share
}

func killParticipationPercent(player participantDTO, participants []participantDTO) *int {
	teamKills := 0
	for _, participant := range participants {
		if participant.TeamID == player.TeamID {
			teamKills += participant.Kills
		}
	}
	if teamKills <= 0 {
		return nil
	}
	value := int(math.Round(float64(player.Kills+player.Assists) * 100 / float64(teamKills)))
	if value < 0 {
		value = 0
	}
	if value > 100 {
		value = 100
	}
	return &value
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
	tmpl, err := parseTemplates()
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
		LiveChecker: client,
		LiveLoader:  client,
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
