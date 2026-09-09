package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/api/v1/v1connect"
	"github.com/deziss/tasch/internal/auth"
	"github.com/deziss/tasch/internal/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The HTTP API.
//
// A browser cannot speak gRPC — it has no way to produce the trailers and framing the protocol
// needs — so nothing could talk to Tasch from a web page, which is why there was no web UI to
// have. Connect fixes that without a second API: one handler serves the Connect protocol
// (JSON over plain HTTP POST), gRPC-Web, and gRPC itself, all from the service definition the
// CLI and workers already use. There is nothing here to keep in step with the gRPC surface,
// because it *is* the gRPC surface.
//
// Every method below is a thin adapter over the same schedulerServer method the gRPC server
// calls. That is deliberate: authorization, leader redirects, quota checks and ownership rules
// live in one implementation, so an HTTP caller cannot reach a path with weaker rules than a
// gRPC caller.

// connectAPI adapts schedulerServer to the Connect handler interface.
type connectAPI struct {
	srv *schedulerServer
}

// startHTTPAPI serves the scheduler API over HTTP, returning nil when it is not enabled.
func startHTTPAPI(srv *schedulerServer, cfg *config.Config, authn *auth.Authenticator) *http.Server {
	if !cfg.API.Enabled {
		return nil
	}

	mux := http.NewServeMux()
	path, handler := v1connect.NewSchedulerServiceHandler(
		&connectAPI{srv: srv},
		connect.WithInterceptors(connectAuthInterceptor(authn)),
	)
	mux.Handle(path, handler)

	root := withCORS(cfg.API.CORSOrigins, mux)

	// Serve HTTP/2 without TLS as well as HTTP/1.1, so the endpoint answers gRPC and gRPC-Web
	// clients even when TLS is terminated by a proxy in front of it. Connect itself only needs
	// HTTP/1.1; the others require HTTP/2 framing.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)

	server := &http.Server{
		Addr:      cfg.API.Bind,
		Handler:   root,
		Protocols: protocols,
		// The health server's timeouts exist for the same reason: a client that opens a
		// connection and never sends a request must not hold a handler forever.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      0, // streaming responses have no fixed length
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		var err error
		if cfg.API.TLS && cfg.TLS.Enabled {
			err = server.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			err = server.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			slog.Error("http api server stopped", "error", err)
		}
	}()

	slog.Info("http api listening", "addr", cfg.API.Bind, "tls", cfg.API.TLS && cfg.TLS.Enabled,
		"cors_origins", cfg.API.CORSOrigins)
	if !cfg.API.TLS && !strings.HasPrefix(cfg.API.Bind, "127.0.0.1") &&
		!strings.HasPrefix(cfg.API.Bind, "localhost") {
		slog.Warn("the http api is bound off-loopback without TLS, so tokens and job commands " +
			"travel in cleartext; put it behind a TLS-terminating proxy or set api.tls")
	}
	return server
}

// connectAuthInterceptor applies the same token authentication the gRPC interceptors do.
func connectAuthInterceptor(authn *auth.Authenticator) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			principal, err := authn.PrincipalForHeader(
				req.Header().Get("Authorization"), req.Spec().Procedure)
			if err != nil {
				return nil, connectError(err)
			}
			return next(auth.WithPrincipal(ctx, principal), req)
		}
	}
}

