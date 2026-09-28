package main

import (
	"fmt"
	"os"

	"github.com/livepeer/node/internal/signer"
)

func main() {
	if err := signer.Root(os.Stdout, os.Stderr).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
