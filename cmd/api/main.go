package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"football-scraping/cmd/scrape"
	"football-scraping/internal/db"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
)

func main() {
	r := chi.NewRouter()

	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	ctx := context.Background()
	mongoClient, err := db.Connect(ctx)
	if err != nil {
		logger.Fatal().Err(err).Msg("could not connect to mongo")
	}
	defer mongoClient.Disconnect(ctx) //nolint:errcheck

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))

		logger.Debug().Msg("Request received")
	})

	r.Post("/scrape", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Season string `json:"season"`
			League string `json:"league"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if payload.Season == "" || payload.League == "" {
			http.Error(w, "season and league are required", http.StatusBadRequest)
			return
		}

		if err := scrape.GetMatches(payload.League, payload.Season); err != nil {
			logger.Error().Err(err).Str("league", payload.League).Str("season", payload.Season).Msg("could not scrape matches")
			http.Error(w, "could not scrape matches", http.StatusInternalServerError)
			return
		}

		logger.Debug().Str("league", payload.League).Str("season", payload.Season).Msg("Matches scraped")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
			"status": "ok",
			"league": payload.League,
			"season": payload.Season,
		})
	})

	r.Get("/regions", func(w http.ResponseWriter, r *http.Request) {
		regions, err := scrape.GetRegions()
		if err != nil {
			logger.Error().Err(err).Msg("could not scrape regions")
			http.Error(w, "could not scrape regions", http.StatusInternalServerError)
			return
		}

		if err := db.SaveRegions(r.Context(), mongoClient, regions); err != nil {
			logger.Error().Err(err).Msg("could not save regions")
			http.Error(w, "could not save regions", http.StatusInternalServerError)
			return
		}

		logger.Debug().Int("count", len(regions)).Msg("Regions updated")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(regions) //nolint:errcheck
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	fmt.Printf("Server started on :%s\n", port)
	http.ListenAndServe(":"+port, r)
}
