package cli

import (
	"log"

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
func GetClient(cfg *config.Config) (pb.SchedulerServiceClient, *grpc.ClientConn) {
	addr := cfg.GRPCAddr()

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
