package scrape

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"football-scraping/internal/db"
	"football-scraping/internal/queue"

	"github.com/dop251/goja"
	"github.com/mxschmitt/playwright-go"
)

var regionsToLeague = map[string]string{
	"Serie-A":        "Italy",
	"La-Liga":        "Spain",
	"Bundesliga":     "Germany",
	"Ligue-1":        "France",
	"Premier-League": "England",
}

var leagueToName = map[string]string{
	"Serie-A":        "Serie A",
	"La-Liga":        "LaLiga",
	"Bundesliga":     "Bundesliga",
	"Ligue-1":        "Ligue 1",
	"Premier-League": "Premier League",
}

type Tournament struct {
	Name string `json:"name" bson:"name"`
	URL  string `json:"url" bson:"url"`
}

type Region struct {
	Name        string       `json:"name" bson:"name"`
	Tournaments []Tournament `json:"tournaments" bson:"tournaments"`
}

type GameEntry struct {
	Venue    string
	Opponent string
	URL      string
}

func saveMapAsJSONFile(filePath string, data map[string]interface{}) error {
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		fmt.Println("Error marshaling JSON:", err)
		return err
	}

	// Save to file
	err = os.WriteFile(filePath, jsonData, 0644)
	if err != nil {
		fmt.Println("Error writing file:", err)
		return err
	}
	return nil
}

func sliceEquality(a, b []playwright.Locator) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func listEquality[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func extractJson(content, target string) map[string]interface{} {

	if strings.Contains(content, target) {
		contentParts := strings.SplitN(content, target+" = ", 2)[1]
		contentParts = strings.TrimRight(contentParts, " \n\r\t;")

		contentParts = fmt.Sprintf("(%s)", contentParts)

		vm := goja.New()

		// Evaluate JS object
		v, err := vm.RunString(contentParts)
		if err != nil {
			panic(err)
		}

		// Export to Go value
		exported := v.Export()

		//return exported.

		return exported.(map[string]interface{})
	}

	return nil
}

func parseHTML(html string) map[string]interface{} {
	re := regexp.MustCompile(`(?s)<script\b[^>]*>(.*?)</script>`)

	matches := re.FindAllStringSubmatch(html, -1)

	result := make(map[string]interface{})
	match_info := make(map[string]interface{})
	match_details := make(map[string]interface{})
	for _, match := range matches {
		content := match[1]
		target := "require.config.params['matchheader']"
		_match_info := extractJson(content, target)

		target = "require.config.params[\"args\"]"
		_match_details := extractJson(content, target)

		if _match_info != nil {
			match_info = _match_info
		}
		if _match_details != nil {
			match_details = _match_details
		}

		match_id := match_info["matchId"]
		result[match_id.(string)] = map[string]interface{}{
			"match_info":    match_info,
			"match_details": match_details,
		}

	}

	return result

}

func createDir(path string) error {
	return os.MkdirAll(path, 0755)
}

func fixTeamName(teamName string) string {
	if !strings.Contains(teamName, "\n") {
		return teamName
	}
	return strings.SplitN(teamName, "\n", 2)[0]
}

func getURL(data []Region, league string) (string, string, error) {
	leagueRegion, ok := regionsToLeague[league]
	if !ok {
		return "", "", fmt.Errorf("unknown league: %s", league)
	}
	leagueName, ok := leagueToName[league]
	if !ok {
		return "", "", fmt.Errorf("unknown league: %s", league)
	}
	for _, region := range data {
		if region.Name == leagueRegion {
			for _, t := range region.Tournaments {
				if t.Name == leagueName {
					return "https://www.whoscored.com" + t.URL, leagueName, nil
				}
			}
		}
	}
	return "", "", fmt.Errorf("could not find URL for league: %s", league)
}

var localeSubdomainRe = regexp.MustCompile(`^https://[a-z]{2}\.whoscored\.com`)

// blockLocaleRedirect rewrites any request to a WhoScored country-code
// subdomain (e.g. it.whoscored.com) back to www.whoscored.com. WhoScored
// geo-redirects based on the request's IP via what looks like a delayed
// client-side redirect - it's inconsistent, and setting Locale on the
// browser context alone doesn't reliably prevent it, so this catches it at
// the network level whenever it does fire.
func blockLocaleRedirect(ctx playwright.BrowserContext) error {
	return ctx.Route("**/*", func(route playwright.Route) {
		reqURL := route.Request().URL()
		if localeSubdomainRe.MatchString(reqURL) {
			newURL := localeSubdomainRe.ReplaceAllString(reqURL, "https://www.whoscored.com")
			if err := route.Continue(playwright.RouteContinueOptions{URL: &newURL}); err != nil {
				log.Printf("could not rewrite locale redirect for %s: %v", reqURL, err)
			}
			return
		}
		if err := route.Continue(); err != nil {
			log.Printf("could not continue request %s: %v", reqURL, err)
		}
	})
}

// strVal safely retrieves a map value as a string.
func strVal(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(val)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// subMap safely retrieves a nested map from a map.
func subMap(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key]; ok {
		if sub, ok := v.(map[string]interface{}); ok {
			return sub
		}
	}
	return map[string]interface{}{}
}

// checkMatchExistence returns true if the JSON file at filePath already contains
// a top-level key equal to source.
func checkMatchExistence(filePath, source string) bool {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return false
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	_, exists := m[source]
	return exists
}

// saveMatchInfoJSONFile merges matchDict under the given source key into the
// JSON file at filePath, creating or updating it as necessary.
func saveMatchInfoJSONFile(filePath, source string, matchDict map[string]interface{}) error {
	existing := map[string]interface{}{}
	if data, err := os.ReadFile(filePath); err == nil {
		json.Unmarshal(data, &existing) //nolint:errcheck
	}
	existing[source] = matchDict
	b, err := json.Marshal(existing)
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, b, 0644)
}

