// capability-table prints the capability table of this Bifrost build as JSON: for each
// provider type, request type and model family, whether the driver implements the request
// type and which request parameters it ignores (bifrost.CapabilityTable). -commit names
// the commit the table describes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	bifrost "github.com/capsohq/bifrost/core"
)

func main() {
	commit := flag.String("commit", "", "the Bifrost commit this build is (required)")
	flag.Parse()
	if *commit == "" {
		fmt.Fprintln(os.Stderr, "capability-table: -commit is required")
		os.Exit(2)
	}
	entries, err := bifrost.CapabilityTable(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"bifrost_commit": *commit, "entries": entries}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
