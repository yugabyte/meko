// Command mccp is a minimal Meko Cloud Control Plane: it serves the OpenAPI
// subset (design/mccp-openapi.yaml) and hosts the console. State persists to
// Postgres when MCCP_DATABASE_URL is set, otherwise to an in-memory store.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"meko/mccp/internal/server"
	"meko/mccp/internal/store"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dsn := flag.String("db", os.Getenv("MCCP_DATABASE_URL"), "Postgres DSN; empty = in-memory store")
	flag.Parse()

	var st store.Store
	if *dsn != "" {
		pg, err := store.NewPostgres(context.Background(), *dsn)
		if err != nil {
			log.Fatalf("postgres: %v", err)
		}
		st = pg
		log.Printf("store: postgres")
	} else {
		st = store.NewMemory()
		log.Printf("store: in-memory (set -db / MCCP_DATABASE_URL for durable state)")
	}
	defer st.Close()

	// Pre-register the demo agent's environment with a known enrollment token so the
	// agent can complete the real token handshake (emulates an admin having registered it).
	// Re-arm it on a ticker (well within the token TTL) so the demo agent can always
	// (re-)enroll — real environments use per-registration tokens, not this seed path.
	if name, tok := os.Getenv("MCCP_SEED_ENV_NAME"), os.Getenv("MCCP_SEED_ENV_TOKEN"); name != "" && tok != "" {
		id := store.Slug(name)
		arm := func() {
			if err := st.SeedEnrollment(id, "cloud", tok); err != nil {
				log.Printf("seed enrollment: %v", err)
			}
		}
		arm()
		log.Printf("seeded enrollment for env %q (re-armed every 10m)", id)
		go func() {
			t := time.NewTicker(10 * time.Minute)
			defer t.Stop()
			for range t.C {
				arm()
			}
		}()
	}

	log.Printf("MCCP listening on %s (console at /, api under /api/v1)", *addr)
	if err := http.ListenAndServe(*addr, server.Handler(st)); err != nil {
		log.Fatal(err)
	}
}
