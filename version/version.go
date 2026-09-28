package version

import "fmt"

// These values are set once for the complete release bundle.
var (
	Name   = "dev"
	Commit = "unknown"
)

func String() string { return fmt.Sprintf("livepeer %s (%s)", Name, Commit) }
