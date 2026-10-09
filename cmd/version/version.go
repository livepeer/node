// Package version holds metadata shared by command binaries.
// Keep it small!
package version

var Version = "dev"
var Commit = "unknown"

func String() string { return "livepeer " + Version + " (" + Commit + ")" }
