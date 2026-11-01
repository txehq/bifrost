// Command warpwireseed writes and deletes log rows through the log store itself,
// for tests/e2e/api/runners/run-warp-wire.mjs. Going through the store keeps one
// row definition good for SQLite, Postgres and ClickHouse, which a SQL statement
// per backend would not, and lets the rows share a timestamp to the millisecond,
// which traffic through a gateway cannot.
//
//	warpwireseed -config logs_store.json -insert rows.json
//	warpwireseed -config logs_store.json -delete <id>
//
// -config is the gateway's logs_store object. rows.json is an array of log rows
// in the log store's JSON shape, plus "tokens".
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/logstore"
)

// row carries the token columns, which logstore.Log leaves out of its JSON.
type row struct {
	logstore.Log
	Tokens int `json:"tokens"`
}

func main() {
	configPath := flag.String("config", "", "the gateway's logs_store object, as a JSON file")
	insertPath := flag.String("insert", "", "a JSON array of log rows to write")
	deleteID := flag.String("delete", "", "the id of a log row to delete")
	flag.Parse()
	if *configPath == "" || (*insertPath == "") == (*deleteID == "") {
		log.Fatal("usage: warpwireseed -config <file> (-insert <file> | -delete <id>)")
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	var config logstore.Config
	if err := json.Unmarshal(raw, &config); err != nil {
		log.Fatalf("parse %s: %v", *configPath, err)
	}
	ctx := context.Background()
	store, err := logstore.NewLogStore(ctx, &config, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close(ctx)

	if *deleteID != "" {
		if err := store.DeleteLog(ctx, *deleteID); err != nil {
			log.Fatal(err)
		}
		return
	}
	raw, err = os.ReadFile(*insertPath)
	if err != nil {
		log.Fatal(err)
	}
	var rows []row
	if err := json.Unmarshal(raw, &rows); err != nil {
		log.Fatalf("parse %s: %v", *insertPath, err)
	}
	entries := make([]*logstore.Log, 0, len(rows))
	for index := range rows {
		entry := &rows[index].Log
		entry.PromptTokens, entry.TotalTokens = rows[index].Tokens, rows[index].Tokens
		entries = append(entries, entry)
	}
	if err := store.BatchCreateIfNotExists(ctx, entries); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %d rows\n", len(entries))
}