// selectOption keeps selecting the given visible-text value in the <select>
// matched by xpath until it sticks, retrying every 3 seconds.
func selectOption(page playwright.Page, xpath, value string) error {
	locator := page.Locator("xpath=" + xpath)
	for {
		labels := []string{value}
		if _, err := locator.SelectOption(playwright.SelectOptionValues{Labels: &labels}); err != nil {
			return fmt.Errorf("SelectOption failed: %v", err)
		}
		time.Sleep(3 * time.Second)
		curr, err := page.EvalOnSelector("xpath="+xpath, "el => el.options[el.selectedIndex].text", nil)
		if err != nil {
			return fmt.Errorf("could not read selected option: %v", err)
		}
		if fmt.Sprintf("%v", curr) == value {
			break
		}
	}
	return nil
}

// getLeagueMatchCentre is the main scraping entry point.  It navigates WhoScored,
// collects every finished fixture for the given league / season, then fetches and
// stores the match-centre data for each game.
func getLeagueMatchCentre(rootDir, leagueName, season string, filterTeam *string) error {

	mongoClient, err := db.Connect(context.Background())
	if err != nil {
		return fmt.Errorf("could not connect to mongo: %v", err)
	}
	defer mongoClient.Disconnect(context.Background()) //nolint:errcheck

	regions, err := db.LoadRegions[Region](context.Background(), mongoClient)
	if err != nil {
		return fmt.Errorf("could not load regions: %v", err)
	}

	url, displayLeague, err := getURL(regions, leagueName)
	if err != nil {
		return err
	}
	fmt.Println(url, displayLeague)

	dataDir := filepath.Join(rootDir, season, leagueName)
	createDir(dataDir)

	// Start playwright
	pw, err := playwright.Run()
	if err != nil {
		return fmt.Errorf("could not start playwright: %v", err)
	}
	defer pw.Stop()

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(false),
	})
	if err != nil {
		return fmt.Errorf("could not launch browser: %v", err)
	}
	defer browser.Close()

	// Unlike GetRegions, this only ever matches on URLs/hrefs/dropdown
	// values scraped live from the site, never on text content - so it
	// doesn't need (and shouldn't force) English locale. WhoScored's
	// geo-redirect to a localized subdomain is a real, repeating
	// server-side 302 for deep URLs like this one, and blockLocaleRedirect
	// fighting it just causes a redirect loop that eventually collides with
	// the next navigation (net::ERR_ABORTED) - so plain NewContext is used.
	browserCtx, err := browser.NewContext()

	if err != nil {
		return fmt.Errorf("could not create page: %v", err)
	}

	page, err := browserCtx.NewPage()

	if _, err := page.Goto(url, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(30000),
	}); err != nil {
		return fmt.Errorf("could not navigate to %s: %v", url, err)
	}

	// Accept cookies
	if err := page.Locator("button.Button__StyledButton-buoy__sc-a1qza5-0").Click(); err != nil {
		log.Println("Could not click cookie button:", err)
	} else {
		fmt.Println("Accepted cookies")
		fmt.Println("Waiting for page to load...")
	}

	// Close subscription banner (optional element)
	/*if err := page.Locator("button.webpush-swal2-close").Click(); err != nil {
		fmt.Println("Subscription banner not present")
	} else {
		fmt.Println("Closed subscription banner")
	}*/

	fmt.Printf("Opened %s %s page\n", leagueName, season)

	selectLocator := page.Locator("#seasons")
	options := selectLocator.Locator("option")

	count, err := options.Count()
	if err != nil {
		panic(err)
	}

	var season_url string = ""
	for i := 0; i < count; i++ {
		option := options.Nth(i)

		period, _ := option.TextContent()
		period_url, _ := option.GetAttribute("value")

		if period == season {
			season_url = period_url
		}

	}

	if season_url == "" {
		return errors.New("[Error] season " + season + " not present")
	}

	url = "https://www.whoscored.com" + season_url
	fmt.Print("[SEASON URL] " + season_url)
	if _, err := page.Goto(url, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(30000),
	}); err != nil {
		return fmt.Errorf("could not navigate to %s: %v", url, err)
	}

	// Save team names from standings table if not already saved
	teamsNamePath := filepath.Join(dataDir, "whoscored_names.txt")
	if _, err := os.Stat(teamsNamePath); os.IsNotExist(err) {
		result, err := page.EvalOnSelectorAll(
			"table[id^='standings-'] a.team-link",
			"elements => elements.map(e => e.innerText)",
		)
		if err != nil {
			return fmt.Errorf("could not extract team names: %v", err)
		}
		if namesRaw, ok := result.([]interface{}); ok {
			teamNames := make([]string, 0, len(namesRaw))
			for _, n := range namesRaw {
				if s, ok := n.(string); ok {
					teamNames = append(teamNames, s)
				}
			}
			sort.Strings(teamNames)
			if err := os.WriteFile(teamsNamePath, []byte(strings.Join(teamNames, "\n")), 0644); err != nil {
				return err
			}
			fmt.Println("Saved team names")
		}
	}

	// Load names mapping
	/*nameMappingPath := filepath.Join(dataDir, "names_mapping.json")
	if _, err := os.Stat(teamsNamePath); os.IsNotExist(err) {
		fmt.Printf("Missing %s\n", nameMappingPath)
		return fmt.Errorf("names_mapping.json not found at %s", nameMappingPath)
	}
	nameMappingData, err := os.ReadFile(nameMappingPath)
	if err != nil {
		return fmt.Errorf("could not read names_mapping.json: %v", err)
	}
	var namesMap map[string]string
	if err := json.Unmarshal(nameMappingData, &namesMap); err != nil {
		return fmt.Errorf("could not parse names_mapping.json: %v", err)
	}*/

	// Click Fixtures tab (index 1 in sub-navigation), retrying on intercept
	for {
		if err := page.Locator("#sub-navigation li").Nth(1).Click(); err == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	fmt.Println("Opened other navigation window")

	fmt.Printf("\n\n\n")

	games_urls := make([]string, 0)

	prev_page_game_urls := make([]string, 0)

	max_retry := 5
	curr_page_games_urls := make([]string, 0)

	for {

		// data := make(map[string]interface{})

		retry := 0
		for retry = 0; retry < max_retry; retry++ {

			time.Sleep(1 * time.Second)

			links, err := page.Locator(`a[class^="Match-module_statsBtn_"]:not([id^="live"])`).All()

			if err != nil {
				log.Fatal(err)
			}

			curr_page_games_urls = make([]string, 0)
			for _, link := range links {
				href, _ := link.GetAttribute("href")
				curr_page_games_urls = append(curr_page_games_urls, href)
			}

			prev_button := page.Locator("button[id^='dayChangeBtn-prev']")

			// Click the button and wait for the page to update
			if err := prev_button.Click(); err != nil {
				log.Printf("Failed to click previous button: %v", err)
				break
			}

			if listEquality(prev_page_game_urls, curr_page_games_urls) {
				//fmt.Println(prev_page_game_urls, curr_page_games_urls)
				log.Printf("Page did not update after clicking previous button, retrying... (attempt %d/%d)", retry+1, max_retry)
			} else {
				log.Printf("Page updated successfully after clicking previous button.")
				break
			}

		}

		prev_page_game_urls = curr_page_games_urls
		games_urls = append(games_urls, curr_page_games_urls...)

		if retry == max_retry {
			log.Printf("Max retry reached, stopping...")
			break
		}

	}

	// Rather than scraping each match page in-process, create a tracked job
	// per match in Mongo and enqueue its ID on a Redis list so worker
	// processes can pull jobs off the queue, scrape them independently, and
	// update their status as they go.
	redisClient, err := queue.Connect(context.Background())
	if err != nil {
		return fmt.Errorf("could not connect to redis: %v", err)
	}
	defer redisClient.Close()

	if err := queue.EnsureJobsStream(context.Background(), redisClient); err != nil {
		return fmt.Errorf("could not ensure jobs stream: %v", err)
	}

	matchIDRe := regexp.MustCompile(`/matches/(\d+)/`)

	enqueued := 0
	for _, url := range games_urls {
		matchURL := "https://www.whoscored.com" + url

		idMatch := matchIDRe.FindStringSubmatch(url)
		if idMatch == nil {
			log.Printf("could not extract match id from %s, skipping", matchURL)
			continue
		}
		matchID := idMatch[1]

		exists, err := db.MatchExists(context.Background(), mongoClient, matchID)
		if err != nil {
			return fmt.Errorf("could not check existing match %s: %v", matchID, err)
		}
		if exists {
			log.Printf("match %s already scraped, skipping", matchID)
			continue
		}

		job, err := db.CreateJob(context.Background(), mongoClient, matchURL, matchID, leagueName, season)
		if err != nil {
			return fmt.Errorf("could not create job for %s: %v", matchURL, err)
		}

		if err := queue.PushJob(context.Background(), redisClient, job.ID.Hex()); err != nil {
			return fmt.Errorf("could not enqueue job for %s: %v", matchURL, err)
		}
		enqueued++
	}

	fmt.Printf("Enqueued %d match jobs\n", enqueued)

	// saveMapAsJSONFile("data.json", data)

	/*
		// Select the correct season and tournament from their dropdowns
		seasonFormatted := strings.ReplaceAll(season, "-", "/")
		if err := selectOption(page, "//select[contains(@id, 'season')]", seasonFormatted); err != nil {
			return fmt.Errorf("could not select season: %v", err)
		}
		if err := selectOption(page, "//select[contains(@id, 'tournaments')]", displayLeague); err != nil {
			return fmt.Errorf("could not select tournament: %v", err)
		}

		// Paginate through all fixture pages and collect finished games
		teamsGames := map[string][]GameEntry{}
		oldGame := ""
		count := 0

		for {
			count++
			time.Sleep(2 * time.Second)

			allGames, err := page.Locator("div[class^='Match-module_match']").All()
			if err != nil {
				continue
			}

			var filteredGames []playwright.Locator
			for _, game := range allGames {
				href, err := game.Locator("a[class^='Match-module_stat']").GetAttribute("href")
				if err != nil || href == "" {
					continue
				}
				text, err := game.Locator("span[class^='Match-module_']").TextContent()
				if err != nil || text != "FIN" {
					continue
				}
				filteredGames = append(filteredGames, game)
			}

			if len(filteredGames) == 0 {
				if count >= 5 {
					break
				}
				continue
			}

			newGame, _ := filteredGames[len(filteredGames)-1].
				Locator("a[class^='Match-module_stat']").GetAttribute("href")

			if count == 5 {
				break
			}
			if newGame == oldGame {
				continue
			}

			count = 0
			oldGame = newGame

			// Iterate in reverse (most recent first → oldest first after full reversal)
			for i := len(filteredGames) - 1; i >= 0; i-- {
				game := filteredGames[i]
				gameURL, _ := game.Locator("a[class^='Match-module_stat']").GetAttribute("href")

				homeText, _ := game.Locator("div[class^='Match-module_teamName']").Nth(0).TextContent()
				awayText, _ := game.Locator("div[class^='Match-module_teamName']").Nth(1).TextContent()

				home := fixTeamName(homeText)
				away := fixTeamName(awayText)
				/*if mapped, ok := namesMap[home]; ok {
					home = mapped
				}
				if mapped, ok := namesMap[away]; ok {
					away = mapped
				}

				for _, e := range []struct{ ref, opp, venue string }{
					{home, away, "home"},
					{away, home, "away"},
				} {
					teamsGames[e.ref] = append(teamsGames[e.ref], GameEntry{
						Venue: e.venue, Opponent: e.opp, URL: gameURL,
					})
				}
			}

			page.Locator("#dayChangeBtn-prev").Click() //nolint:errcheck
		}

		// Determine which teams to process
		matchInfoDir := filepath.Join(dataDir, "matches")
		createDir(matchInfoDir)

		var teamsToProcess []string
		if filterTeam != nil {
			teamsToProcess = []string{*filterTeam}
		} else {
			for t := range teamsGames {
				teamsToProcess = append(teamsToProcess, t)
			}
		}

		for _, team := range teamsToProcess {
			idx := 1
			fmt.Printf("\n%s\n", team)

			games := make([]GameEntry, len(teamsGames[team]))
			copy(games, teamsGames[team])
			// Reverse to get chronological order (matching Python's [::-1])
			for l, r := 0, len(games)-1; l < r; l, r = l+1, r-1 {
				games[l], games[r] = games[r], games[l]
			}

			for _, entry := range games {
				fmt.Println(entry.URL)

				teamDir := filepath.Join(dataDir, team, "matchlogs")
				createDir(teamDir)
				teamFile := filepath.Join(teamDir, fmt.Sprintf("match_%d.json", idx))

				var matchTeam, matchOpponent string
				if entry.Venue == "home" {
					matchTeam = team
					matchOpponent = entry.Opponent
				} else {
					matchTeam = entry.Opponent
					matchOpponent = team
				}
				matchDict := map[string]interface{}{
					"team":     matchTeam,
					"opponent": matchOpponent,
					"venue":    entry.Venue,
					"index":    idx,
				}

				if checkMatchExistence(teamFile, "whoscored") {
					saveMatchInfoJSONFile(teamFile, "whoscored", matchDict) //nolint:errcheck
					idx++
					continue
				}

				var matchName string
				if entry.Venue == "home" {
					matchName = fmt.Sprintf("%s-%s", team, entry.Opponent)
				} else {
					matchName = fmt.Sprintf("%s-%s", entry.Opponent, team)
				}
				matchDir := filepath.Join(matchInfoDir, matchName)
				createDir(matchDir)

				if _, err := os.Stat(filepath.Join(matchDir, "match_info.json")); err == nil {
					saveMatchInfoJSONFile(teamFile, "whoscored", matchDict) //nolint:errcheck
					idx++
					continue
				}

				// Open a new browser tab for the game URL
				newPage, err := browser.NewPage()
				if err != nil {
					log.Printf("could not create page for %s: %v", entry.URL, err)
					idx++
					continue
				}
				if _, err := newPage.Goto(entry.URL); err != nil {
					log.Printf("could not navigate to %s: %v", entry.URL, err)
					newPage.Close()
					idx++
					continue
				}

				// Wait until the match-centre data is embedded in the page source
				for {
					html, err := newPage.Content()
					if err != nil {
						time.Sleep(1 * time.Second)
						continue
					}
					if strings.Contains(html, "require.config.params['matchheader'] = ") {
						if err := extractMatchPanelInfo(matchDir, html); err != nil {
							log.Printf("could not extract match panel info for %s: %v", matchDir, err)
						}
						break
					}
					time.Sleep(1 * time.Second)
				}

				newPage.Close()
				saveMatchInfoJSONFile(teamFile, "whoscored", matchDict) //nolint:errcheck
				fmt.Printf("Saved data in %s\n", matchDir)
				idx++
			}
		}*/

	return nil
}

