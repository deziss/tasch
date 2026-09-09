package cli

import (
	"context"
	"fmt"
	"log"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/auth"
	"github.com/deziss/tasch/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// GetClient creates a gRPC client connected to the master from config.
//
// Transport credentials follow cfg.TLS, matching how the worker dials. The CLI used to hardcode
// insecure credentials, so enabling TLS on the master left every CLI command unable to connect.
//
// With several masters configured, this finds the one currently accepting writes: only the
// leader does, and which master that is changes on failover.
func GetClient(cfg *config.Config) (pb.SchedulerServiceClient, *grpc.ClientConn) {
	addrs := cfg.GRPCAddrs()
	if len(addrs) > 1 {
		return leaderClient(cfg, addrs)
	}
	addr := addrs[0]

	transport := insecure.NewCredentials()
	if cfg.TLS.Enabled {
		if cfg.TLS.CAFile == "" {
			log.Fatalf("TLS is enabled but tls.ca_file is not set; the CLI needs the CA certificate to verify the master")
		}
		creds, err := credentials.NewClientTLSFromFile(cfg.TLS.CAFile, "")
		if err != nil {
			log.Fatalf("Failed to load TLS CA %s: %v", cfg.TLS.CAFile, err)
		}
		transport = creds
	}

	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(transport)}
	if cfg.ClientToken != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(auth.TokenCredentials{
			Token: cfg.ClientToken,
			// Tasch supports plaintext clusters, so the token may travel without TLS. That is
			// the operator's choice; gRPC would otherwise refuse to send it at all.
			Insecure: !cfg.TLS.Enabled,
		}))
	}

	conn, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		log.Fatalf("Failed to connect to master at %s: %v", addr, err)
	}
	return pb.NewSchedulerServiceClient(conn), conn
}

// leaderClient returns a connection to whichever master is currently the leader.
//
// Every master answers reads, but only the leader accepts writes, so a client that simply picked
// the first reachable address would see its submissions rejected after any failover. Each master
// is asked in turn until one identifies itself as the leader.
func leaderClient(cfg *config.Config, addrs []string) (pb.SchedulerServiceClient, *grpc.ClientConn) {
	var lastErr error

	for attempt := 0; attempt < 2; attempt++ {
		for _, addr := range addrs {
			conn, err := dial(cfg, addr)
			if err != nil {
				lastErr = err
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			resp, err := pb.NewSchedulerServiceClient(conn).ClusterStatus(ctx, &pb.ClusterStatusRequest{})
			cancel()
			if err == nil && resp.IsLeader {
				return pb.NewSchedulerServiceClient(conn), conn
			}
			lastErr = err
			_ = conn.Close()
		}
		// An election in progress leaves every master a follower for a moment.
		if attempt == 0 {
			time.Sleep(time.Second)
		}
	}

	log.Fatalf("No master is currently accepting writes (tried %v): %v", addrs, lastErr)
	return nil, nil
}

// dial opens a connection with the configured credentials.
func dial(cfg *config.Config, addr string) (*grpc.ClientConn, error) {
	transport := insecure.NewCredentials()
	if cfg.TLS.Enabled {
		if cfg.TLS.CAFile == "" {
			return nil, fmt.Errorf("TLS is enabled but tls.ca_file is not set")
		}
		creds, err := credentials.NewClientTLSFromFile(cfg.TLS.CAFile, "")
		if err != nil {
			return nil, fmt.Errorf("load TLS CA %s: %w", cfg.TLS.CAFile, err)
		}
		transport = creds
	}

	opts := []grpc.DialOption{grpc.WithTransportCredentials(transport)}
	if cfg.ClientToken != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(auth.TokenCredentials{
			Token:    cfg.ClientToken,
			Insecure: !cfg.TLS.Enabled,
		}))
	}
	return grpc.NewClient(addr, opts...)
}
