// Command gpu-bouncer arbitrates one GPU between local AI services that do not
// cooperate with each other.
package main

import (
	"os"

	"github.com/hyprtuna/gpu-bouncer/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], cli.Env{Stdout: os.Stdout, Stderr: os.Stderr}))
}
