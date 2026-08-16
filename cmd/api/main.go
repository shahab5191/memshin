package main

import (
	"context"
	"log"
	"net"
	"net/url"
	"os"

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

	// Order matters for layers that read pipeline state another layer writes:
	// focus memory, once it exists, has to sit ahead of mid-term, which hands
	// ChatContext.Focus to the decomposer to resolve references before probing.
	// Rendering order into the prompt is independent of this and comes from
	// each block's Priority.
	memoryList := []pipeline.MemoryLayer{
		memory.NewShortTermMemory(memoryStore),
		memory.NewMidTermMemory(memoryStore, factStore, analyst, embedder),
	}
	engine := pipeline.NewEngine(memoryList, provider)
	log.Println("engine initialized")

	// Promotions are published on a buffered channel; without a running
	// dispatcher the layers fill it and then start dropping claimed events.
	go engine.RunPromotions(ctx)

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
