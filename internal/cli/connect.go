package cli

import (
	"log"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// GetClient creates a gRPC client connected to the master from config.
func GetClient(cfg *config.Config) (pb.SchedulerServiceClient, *grpc.ClientConn) {
	addr := cfg.GRPCAddr()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to connect to master at %s: %v", addr, err)
	}
	return pb.NewSchedulerServiceClient(conn), conn
}
