// scaffold generates a new Kanz service skeleton on the EVT-16a layout (DEVX-01b).
//
//	go run ./tools/scaffold/cmd/scaffold -name my-service
//
// Writes services/my-service/{cmd,internal,README.md}. Refuses to overwrite.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/kanz-eng/kanz/tools/scaffold"
)

func main() {
	name := flag.String("name", "", "service name (lowercase, dashes; e.g. risk-engine)")
	root := flag.String("root", ".", "repo root the services/ tree lives under")
	flag.Parse()

	if *name == "" {
		fmt.Fprintln(os.Stderr, "scaffold: -name is required")
		flag.Usage()
		os.Exit(2)
	}

	created, err := scaffold.Generate(*root, *name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("scaffolded service %q:\n", *name)
	for _, p := range created {
		fmt.Printf("  %s\n", p)
	}
	fmt.Printf("\nnext: go build ./services/%s/... && go run ./services/%s/cmd/%s\n", *name, *name, *name)
}
