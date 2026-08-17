package main

import (
	"fmt"
	"os"
	"pictl/pinctl"
)

// Initialize HTTP API server and goroutines
func main() {
	err := pinctl.Initialize()
	if err != nil {
		fmt.Printf("pin initialization error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Pins initialized")

	err = pinctl.CloseAll()
	if err != nil {
		fmt.Printf("pin close error: %v\n", err)
	}
}
