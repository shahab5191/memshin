package main

import (
	"context"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"

	"github.com/joho/godotenv"
	"github.com/shahab5191/memshin/internal/api"
	"github.com/shahab5191/memshin/internal/db"
	"github.com/shahab5191/memshin/internal/llm"
	"github.com/shahab5191/memshin/internal/memory"
	"github.com/shahab5191/memshin/internal/pipeline"
	"github.com/shahab5191/memshin/internal/repository"
)

// defaultPromotionWorkers is the number of dispatcher goroutines draining the
// promotion channel when PROMOTION_WORKERS is unset. The database claim — not
// this count — guarantees each batch is summarised once, so it is a throughput
// knob, not a correctness one.
const defaultPromotionWorkers = 4

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

	memoryList := make([]pipeline.MemoryLayer, 0)
	memoryStore := repository.NewConversations(pool)
	memoryList = append(memoryList, memory.NewShortTermMemory(memoryStore))

	// Mid-term sits after short-term so it consumes what short-term releases.
	// memoryStore doubles as the promotionStore; the Gemini provider supplies
	// both summarization and embedding.
	vectorStore := repository.NewMidTermMemory(pool)
	memoryList = append(memoryList, memory.NewMidTermMemory(memoryStore, vectorStore, provider, provider))

	engine := pipeline.NewEngine(memoryList, provider)
	log.Println("engine initialized")

	// Promotions are published on a buffered channel; without a running
	// dispatcher the layers fill it and then start dropping claimed events.
	// Run a small pool of them so a slow summarisation for one user does not
	// stall the drain loop for everyone else.
	workers := defaultPromotionWorkers
	if raw := os.Getenv("PROMOTION_WORKERS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			workers = n
		} else {
			log.Printf("invalid PROMOTION_WORKERS %q, using default %d", raw, defaultPromotionWorkers)
		}
	}
	for i := 0; i < workers; i++ {
		go engine.RunPromotions(ctx)
	}

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
