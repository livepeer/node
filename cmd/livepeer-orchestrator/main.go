package main

import (
	"fmt"
	"os"

	"github.com/livepeer/node/orchestrator"
)

func main() {
	if err := orchestrator.Root(os.Stdout, os.Stderr).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
