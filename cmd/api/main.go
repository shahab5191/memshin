package main

import (
	"context"
	"log"
	"net"
	"net/url"
	"os"

	"github.com/joho/godotenv"
	"github.com/shahab5191/memshin/internal/db"
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
}