// GetMatches scrapes every finished fixture for the given league and season
// (e.g. league "Serie-A", season "2024/2025") and enqueues a job per match.
func GetMatches(league, season string) error {
	return getLeagueMatchCentre("data/", league, season, nil)
}

// GetRegions navigates to whoscored.com and scrapes the current list of
// regions and their tournaments out of the page's embedded "allRegions"
// JS variable.
//
// WhoScored's geo-redirect to a localized subdomain (e.g. it.whoscored.com)
// is inconsistent: it fires as a delayed client-side redirect, and even
// with Locale set and blockLocaleRedirect rewriting it back to www, a
// session cookie set during the brief window before that rewrite kicks in
// can still cause a given attempt to render localized content. So rather
// than chase a single-attempt fix for what's an external, flaky dependency,
// this retries a few times with a fresh context each time.
func GetRegions() ([]Region, error) {
	pw, err := playwright.Run()
	if err != nil {
		return nil, fmt.Errorf("could not start playwright: %v", err)
	}
	defer pw.Stop()

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(false),
	})
	if err != nil {
		return nil, fmt.Errorf("could not launch browser: %v", err)
	}
	defer browser.Close()

	const maxAttempts = 5

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		regions, err := fetchRegionsOnce(browser)
		if err == nil {
			return regions, nil
		}
		lastErr = err
		log.Printf("GetRegions attempt %d/%d failed: %v", attempt, maxAttempts, err)
	}

	return nil, fmt.Errorf("could not scrape regions after %d attempts: %v", maxAttempts, lastErr)
}

