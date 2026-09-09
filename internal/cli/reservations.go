package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/config"
	"github.com/spf13/cobra"
)

// ReserveCmd manages node reservations.
//
// The point of this over `tasch nodes cordon` is the window. A cordon has to be applied by hand
// at the right moment and lifted by hand afterwards; a reservation is stated once, up front,
// and the scheduler does the draining — it stops placing jobs that would still be running when
// the window opens, so the node is empty when the work starts and nothing had to be killed.
func ReserveCmd(cfgLoader func() *config.Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reserve",
		Short: "Hold nodes aside for a window of time",
	}
	cmd.AddCommand(reserveCreateCmd(cfgLoader), reserveListCmd(cfgLoader), reserveDeleteCmd(cfgLoader))
	return cmd
}

func reserveCreateCmd(cfgLoader func() *config.Config) *cobra.Command {
	var nodes []string
	var startAt, duration string
	var users, accounts []string
	var reason string

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Reserve nodes for a window",
		Long: `Reserve nodes for a window of time.

With neither --user nor --account, nobody may run on the nodes during the window: that is a
maintenance reservation. The scheduler drains the nodes for you — it stops placing jobs that
could still be running when the window opens, so nothing has to be killed when it does.

A job with no walltime can never be placed on a node with a reservation ahead of it, because
there is no way to promise it will have finished.

Examples:
  tasch reserve create --nodes gpu-01,gpu-02 --start 2026-09-11T22:00:00Z --for 4h --reason "firmware"
  tasch reserve create --nodes gpu-03 --for 12h --account research --reason "paper deadline"`,
		Run: func(cmd *cobra.Command, args []string) {
			if len(nodes) == 0 {
				log.Fatal("--nodes is required")
			}
			start := time.Now()
			if startAt != "" {
				parsed, err := time.Parse(time.RFC3339, startAt)
				if err != nil {
					log.Fatalf("--start must be RFC3339 (like 2026-09-11T22:00:00Z): %v", err)
				}
				start = parsed
			}
			window, err := time.ParseDuration(duration)
			if err != nil {
				log.Fatalf("--for must be a duration like 4h or 90m: %v", err)
			}

			client, conn := GetClient(cfgLoader())
			defer func() { _ = conn.Close() }()

			resp, err := client.CreateReservation(context.Background(), &pb.CreateReservationRequest{
				Nodes:     nodes,
				StartTime: start.Unix(),
				EndTime:   start.Add(window).Unix(),
				Users:     users,
				Accounts:  accounts,
				Reason:    reason,
			})
			if err != nil {
				log.Fatalf("Could not create the reservation: %v", err)
			}
			fmt.Printf("Reservation created.\n  ID:     %s\n  %s\n", resp.ReservationId, resp.Status)
			if len(users) == 0 && len(accounts) == 0 {
				fmt.Println("  Nobody may run on these nodes during the window.")
			}
		},
	}
	cmd.Flags().StringSliceVar(&nodes, "nodes", nil, "Nodes to reserve (comma-separated)")
	cmd.Flags().StringVar(&startAt, "start", "", "When the window opens, RFC3339 (default: now)")
	cmd.Flags().StringVar(&duration, "for", "1h", "How long the window lasts, e.g. 4h or 90m")
	cmd.Flags().StringSliceVar(&users, "user", nil, "Users allowed to run during the window")
	cmd.Flags().StringSliceVar(&accounts, "account", nil, "Accounts allowed to run during the window")
	cmd.Flags().StringVar(&reason, "reason", "", "Why the nodes are reserved")
	return cmd
}

func reserveListCmd(cfgLoader func() *config.Config) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List reservations, soonest first",
		Run: func(cmd *cobra.Command, args []string) {
			client, conn := GetClient(cfgLoader())
			defer func() { _ = conn.Close() }()

			resp, err := client.ListReservations(context.Background(), &pb.ListReservationsRequest{})
			if err != nil {
				log.Fatalf("Could not list reservations: %v", err)
			}
			if len(resp.Reservations) == 0 {
				fmt.Println("No reservations.")
				return
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tSTATE\tNODES\tFROM\tUNTIL\tFOR\tREASON")
			for _, r := range resp.Reservations {
				state := "upcoming"
				if r.Active {
					state = "ACTIVE"
				}
				who := "maintenance"
				if len(r.Users) > 0 || len(r.Accounts) > 0 {
					who = strings.Join(append(append([]string{}, r.Users...), r.Accounts...), ",")
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.ReservationId, state, strings.Join(r.Nodes, ","),
					time.Unix(r.StartTime, 0).Format(time.RFC3339),
					time.Unix(r.EndTime, 0).Format(time.RFC3339),
					who, r.Reason)
			}
			_ = w.Flush()
		},
	}
}

func reserveDeleteCmd(cfgLoader func() *config.Config) *cobra.Command {
	return &cobra.Command{
		Use:   "delete [reservation_id]",
		Short: "Release a reservation early",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			client, conn := GetClient(cfgLoader())
			defer func() { _ = conn.Close() }()

			resp, err := client.DeleteReservation(context.Background(), &pb.DeleteReservationRequest{
				ReservationId: args[0],
			})
			if err != nil {
				log.Fatalf("Could not delete the reservation: %v", err)
			}
			if !resp.Deleted {
				fmt.Printf("No reservation %s.\n", args[0])
				return
			}
			fmt.Printf("Reservation %s released.\n", args[0])
		},
	}
}
