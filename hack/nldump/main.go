// Command nldump prints the host's netlink View and host fields as JSON.
// Run it on a node to capture a driver fixture:
//
//	GOOS=linux go build -o nldump ./hack/nldump && scp nldump node:/tmp/ && ssh node sudo /tmp/nldump > internal/cni/testdata/<cni>-healthy.json
package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/Perserverance-syn/Cluster-recorder/internal/host"
	"github.com/Perserverance-syn/Cluster-recorder/internal/netlink"
)

func main() {
	v, err := netlink.Host{}.View(context.Background())
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	root := "/"
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"view": v, "host": host.Fields("/proc", root)})
}
