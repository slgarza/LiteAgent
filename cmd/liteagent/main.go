package main

import (
	"os"

	"github.com/slgarza/LiteAgent/internal/app"
)

func main() {
	if err := app.Main(os.Args[1:]); err != nil {
		reportError(err)
		os.Exit(1)
	}
}
