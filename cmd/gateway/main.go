// Command gateway is the entry point for the S3-compatible ZK Gateway
// that runs on the Linode data plane. See docs/PROPOSAL.md §3.1.
//
// Phase 1 is a scaffold: the binary parses a config file, mounts the
// s3compat handler stubs, and serves 501 responses. Real request
// handling lands in Phase 2.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat"
	"github.com/kennguy3n/zk-object-fabric/internal/config"
)

func main() {
	cfgPath := flag.String("config", "", "path to JSON config file (optional)")
	flag.Parse()

	cfg := config.Default()
	if *cfgPath != "" {
		loaded, err := config.Load(*cfgPath)
		if err != nil {
			log.Fatalf("gateway: load config: %v", err)
		}
		cfg = loaded
	}

	mux := http.NewServeMux()
	s3compat.New().Register(mux)

	srv := &http.Server{
		Addr:         cfg.Gateway.ListenAddr,
		Handler:      mux,
		ReadTimeout:  cfg.Gateway.ReadTimeout.ToDuration(),
		WriteTimeout: cfg.Gateway.WriteTimeout.ToDuration(),
	}

	log.Printf("gateway: listening on %s (env=%s)", cfg.Gateway.ListenAddr, cfg.Env)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("gateway: listen: %v", err)
	}
}
