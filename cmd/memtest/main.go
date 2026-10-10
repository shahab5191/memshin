// Command memtest drives the memory pipeline against an ephemeral test
// database using scripted, hand-written conversations. It exists so the focus
// and mid-term prompts can be iterated on without talking to the API by hand.
//
// Subcommands:
//
//	run                          create a test DB, migrate, run all scenarios
//	list                         list existing test databases
//	inspect -db NAME [-user U]   dump focus + mid-term output for a run
//	drop -db NAME                drop one test database
//	drop-all                     drop every test database
//
// Example:
//
//	go run ./cmd/memtest run -verbose
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/shahab5191/memshin/internal/app"
	"github.com/shahab5191/memshin/internal/llm"
	"github.com/shahab5191/memshin/internal/memory"
	"github.com/shahab5191/memshin/internal/promotion"
	"github.com/shahab5191/memshin/internal/testdb"
)

func main() {
	_ = godotenv.Load()
	ctx := context.Background()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "run":
		run(ctx, os.Args[2:])
	case "list":
		list(ctx, os.Args[2:])
	case "inspect":
		inspect(ctx, os.Args[2:])
	case "drop":
		drop(ctx, os.Args[2:])
	case "drop-all":
		dropAll(ctx, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  memtest run [-turns N] [-scenario S] [-verbose]
  memtest list
  memtest inspect -db NAME [-user U]
  memtest drop -db NAME
  memtest drop-all
`)
}

// run creates a fresh test database, migrates it, and orchestrates every
// scenario synchronously turn by turn.
func run(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var turns int
	var scenario string
	var verbose bool
	fs.IntVar(&turns, "turns", 0, "max turns per scenario (0 = run the whole script)")
	fs.StringVar(&scenario, "scenario", "", "filter scenarios by user id or title (substring)")
	fs.BoolVar(&verbose, "verbose", false, "print focus subject and new facts as each turn is processed")
	_ = fs.Parse(args)

	provider, err := llm.NewGemini(ctx, mustGeminiConfig())
	if err != nil {
		log.Fatalf("llm provider: %v", err)
	}

	name := testdb.Name()
	pool, err := testdb.Create(ctx, name)
	if err != nil {
		log.Fatalf("create test db: %v", err)
	}
	defer pool.Close()

	log.Printf("created test database %s", name)

	mem := app.NewMemoryPipeline(pool, provider)

	selected := selectScenarios(scenario, turns)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	for _, sc := range selected {
		if err := testdb.RecordScenario(ctx, pool, name, sc.UserID, sc.Title, len(sc.Turns)); err != nil {
			log.Fatalf("record scenario: %v", err)
		}
		wg.Add(1)
		go func(sc Scenario) {
			defer wg.Done()
			if err := orchestrate(ctx, pool, mem, sc, verbose); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", sc.UserID, err))
				mu.Unlock()
			}
		}(sc)
	}
	wg.Wait()

	if len(errs) > 0 {
		for _, e := range errs {
			log.Printf("scenario failed: %v", e)
		}
		log.Fatalf("%d scenario(s) failed", len(errs))
	}

	log.Printf("done — inspect with:  go run ./cmd/memtest inspect -db %s", name)
	printSummary(ctx, pool, selected)
}

// orchestrate drives one scenario synchronously: append the turn, extract
// focus against the current window, then release and summarise any mid-term
// batch that became available. This mirrors the async pipeline's production
// order without the worker pool, so state is complete after every turn.
func orchestrate(ctx context.Context, pool *pgxpool.Pool, mem *app.MemoryPipeline, sc Scenario, verbose bool) error {
	for i, t := range sc.Turns {
		if err := mem.Conversations.AppendTurn(ctx, sc.UserID, t.Prompt, t.Response); err != nil {
			return fmt.Errorf("append turn %d: %w", i+1, err)
		}

		focusEv := promotion.Event{
			UserID:      sc.UserID,
			SourceLayer: memory.FocusMemoryName,
			TargetLayer: memory.FocusMemoryName,
		}
		if err := mem.Focus.HandlePromotion(ctx, focusEv, nil); err != nil {
			return fmt.Errorf("focus turn %d: %w", i+1, err)
		}

		released, err := mem.Conversations.ReleaseBatch(
			ctx, sc.UserID, memory.MidTermMemoryName, memory.RecentMessageFloor, memory.PromotionBatchSize)
		if err != nil {
			return fmt.Errorf("release turn %d: %w", i+1, err)
		}
		if released > 0 {
			midEv := promotion.Event{
				UserID:      sc.UserID,
				SourceLayer: memory.ShortTermMemoryName,
				TargetLayer: memory.MidTermMemoryName,
			}
			if err := mem.MidTerm.HandlePromotion(ctx, midEv, nil); err != nil {
				return fmt.Errorf("mid-term turn %d: %w", i+1, err)
			}
		}

		if verbose {
			subject, err := latestFocus(ctx, pool, sc.UserID)
			if err != nil {
				subject = "<error>"
			}
			if released > 0 {
				fmt.Printf("[%s] turn %3d  focus=%q  (released %d messages)\n", sc.UserID, i+1, subject, released)
			} else {
				fmt.Printf("[%s] turn %3d  focus=%q\n", sc.UserID, i+1, subject)
			}
		}
	}
	return nil
}

// latestFocus returns the current focus subject for a user (empty when none).
func latestFocus(ctx context.Context, pool *pgxpool.Pool, userID string) (string, error) {
	var subject string
	err := pool.QueryRow(ctx,
		`SELECT subject FROM focus WHERE user_id = $1 ORDER BY last_turn_seq DESC, id DESC LIMIT 1`,
		userID).Scan(&subject)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return subject, nil
}

// selectScenarios filters the built-in scenarios by user id/title substring and
// truncates each to the requested turn count (0 keeps the whole script).
func selectScenarios(filter string, turns int) []Scenario {
	all := scenarios()
	if filter == "" && turns <= 0 {
		return all
	}

	var out []Scenario
	for _, sc := range all {
		if filter != "" {
			f := strings.ToLower(filter)
			if !strings.Contains(strings.ToLower(sc.UserID), f) && !strings.Contains(strings.ToLower(sc.Title), f) {
				continue
			}
		}
		if turns > 0 && len(sc.Turns) > turns {
			sc.Turns = sc.Turns[:turns]
		}
		out = append(out, sc)
	}
	return out
}

func mustGeminiConfig() llm.GeminiConfig {
	cfg, err := llm.GeminiConfigFromEnv()
	if err != nil {
		log.Fatalf("gemini config: %v", err)
	}
	return cfg
}

func list(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	_ = fs.Parse(args)

	names, err := testdb.List(ctx)
	if err != nil {
		log.Fatalf("list: %v", err)
	}
	if len(names) == 0 {
		fmt.Println("no test databases")
		return
	}
	for _, n := range names {
		fmt.Println(n)
	}
}

func inspect(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	dbName := fs.String("db", "", "test database name (required)")
	userID := fs.String("user", "", "user id to inspect (default: all scenarios)")
	_ = fs.Parse(args)

	if *dbName == "" {
		log.Fatal("-db is required")
	}

	pool, err := pgxpool.New(ctx, testdb.DSNFor(*dbName))
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	meta, err := testdb.ReadMeta(ctx, pool)
	if err != nil {
		log.Fatalf("read meta: %v", err)
	}

	fmt.Printf("Database: %s\n", *dbName)
	for _, m := range meta {
		if *userID != "" && m.UserID != *userID {
			continue
		}
		printUser(ctx, pool, m)
	}
}

func printUser(ctx context.Context, pool *pgxpool.Pool, m testdb.Meta) {
	fmt.Printf("\n=== %s  (%s, %d turns) ===\n", m.UserID, m.Title, m.Turns)
	printFocus(ctx, pool, m.UserID)
	printFacts(ctx, pool, m.UserID)
}

func printFocus(ctx context.Context, pool *pgxpool.Pool, userID string) {
	rows, err := pool.Query(ctx,
		`SELECT last_turn_seq, subject FROM focus WHERE user_id = $1 ORDER BY last_turn_seq ASC, id ASC`, userID)
	if err != nil {
		fmt.Printf("  focus: %v\n", err)
		return
	}
	defer rows.Close()

	fmt.Println("Focus history:")
	n := 0
	for rows.Next() {
		var seq int64
		var subject string
		if err := rows.Scan(&seq, &subject); err != nil {
			fmt.Printf("  focus: %v\n", err)
			return
		}
		fmt.Printf("  [seq %3d] %s\n", seq, subject)
		n++
	}
	if rows.Err() != nil {
		fmt.Printf("  focus: %v\n", rows.Err())
		return
	}
	if n == 0 {
		fmt.Println("  (none)")
	}
}

func printFacts(ctx context.Context, pool *pgxpool.Pool, userID string) {
	rows, err := pool.Query(ctx, `
SELECT publish_version, fact_index, summary, start_seq, end_seq
FROM mid_term_memory
WHERE user_id = $1
ORDER BY publish_version ASC, fact_index ASC`, userID)
	if err != nil {
		fmt.Printf("  mid-term: %v\n", err)
		return
	}
	defer rows.Close()

	fmt.Println("Mid-term facts:")
	n := 0
	for rows.Next() {
		var version, idx int32
		var summary string
		var start, end int64
		if err := rows.Scan(&version, &idx, &summary, &start, &end); err != nil {
			fmt.Printf("  mid-term: %v\n", err)
			return
		}
		fmt.Printf("  v%-3d f%d [%d-%d] %s\n", version, idx, start, end, summary)
		n++
	}
	if rows.Err() != nil {
		fmt.Printf("  mid-term: %v\n", rows.Err())
		return
	}
	if n == 0 {
		fmt.Println("  (none)")
	}
}

func printSummary(ctx context.Context, pool *pgxpool.Pool, selected []Scenario) {
	for _, sc := range selected {
		printUser(ctx, pool, testdb.Meta{UserID: sc.UserID, Title: sc.Title, Turns: len(sc.Turns)})
	}
}

func drop(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("drop", flag.ExitOnError)
	dbName := fs.String("db", "", "test database name (required)")
	_ = fs.Parse(args)

	if *dbName == "" {
		log.Fatal("-db is required")
	}
	if err := testdb.Drop(ctx, *dbName); err != nil {
		log.Fatalf("drop: %v", err)
	}
	fmt.Printf("dropped %s\n", *dbName)
}

func dropAll(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("drop-all", flag.ExitOnError)
	_ = fs.Parse(args)

	names, err := testdb.List(ctx)
	if err != nil {
		log.Fatalf("list: %v", err)
	}
	for _, n := range names {
		if err := testdb.Drop(ctx, n); err != nil {
			log.Printf("drop %s: %v", n, err)
			continue
		}
		fmt.Printf("dropped %s\n", n)
	}
}
