package main

import (
	"os"

	"github.com/shahab5191/memshin/internal/db"
)

func buildDSN() string {
	// read from environment variables or .env file
	// first load .env file if exists.
	host := os.Getenv("POSTGRES_HOST")
	port := os.Getenv("POSTGRES_PORT")
	user := os.Getenv("POSTGRES_USER")
	password := os.Getenv("POSTGRES_PASSWORD")
	dbname := os.Getenv("POSTGRES_DB")

	if host == "" || port == "" || user == "" || password == "" || dbname == "" {
		panic("Database connection details are not set in environment variables")
	}

	dsn := "postgres://" + user + ":" + password + "@" + host + ":" + port + "/" + dbname + "?sslmode=disable"
	return dsn
}

func main() {
	dsn := "postgres://username:password@localhost:5432/mydb?sslmode=disable"
	db, err := db.Connect(dsn)
	if err != nil {
		panic(err)
	}
	defer db.Close()
}
