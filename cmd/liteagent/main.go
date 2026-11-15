package main

import (
	"log"
	"os"

	"github.com/slgarza/LiteAgent/internal/app"
)

func main() {
	if err := app.Main(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