// withCORS answers browser preflights for the configured origins.
//
// The allow-list is exact and never "*". This endpoint accepts job submissions, so a wildcard
// would let any page a user visits run commands on the cluster with a token that user had
// granted to something else entirely.
func withCORS(origins []string, next http.Handler) http.Handler {
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[strings.TrimSuffix(o, "/")] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSuffix(r.Header.Get("Origin"), "/")
		if origin != "" && allowed[origin] {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			// Vary, so a cache cannot serve one origin's allowance to another.
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			h.Set("Access-Control-Allow-Headers",
				"Content-Type, Authorization, Connect-Protocol-Version, Connect-Timeout-Ms, "+
					"Grpc-Timeout, X-Grpc-Web, X-User-Agent")
			h.Set("Access-Control-Expose-Headers",
				"Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin")
			h.Set("Access-Control-Max-Age", "7200")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// connectError maps a gRPC status onto a Connect error, so an HTTP client sees the same code
// the gRPC one would.
func connectError(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewError(connect.Code(st.Code()), fmt.Errorf("%s", st.Message()))
}

// --- Method adapters ---
//
// Each one calls the identical schedulerServer method the gRPC server calls, so both transports
// share every rule about who may do what.

func (a *connectAPI) SubmitJob(ctx context.Context, req *connect.Request[pb.SubmitJobRequest]) (*connect.Response[pb.SubmitJobResponse], error) {
	resp, err := a.srv.SubmitJob(ctx, req.Msg)
	return connectResult(resp, err)
}

func (a *connectAPI) SubmitDistributedJob(ctx context.Context, req *connect.Request[pb.SubmitDistributedJobRequest]) (*connect.Response[pb.SubmitDistributedJobResponse], error) {
	resp, err := a.srv.SubmitDistributedJob(ctx, req.Msg)
	return connectResult(resp, err)
}

func (a *connectAPI) CancelJob(ctx context.Context, req *connect.Request[pb.CancelJobRequest]) (*connect.Response[pb.CancelJobResponse], error) {
	resp, err := a.srv.CancelJob(ctx, req.Msg)
	return connectResult(resp, err)
}

func (a *connectAPI) GetJobStatus(ctx context.Context, req *connect.Request[pb.GetJobStatusRequest]) (*connect.Response[pb.GetJobStatusResponse], error) {
	resp, err := a.srv.GetJobStatus(ctx, req.Msg)
	return connectResult(resp, err)
}

func (a *connectAPI) ListJobs(ctx context.Context, req *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error) {
	resp, err := a.srv.ListJobs(ctx, req.Msg)
	return connectResult(resp, err)
}

func (a *connectAPI) WorkerStatus(ctx context.Context, req *connect.Request[pb.WorkerStatusRequest]) (*connect.Response[pb.WorkerStatusResponse], error) {
	resp, err := a.srv.WorkerStatus(ctx, req.Msg)
	return connectResult(resp, err)
}

func (a *connectAPI) ClusterStatus(ctx context.Context, req *connect.Request[pb.ClusterStatusRequest]) (*connect.Response[pb.ClusterStatusResponse], error) {
	resp, err := a.srv.ClusterStatus(ctx, req.Msg)
	return connectResult(resp, err)
}

func (a *connectAPI) CordonNode(ctx context.Context, req *connect.Request[pb.CordonNodeRequest]) (*connect.Response[pb.CordonNodeResponse], error) {
	resp, err := a.srv.CordonNode(ctx, req.Msg)
	return connectResult(resp, err)
}

func (a *connectAPI) CreateReservation(ctx context.Context, req *connect.Request[pb.CreateReservationRequest]) (*connect.Response[pb.CreateReservationResponse], error) {
	resp, err := a.srv.CreateReservation(ctx, req.Msg)
	return connectResult(resp, err)
}

func (a *connectAPI) DeleteReservation(ctx context.Context, req *connect.Request[pb.DeleteReservationRequest]) (*connect.Response[pb.DeleteReservationResponse], error) {
	resp, err := a.srv.DeleteReservation(ctx, req.Msg)
	return connectResult(resp, err)
}

func (a *connectAPI) ListReservations(ctx context.Context, req *connect.Request[pb.ListReservationsRequest]) (*connect.Response[pb.ListReservationsResponse], error) {
	resp, err := a.srv.ListReservations(ctx, req.Msg)
	return connectResult(resp, err)
}

func (a *connectAPI) AcknowledgeStart(ctx context.Context, req *connect.Request[pb.AcknowledgeStartRequest]) (*connect.Response[pb.AcknowledgeStartResponse], error) {
	resp, err := a.srv.AcknowledgeStart(ctx, req.Msg)
	return connectResult(resp, err)
}

func (a *connectAPI) ReportResult(ctx context.Context, req *connect.Request[pb.ReportResultRequest]) (*connect.Response[pb.ReportResultResponse], error) {
	resp, err := a.srv.ReportResult(ctx, req.Msg)
	return connectResult(resp, err)
}

// StreamLogs streams a job's log lines, which is what makes a live view in a browser possible.
func (a *connectAPI) StreamLogs(ctx context.Context, req *connect.Request[pb.LogStreamRequest],
	stream *connect.ServerStream[pb.LogMessage]) error {
	return connectError(a.srv.StreamLogs(req.Msg, &connectLogStream{ctx: ctx, stream: stream}))
}

// WatchDispatch is how a worker receives its work, and it is deliberately not served here.
//
// Workers connect over gRPC with a long-lived stream and a client certificate. Exposing the
// dispatch channel on the browser-facing endpoint would widen the blast radius of a leaked
// user token from "can submit jobs" to "can receive every other node's work, including the
// environment variables in it".
func (a *connectAPI) WatchDispatch(ctx context.Context, req *connect.Request[pb.WatchDispatchRequest],
	stream *connect.ServerStream[pb.DispatchMessage]) error {
	return connect.NewError(connect.CodeUnimplemented,
		fmt.Errorf("workers receive dispatch over gRPC, not over the HTTP API"))
}

// connectResult wraps a gRPC-style result pair into a Connect response.
func connectResult[T any](msg *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(msg), nil
}

// connectLogStream adapts a Connect server stream to the gRPC stream interface StreamLogs
// expects, so the handler itself does not need to know which transport it is serving.
type connectLogStream struct {
	pb.SchedulerService_StreamLogsServer
	ctx    context.Context
	stream *connect.ServerStream[pb.LogMessage]
}

func (s *connectLogStream) Context() context.Context { return s.ctx }

func (s *connectLogStream) Send(msg *pb.LogMessage) error {
	if err := s.stream.Send(msg); err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	return nil
}
