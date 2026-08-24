package main

import (
	"os"

	"github.com/garysng/wizard/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
