// Command literouter is an OpenAI-compatible local gateway with transparent
// model failover.
package main

import (
	"os"
	_ "time/tzdata"

	"github.com/Schallophy/literouter/internal/app"
)

func main() {
	os.Exit(app.Run())
}
