package main

import (
	"context"
	"log"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/shahab5191/memshin/internal/api"
	"github.com/shahab5191/memshin/internal/db"
	"github.com/shahab5191/memshin/internal/llm"
	"github.com/shahab5191/memshin/internal/memory"
	"github.com/shahab5191/memshin/internal/pipeline"
	"github.com/shahab5191/memshin/internal/repository"
)

func buildDSN() string {
	host := os.Getenv("POSTGRES_HOST")
	port := os.Getenv("POSTGRES_PORT")
	user := os.Getenv("POSTGRES_USER")
	password := os.Getenv("POSTGRES_PASSWORD")
	dbname := os.Getenv("POSTGRES_DB")
	sslmode := os.Getenv("POSTGRES_SSLMODE")

	if host == "" || port == "" || user == "" || password == "" || dbname == "" || sslmode == "" {
		panic("Database configuration is not set properly in environment variables or .env file")
	}

	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(host, port),
		Path:     dbname,
		RawQuery: "sslmode=" + sslmode,
	}
	return u.String()
}

func main() {
	_ = godotenv.Load()

	ctx := context.Background()

	pool, err := db.Connect(ctx, buildDSN())
	if err != nil {
		panic(err)
	}
	defer pool.Close()

	log.Println("connected to database")

	llmCfg, err := llm.GeminiConfigFromEnv()
	if err != nil {
		panic(err)
	}
	provider, err := llm.NewGemini(ctx, llmCfg)
	if err != nil {
		panic(err)
	}
	log.Println("llm provider initialized:", provider.Name())

	embedCfg, err := llm.EmbedderConfigFromEnv()
	if err != nil {
		panic(err)
	}
	embedder, err := llm.NewGeminiEmbedder(ctx, embedCfg)
	if err != nil {
		panic(err)
	}

	analystCfg, err := llm.AnalystConfigFromEnv()
	if err != nil {
		panic(err)
	}
	analyst, err := llm.NewAnalyst(ctx, analystCfg)
	if err != nil {
		panic(err)
	}
	log.Println("mid-term analyst initialized:", analyst.Name())

	memoryStore := repository.NewConversations(pool)
	factStore := repository.NewFacts(pool)
	focusStore := repository.NewFocus(pool)

	// Order matters here, unlike everywhere else in the pipeline: focus writes
	// ChatContext.Focus and mid-term hands it to the decomposer to resolve
	// references before probing, so focus has to run first. Rendering order into
	// the prompt is independent of this and comes from each block's Priority.
	memoryList := []pipeline.MemoryLayer{
		memory.NewFocusMemory(focusStore, analyst),
		memory.NewShortTermMemory(memoryStore),
		memory.NewMidTermMemory(memoryStore, factStore, analyst, embedder),
	}
	engine := pipeline.NewEngine(memoryList, provider)
	log.Println("engine initialized")

	// Promotions are published on a buffered channel; without a running
	// dispatcher the layers fill it and then start dropping claimed events.
	go engine.RunPromotions(ctx)

	// Nothing a request does can promote a conversation that simply stopped, or
	// recover a release whose ingest died. Both leave the short-term window
	// growing without bound, so they need a clock rather than a turn.
	go pipeline.NewSweeper(memoryStore, engine.Publisher(), pipeline.SweeperConfig{
		Interval:    time.Minute,
		IdleGap:     memory.SessionIdleGap,
		Lease:       5 * time.Minute,
		SourceLayer: memory.ShortTermMemoryTag,
		TargetLayer: memory.MidTermMemoryName,
	}).Run(ctx)

	cfg := api.GetConfigFromEnv()

	server := api.NewServer(pool, engine, cfg)
	log.Println("server initialized on", cfg.Addr)

	log.Println("starting server...")
	// ListenAndServe only ever returns an error here, so it has to be checked:
	// otherwise a failed bind is indistinguishable from a clean exit.
	if err := server.HTTPServer().ListenAndServe(); err != nil {
		log.Fatalf("http server: %v", err)
	}
}
