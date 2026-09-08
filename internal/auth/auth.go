// Package auth provides token authentication and per-job authorization for the gRPC API.
//
// Before this existed, the API had no authentication of any kind: any host that could reach the
// gRPC port could submit a job — which the worker runs as a shell command — cancel anyone's
// job, or forge a result. The `--user` flag was a free-text label used for fairshare, not an
// identity, so it could be set to any value and impersonated at will.
package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"

	"github.com/deziss/tasch/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Role determines what a principal may do.
type Role string

const (
	// RoleUser may submit jobs and act on its own jobs.
	RoleUser Role = "user"
	// RoleAdmin may act on any job.
	RoleAdmin Role = "admin"
	// RoleWorker may only report job results back to the master.
	RoleWorker Role = "worker"
)

// MetadataKey is the gRPC metadata key carrying the bearer token.
const MetadataKey = "authorization"

// Principal is an authenticated caller.
type Principal struct {
	Name string
	Role Role
}

// Anonymous is the principal used when authentication is disabled.
var Anonymous = &Principal{Name: "anonymous", Role: RoleAdmin}

type principalKey struct{}

// FromContext returns the authenticated principal for a request.
//
// When authentication is disabled there is no principal on the context, and the caller is
// treated as anonymous with full access — which is the documented, pre-auth behaviour.
func FromContext(ctx context.Context) *Principal {
	if p, ok := ctx.Value(principalKey{}).(*Principal); ok && p != nil {
		return p
	}
	return Anonymous
}

// Authenticator verifies bearer tokens against the configured principals.
type Authenticator struct {
	enabled bool

	// OnFailure, if set, is called with a short reason each time a request is rejected. The
	// daemon uses it to drive a metric without this package importing Prometheus.
	OnFailure func(reason string)
	// byToken maps a token to its principal. Lookup is linear and constant-time per entry to
	// avoid leaking which prefix of a guessed token was correct.
	principals []tokenPrincipal
}

type tokenPrincipal struct {
	token     []byte
	principal *Principal
}

// New builds an Authenticator from config. It returns an Authenticator that permits everything
// when auth is disabled, so callers need no conditional logic.
func New(cfg *config.Config) (*Authenticator, error) {
	if !cfg.Auth.Enabled {
		return &Authenticator{enabled: false}, nil
	}

	configured, err := cfg.Principals()
	if err != nil {
		return nil, err
	}
	if len(configured) == 0 {
		return nil, fmt.Errorf("auth is enabled but no principals are configured")
	}

	a := &Authenticator{enabled: true}
	for _, p := range configured {
		a.principals = append(a.principals, tokenPrincipal{
			token:     []byte(p.Token),
			principal: &Principal{Name: p.Name, Role: Role(p.Role)},
		})
	}
	return a, nil
}

// Enabled reports whether tokens are being checked.
func (a *Authenticator) Enabled() bool { return a.enabled }

// authenticate resolves a token to a principal.
func (a *Authenticator) authenticate(token string) (*Principal, bool) {
	candidate := []byte(token)
	var found *Principal
	// Compare against every principal rather than returning early, so the time taken does not
	// reveal how many principals were checked before a match.
	for _, tp := range a.principals {
		if subtle.ConstantTimeCompare(tp.token, candidate) == 1 {
			found = tp.principal
		}
	}
	return found, found != nil
}

// tokenFromMetadata extracts a bearer token from request metadata.
func tokenFromMetadata(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	values := md.Get(MetadataKey)
	if len(values) == 0 {
		return "", false
	}
	token := strings.TrimSpace(values[0])
	token = strings.TrimPrefix(token, "Bearer ")
	token = strings.TrimPrefix(token, "bearer ")
	if token == "" {
		return "", false
	}
	return token, true
}

// methodsForWorkers are the only RPCs a worker-role principal may call.
var methodsForWorkers = map[string]bool{
	"/v1.SchedulerService/ReportResult":     true,
	"/v1.SchedulerService/WorkerStatus":     true,
	"/v1.SchedulerService/WatchDispatch":    true,
	"/v1.SchedulerService/AcknowledgeStart": true,
}

// authorizeMethod applies role-level restrictions that do not depend on a specific job.
func authorizeMethod(p *Principal, method string) error {
	if p.Role == RoleWorker && !methodsForWorkers[method] {
		return status.Errorf(codes.PermissionDenied, "worker principals may not call %s", method)
	}
	return nil
}

// UnaryInterceptor authenticates and authorizes unary calls.
func (a *Authenticator) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if !a.enabled {
			return handler(ctx, req)
		}
		p, err := a.principalFor(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(context.WithValue(ctx, principalKey{}, p), req)
	}
}

// StreamInterceptor authenticates and authorizes streaming calls.
func (a *Authenticator) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !a.enabled {
			return handler(srv, ss)
		}
		p, err := a.principalFor(ss.Context(), info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &authenticatedStream{
			ServerStream: ss,
			ctx:          context.WithValue(ss.Context(), principalKey{}, p),
		})
	}
}

func (a *Authenticator) principalFor(ctx context.Context, method string) (*Principal, error) {
	token, ok := tokenFromMetadata(ctx)
	if !ok {
		a.reportFailure("missing_token")
		return nil, status.Error(codes.Unauthenticated, "missing authorization token")
	}
	p, ok := a.authenticate(token)
	if !ok {
		a.reportFailure("invalid_token")
		return nil, status.Error(codes.Unauthenticated, "invalid authorization token")
	}
	if err := authorizeMethod(p, method); err != nil {
		a.reportFailure("forbidden_method")
		return nil, err
	}
	return p, nil
}

func (a *Authenticator) reportFailure(reason string) {
	if a.OnFailure != nil {
		a.OnFailure(reason)
	}
}

// authenticatedStream carries the principal on the stream's context.
type authenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authenticatedStream) Context() context.Context { return s.ctx }

// CanAccessJob reports whether the principal may read or modify a job owned by jobOwner.
//
// Admins may act on any job. Everyone else is limited to their own, which is what stops one
// client from cancelling or reading the logs of another client's work.
func CanAccessJob(p *Principal, jobOwner string) bool {
	if p == nil {
		return true
	}
	if p.Role == RoleAdmin {
		return true
	}
	return p.Name == jobOwner
}

// ErrJobForbidden is the error returned when a principal may not touch a job. It deliberately
// does not distinguish "no such job" from "not yours", so job IDs cannot be enumerated.
func ErrJobForbidden(jobID string) error {
	return status.Errorf(codes.NotFound, "job %s not found", jobID)
}

// TokenCredentials supplies a bearer token on outgoing calls.
type TokenCredentials struct {
	Token string
	// Insecure allows sending the token over a non-TLS connection. gRPC refuses by default,
	// which is the right default — but Tasch supports plaintext clusters, so this is explicit.
	Insecure bool
}

// GetRequestMetadata implements credentials.PerRPCCredentials.
func (c TokenCredentials) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{MetadataKey: "Bearer " + c.Token}, nil
}

// RequireTransportSecurity implements credentials.PerRPCCredentials.
func (c TokenCredentials) RequireTransportSecurity() bool { return !c.Insecure }
