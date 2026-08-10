package api

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultAddr           = ":8080"
	defaultRequestTimeout = 90 * time.Second
)

type chatEngine interface {
	Process(ctx context.Context, userID, prompt, sysMsg string) (string, error)
}

type Config struct {
	Addr                 string
	DefaultSystemMessage string
	RequestTimeout       time.Duration
}

func GetConfigFromEnv() Config {
	cfg := &Config{}
	addr := os.Getenv("API_ADDR")
	if addr == "" {
		log.Println("API_ADDR not set, using default:", defaultAddr)
		addr = defaultAddr
	}
	cfg.Addr = addr

	sysMsg := os.Getenv("API_DEFAULT_SYSTEM_MESSAGE")
	if sysMsg == "" {
		log.Println("API_DEFAULT_SYSTEM_MESSAGE not set, using default message.")
		sysMsg = "You are a helpful assistant."
	}
	cfg.DefaultSystemMessage = sysMsg

	timeoutStr := os.Getenv("API_REQUEST_TIMEOUT")
	timeout := defaultRequestTimeout
	if timeoutStr != "" {
		if d, err := time.ParseDuration(timeoutStr); err == nil {
			timeout = d
		} else {
			log.Printf("Invalid API_REQUEST_TIMEOUT value: %v", err)
		}
	}
	cfg.RequestTimeout = timeout

	return *cfg
}

type Server struct {
	db     *pgxpool.Pool
	engine chatEngine
	cfg    Config
}

func NewServer(db *pgxpool.Pool, engine chatEngine, cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = defaultAddr
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	return &Server{
		db:     db,
		engine: engine,
		cfg:    cfg,
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat", s.handleChat())
	mux.HandleFunc("GET /healthz", s.handleHealth())

	return logRequests(mux)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info(
			"request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start),
		)
	})
}
