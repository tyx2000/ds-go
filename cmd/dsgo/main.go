package main

import (
	"dsgo/internal/cli"
	"os"
)

func main() { os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
