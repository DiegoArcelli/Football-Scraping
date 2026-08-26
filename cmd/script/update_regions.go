package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	Name string `json:"name"`
	URL  string `json:"url"`
}

type Region struct {
	Name        string       `json:"name"`
	Tournaments []Tournament `json:"tournaments"`
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
func getLeagueMatchCentre(path string) error {

	// Load regions.json (equivalent to Python's open("selenium_test/regions.json"))
	// f, err := os.Open("regions.json")
	// if err != nil {
	// 	return fmt.Errorf("could not open regions.json: %v", err)
	// }
	// defer f.Close()

	// var regions []Region
	// if err := json.NewDecoder(f).Decode(&regions); err != nil {
	// 	return fmt.Errorf("could not decode regions.json: %v", err)
	// }

	// if err != nil {
	// 	return err
	// }
	url := "https://www.whoscored.com/"
	fmt.Println(url)

	dataDir := filepath.Join(path)
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

	context, err := browser.NewContext()

	if err != nil {
		return fmt.Errorf("could not create page: %v", err)
	}

	page, err := context.NewPage()

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

	count, err := page.Locator("script").Count()
	if err != nil {
		panic(err)
	}

	for i := 0; i < count; i++ {
		text, err := page.Locator("script").Nth(i).TextContent()
		if err != nil {
			panic(err)
		}

		if strings.Contains(text, "allRegions") {
			fmt.Println(text)
			fmt.Println(i)
			// Remove JS variable wrapper if present
			var raw string = strings.TrimSpace(text)
			raw = strings.TrimPrefix(raw, "var allRegions = ")
			raw = strings.TrimSuffix(raw, ";")

			// Convert single quotes to double quotes
			raw = strings.ReplaceAll(raw, "'", `"`)

			// Quote object keys
			re := regexp.MustCompile(`([a-zA-Z0-9_]+):`)
			raw = re.ReplaceAllString(raw, `"$1":`)

			var regions []Region

			err := json.Unmarshal([]byte(raw), &regions)
			if err != nil {
				panic(err)
			}

			fmt.Println(regions[0].Name)
			fmt.Println(regions[0].Tournaments[0].Name)

		}
	}

	return nil
}

func main() {
	if err := getLeagueMatchCentre("data/"); err != nil {
		log.Fatalf("Error: %v", err)
	}
}