// fetchRegionsOnce makes a single attempt at loading whoscored.com in a
// fresh browser context and extracting the "allRegions" JS variable.
func fetchRegionsOnce(browser playwright.Browser) ([]Region, error) {
	url := "https://www.whoscored.com/"

	// Force English so region/tournament names line up with the English
	// names hardcoded in regionsToLeague/leagueToName below - WhoScored
	// otherwise localizes based on the request's Accept-Language/geo-IP.
	context, err := browser.NewContext(playwright.BrowserNewContextOptions{
		Locale: playwright.String("en-US"),
	})
	if err != nil {
		return nil, fmt.Errorf("could not create context: %v", err)
	}
	defer context.Close()

	if err := blockLocaleRedirect(context); err != nil {
		return nil, fmt.Errorf("could not set up locale redirect blocking: %v", err)
	}

	page, err := context.NewPage()
	if err != nil {
		return nil, fmt.Errorf("could not create page: %v", err)
	}

	if _, err := page.Goto(url, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(30000),
	}); err != nil {
		return nil, fmt.Errorf("could not navigate to %s: %v", url, err)
	}

	if err := page.Locator("button.Button__StyledButton-buoy__sc-a1qza5-0").Click(playwright.LocatorClickOptions{
		Timeout: playwright.Float(5000),
	}); err != nil {
		log.Println("Could not click cookie button:", err)
	}

	count, err := page.Locator("script").Count()
	if err != nil {
		return nil, fmt.Errorf("could not count script tags: %v", err)
	}

	for i := 0; i < count; i++ {
		text, err := page.Locator("script").Nth(i).TextContent()
		if err != nil {
			return nil, fmt.Errorf("could not read script tag: %v", err)
		}

		if !strings.Contains(text, "allRegions") {
			continue
		}

		raw := strings.TrimSpace(text)
		raw = strings.TrimPrefix(raw, "var allRegions = ")
		raw = strings.TrimSuffix(raw, ";")

		// Convert single quotes to double quotes
		raw = strings.ReplaceAll(raw, "'", `"`)

		// Quote object keys
		keyRe := regexp.MustCompile(`([a-zA-Z0-9_]+):`)
		raw = keyRe.ReplaceAllString(raw, `"$1":`)

		var regions []Region
		if err := json.Unmarshal([]byte(raw), &regions); err != nil {
			return nil, fmt.Errorf("could not parse allRegions: %v", err)
		}

		// A successful parse doesn't guarantee English content - the page
		// can still render localized (e.g. "Italia" instead of "Italy") even
		// when blockLocaleRedirect kept the request on www.whoscored.com, if
		// a locale cookie got set during the redirect window. Treat that as
		// a failed attempt too, so GetRegions retries it.
		if !hasRegion(regions, "Italy") {
			return nil, fmt.Errorf("got localized region names instead of English")
		}

		return regions, nil
	}

	return nil, fmt.Errorf("could not find allRegions script on page")
}

func hasRegion(regions []Region, name string) bool {
	for _, r := range regions {
		if r.Name == name {
			return true
		}
	}
	return false
}
