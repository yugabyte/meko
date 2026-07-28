// Command dpmcp is the per-datapack MCP server: a real MCP-protocol endpoint
// (JSON-RPC over HTTP) backed by the datapack's database. One instance runs per
// datapack, provisioned by meko-agent. Falls back to an in-memory store when no
// DP_DB_URL is set (dev/tests).
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"

	"meko/ycp/internal/mcp"
)

func main() {
	addr := flag.String("addr", ":3000", "listen address")
	dsn := flag.String("db", os.Getenv("DP_DB_URL"), "datapack Postgres DSN; empty = in-memory")
	flag.Parse()

	var store mcp.Store
	if *dsn != "" {
		s, err := mcp.NewPgStore(context.Background(), *dsn)
		if err != nil {
			log.Fatalf("datapack db: %v", err)
		}
		store = s
		log.Printf("dpmcp: postgres-backed")
	} else {
		store = mcp.NewMemStore()
		log.Printf("dpmcp: in-memory")
	}
	defer store.Close()

	log.Printf("dpmcp listening on %s (POST /mcp, GET /healthz)", *addr)
	if err := http.ListenAndServe(*addr, mcp.Handler(store)); err != nil {
		log.Fatal(err)
	}
}
