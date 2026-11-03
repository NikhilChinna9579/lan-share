// Command lanshare is the Windows side of LAN Share.
//
// For now it only prints its version; the spike adds pairing and file transfer.
package main

import (
	"fmt"

	"github.com/NikhilChinna9579/lan-share/windows/internal/protocol"
)

// version is overwritten at build time: -ldflags "-X main.version=..."
var version = "dev"

func main() {
	fmt.Printf("LAN Share for Windows %s (protocol v%d)\n", version, protocol.Version)
}
