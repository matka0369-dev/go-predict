package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
	// Embeds the IANA tz database in the binary. Round generation resolves
	// each game's zone with time.LoadLocation, which otherwise reads the
	// host's zoneinfo — absent from scratch/distroless images, where every
	// non-UTC game would then be skipped.
	_ "time/tzdata"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/joho/godotenv"

	appauth "predictsim/prediction-service/internal/auth"
	appcors "predictsim/prediction-service/internal/cors"
	appdb "predictsim/prediction-service/internal/db"
	"predictsim/prediction-service/internal/game"
	"predictsim/prediction-service/internal/handlers"
)

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	// Best-effort: a real deployment injects env vars directly and has no
	// .env file, so a missing file here is not an error worth failing on —
	// same convention as core-service's "dotenv/config" import.
	_ = godotenv.Load()

	ctx := context.Background()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	pool, err := appdb.NewPool(ctx, dbURL)
	if err != nil {
		log.Fatalf("unable to connect to database: %v", err)
	}
	defer pool.Close()

	// Keeps Round rows generated ahead of time for every ACTIVE game,
	// skipping weeklyOffDays/GameHoliday — see internal/game/scheduler.go.
	// Runs for the lifetime of the process; cancelled on shutdown via ctx.
	schedulerCtx, stopScheduler := context.WithCancel(ctx)
	defer stopScheduler()
	interval := time.Duration(envInt("ROUND_GENERATOR_INTERVAL_SECONDS", 300)) * time.Second
	daysAhead := envInt("ROUND_GENERATOR_DAYS_AHEAD", 3)
	go game.RunScheduler(schedulerCtx, pool, interval, daysAhead)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(10 * time.Second))
	r.Use(appcors.Middleware)

	r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		if err := pool.Ping(req.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "db_unreachable"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Unauthenticated — this is the one route that has to be, since it's
	// what a session comes from.
	r.Post("/auth/login", appauth.Login(pool))

	r.Group(func(r chi.Router) {
		r.Use(appauth.Middleware(pool))
		r.Get("/games/active", handlers.GetActiveGames(pool))
		r.Get("/games/{gameId}/history", handlers.GetGameHistory(pool))
		r.Post("/predictions", handlers.PostPrediction(pool))
		r.Get("/predictions/me", handlers.GetMyPredictions(pool))

		r.Post("/auth/logout", appauth.Logout(pool))
		r.Get("/auth/me", appauth.Me(pool))
		r.Get("/auth/sessions", appauth.ListSessions(pool))
		r.Delete("/auth/sessions/{id}", appauth.RevokeSession(pool))
		r.Delete("/auth/sessions", appauth.RevokeOtherSessions(pool))

		r.Get("/users/{id}", handlers.GetUser(pool))

		r.Get("/rates/meta", handlers.GetRateMeta(pool))
		r.Get("/rates/me", handlers.GetMyRates(pool))

		r.Get("/ledger", handlers.GetLedger(pool))

		r.Post("/token-requests", handlers.CreateTokenRequest(pool))
		r.Get("/token-requests/me", handlers.MyTokenRequests(pool))
		r.Post("/token-requests/{id}/cancel", handlers.CancelTokenRequest(pool))
		r.Get("/token-requests/{id}/image", handlers.GetTokenRequestImage(pool))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("prediction-service listening on :%s", port)
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatal(err)
	}
}
