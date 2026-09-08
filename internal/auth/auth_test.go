package auth

import (
	"context"
	"testing"

	"github.com/deziss/tasch/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func authedConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = true
	cfg.Auth.Principals = []config.Principal{
		{Name: "alice", Token: "tok-alice", Role: "user"},
		{Name: "bob", Token: "tok-bob", Role: "user"},
		{Name: "root", Token: "tok-root", Role: "admin"},
		{Name: "node1", Token: "tok-node1", Role: "worker"},
	}
	return cfg
}

func ctxWithToken(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(MetadataKey, "Bearer "+token))
}

// TestUnaryInterceptorRejectsUnauthenticated is the regression test for the core defect: the
// gRPC API had no interceptors at all, so any host that could reach the port could submit a
// job — which the worker executes as a shell command on every node.
func TestUnaryInterceptorRejectsUnauthenticated(t *testing.T) {
	a, err := New(authedConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	interceptor := a.UnaryInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/v1.SchedulerService/SubmitJob"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) { return "ran", nil }

	cases := []struct {
		name string
		ctx  context.Context
	}{
		{"no metadata", context.Background()},
		{"empty token", ctxWithToken("")},
		{"wrong token", ctxWithToken("tok-guessed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := interceptor(tc.ctx, nil, info, handler)
			if err == nil {
				t.Fatal("the call was allowed through without a valid token")
			}
			if status.Code(err) != codes.Unauthenticated {
				t.Errorf("code = %s, want Unauthenticated", status.Code(err))
			}
		})
	}
}

// TestUnaryInterceptorAcceptsValidToken confirms a good token reaches the handler with its
// principal attached.
func TestUnaryInterceptorAcceptsValidToken(t *testing.T) {
	a, _ := New(authedConfig())
	interceptor := a.UnaryInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/v1.SchedulerService/SubmitJob"}

	var seen *Principal
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		seen = FromContext(ctx)
		return "ran", nil
	}

	if _, err := interceptor(ctxWithToken("tok-alice"), nil, info, handler); err != nil {
		t.Fatalf("a valid token was rejected: %v", err)
	}
	if seen == nil || seen.Name != "alice" {
		t.Fatalf("principal = %+v, want alice", seen)
	}
	if seen.Role != RoleUser {
		t.Errorf("role = %s, want user", seen.Role)
	}
}

// TestWorkerRoleIsLimitedToReporting confirms a compromised worker token cannot submit jobs.
func TestWorkerRoleIsLimitedToReporting(t *testing.T) {
	a, _ := New(authedConfig())
	interceptor := a.UnaryInterceptor()
	handler := func(ctx context.Context, req interface{}) (interface{}, error) { return "ran", nil }

	allowed := &grpc.UnaryServerInfo{FullMethod: "/v1.SchedulerService/ReportResult"}
	if _, err := interceptor(ctxWithToken("tok-node1"), nil, allowed, handler); err != nil {
		t.Errorf("a worker could not report a result: %v", err)
	}

	denied := &grpc.UnaryServerInfo{FullMethod: "/v1.SchedulerService/SubmitJob"}
	_, err := interceptor(ctxWithToken("tok-node1"), nil, denied, handler)
	if err == nil {
		t.Fatal("a worker principal was allowed to submit a job")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("code = %s, want PermissionDenied", status.Code(err))
	}
}

// TestDisabledAuthAllowsEverything confirms the pre-auth behaviour is preserved when auth is
// off, so upgrading does not silently break an existing trusted-network cluster.
func TestDisabledAuthAllowsEverything(t *testing.T) {
	cfg := config.DefaultConfig() // auth.enabled defaults to false
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Enabled() {
		t.Fatal("auth reports enabled when it is not configured")
	}

	interceptor := a.UnaryInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/v1.SchedulerService/SubmitJob"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) { return "ran", nil }

	if _, err := interceptor(context.Background(), nil, info, handler); err != nil {
		t.Fatalf("a call was rejected while auth is disabled: %v", err)
	}
}

// TestCanAccessJob covers the ownership rule that stops one client cancelling or reading
// another's work.
func TestCanAccessJob(t *testing.T) {
	alice := &Principal{Name: "alice", Role: RoleUser}
	bob := &Principal{Name: "bob", Role: RoleUser}
	admin := &Principal{Name: "root", Role: RoleAdmin}

	if !CanAccessJob(alice, "alice") {
		t.Error("alice cannot access her own job")
	}
	if CanAccessJob(alice, "bob") {
		t.Error("alice can access bob's job")
	}
	if !CanAccessJob(admin, "bob") {
		t.Error("an admin cannot access another user's job")
	}
	if !CanAccessJob(bob, "bob") {
		t.Error("bob cannot access his own job")
	}
	// Anonymous (auth disabled) retains full access.
	if !CanAccessJob(Anonymous, "alice") {
		t.Error("the anonymous principal lost access when auth is disabled")
	}
}

// TestForbiddenLooksLikeNotFound confirms the API cannot be used to enumerate other principals'
// job IDs by distinguishing "not yours" from "does not exist".
func TestForbiddenLooksLikeNotFound(t *testing.T) {
	err := ErrJobForbidden("abc123")
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %s, want NotFound", status.Code(err))
	}
}

// TestNewRejectsEnabledAuthWithNoPrincipals confirms a misconfiguration fails loudly rather
// than starting a server that rejects everyone.
func TestNewRejectsEnabledAuthWithNoPrincipals(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = true
	if _, err := New(cfg); err == nil {
		t.Fatal("auth was enabled with no principals and no error was raised")
	}
}

// TestStreamInterceptorAuthenticates covers the streaming path, which carries job logs.
func TestStreamInterceptorAuthenticates(t *testing.T) {
	a, _ := New(authedConfig())
	interceptor := a.StreamInterceptor()
	info := &grpc.StreamServerInfo{FullMethod: "/v1.SchedulerService/StreamLogs"}
	handler := func(srv interface{}, stream grpc.ServerStream) error { return nil }

	if err := interceptor(nil, fakeStream{ctx: context.Background()}, info, handler); err == nil {
		t.Fatal("an unauthenticated log stream was allowed")
	}
	if err := interceptor(nil, fakeStream{ctx: ctxWithToken("tok-alice")}, info, handler); err != nil {
		t.Fatalf("an authenticated log stream was rejected: %v", err)
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeStream) Context() context.Context { return f.ctx }
