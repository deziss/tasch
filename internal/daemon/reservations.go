package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/auth"
	"github.com/deziss/tasch/internal/ha"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The reservation API.
//
// The alternative an operator has without this is a cordon, which is the same idea with the
// timing removed. Cordoning a node an hour before a maintenance window wastes the hour;
// cordoning it at the start of the window leaves whatever is still running to be killed. A
// reservation carries the window, so the scheduler stops placing work that would run into it
// and lets everything that fits finish — the node empties itself, on time, with nothing lost.

// CreateReservation holds a set of nodes for a window of time.
func (s *schedulerServer) CreateReservation(ctx context.Context, req *pb.CreateReservationRequest) (*pb.CreateReservationResponse, error) {
	// Reserving nodes takes capacity away from everyone, not just the caller.
	if auth.FromContext(ctx).Role != auth.RoleAdmin {
		return nil, status.Error(codes.PermissionDenied, "creating a reservation requires an admin principal")
	}
	if len(req.Nodes) == 0 {
		return nil, status.Error(codes.InvalidArgument, "a reservation needs at least one node")
	}

	start := time.Unix(req.StartTime, 0)
	end := time.Unix(req.EndTime, 0)
	if req.StartTime == 0 {
		start = time.Now()
	}
	if !end.After(start) {
		return nil, status.Errorf(codes.InvalidArgument,
			"a reservation must end after it starts (start %s, end %s)",
			start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	if end.Before(time.Now()) {
		return nil, status.Error(codes.InvalidArgument,
			"this reservation has already ended, so it would take effect on nothing")
	}

	res := ha.Reservation{
		ID:        newJobID(),
		Nodes:     req.Nodes,
		Start:     start,
		End:       end,
		Users:     req.Users,
		Accounts:  req.Accounts,
		Reason:    req.Reason,
		CreatedBy: auth.FromContext(ctx).Name,
		CreatedAt: time.Now(),
	}
	if err := s.state.AddReservation(res); err != nil {
		return nil, leaderRedirect(s, err)
	}
	s.persistReservations()

	slog.Warn("nodes reserved", "reservation_id", res.ID, "nodes", res.Nodes,
		"start", res.Start, "end", res.End, "maintenance", res.Maintenance(), "reason", res.Reason)

	return &pb.CreateReservationResponse{
		ReservationId: res.ID,
		Status: fmt.Sprintf("%d node(s) reserved from %s to %s",
			len(res.Nodes), res.Start.Format(time.RFC3339), res.End.Format(time.RFC3339)),
	}, nil
}

// DeleteReservation releases a reservation early.
func (s *schedulerServer) DeleteReservation(ctx context.Context, req *pb.DeleteReservationRequest) (*pb.DeleteReservationResponse, error) {
	if auth.FromContext(ctx).Role != auth.RoleAdmin {
		return nil, status.Error(codes.PermissionDenied, "deleting a reservation requires an admin principal")
	}
	deleted, err := s.state.RemoveReservation(req.ReservationId)
	if err != nil {
		return nil, leaderRedirect(s, err)
	}
	s.persistReservations()
	if deleted {
		slog.Info("reservation released", "reservation_id", req.ReservationId)
	}
	return &pb.DeleteReservationResponse{ReservationId: req.ReservationId, Deleted: deleted}, nil
}

// ListReservations reports every reservation, soonest first.
func (s *schedulerServer) ListReservations(ctx context.Context, _ *pb.ListReservationsRequest) (*pb.ListReservationsResponse, error) {
	now := time.Now()
	entries := s.reservations.List()
	out := make([]*pb.ReservationInfo, 0, len(entries))
	for _, r := range entries {
		out = append(out, &pb.ReservationInfo{
			ReservationId: r.ID, Nodes: r.Nodes,
			StartTime: r.Start.Unix(), EndTime: r.End.Unix(),
			Users: r.Users, Accounts: r.Accounts,
			Reason: r.Reason, CreatedBy: r.CreatedBy,
			Active: r.Active(now),
		})
	}
	return &pb.ListReservationsResponse{Reservations: out}, nil
}
