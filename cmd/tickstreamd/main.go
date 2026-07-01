// Command tickstreamd is the TickStream consolidation daemon.
// M0: prints a version line and exits; feeds/engine arrive in M3.
package main

import "fmt"

const version = "0.1.0-m0"

func main() {
	fmt.Printf("tickstream v%s\n", version)
}
