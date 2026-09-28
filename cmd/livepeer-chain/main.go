package main

import (
	"fmt"
	"os"

	"github.com/livepeer/node/chain"
)

func main() {
	if err := chain.Root(os.Stdout, os.Stderr).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
