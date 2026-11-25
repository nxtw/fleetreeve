package main

import (
	"os"

	"github.com/nxtw/fleetreeve/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
