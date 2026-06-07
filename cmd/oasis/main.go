package main

import (
	"fmt"
	"os"

	"github.com/oasis/oasis/cmd/oasis/commands"
)

func main() {
	if err := commands.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
