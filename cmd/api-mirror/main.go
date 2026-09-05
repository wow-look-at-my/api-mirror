// Command api-mirror serves a caching mirror of the API XML spec declares.
package main

import (
	"fmt"
	"os"

	"github.com/wow-look-at-my/api-mirror/internal/mirror"
)

func main() {
	if err := mirror.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "api-mirror:", err)
		os.Exit(1)
	}
}
