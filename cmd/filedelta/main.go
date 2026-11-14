package main

import (
	"fmt"
	"os"

	"filedelta/internal/filedelta"
)

func main() {
	if err := filedelta.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "filedelta:", err)
		os.Exit(1)
	}
}
