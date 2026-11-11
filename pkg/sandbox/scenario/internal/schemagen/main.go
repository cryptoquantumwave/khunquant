// Command schemagen writes the JSON Schema of a scenario file (scenario.SchemaJSON).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
)

func main() {
	out := flag.String("o", "", "file to write")
	flag.Parse()
	b, err := scenario.SchemaJSON()
	if err == nil && *out == "" {
		err = fmt.Errorf("-o is required")
	}
	if err == nil {
		err = os.WriteFile(*out, b, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
}
