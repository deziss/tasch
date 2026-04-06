package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/internal/store"
	"github.com/deziss/tasch/pkg/discovery"
	"github.com/deziss/tasch/pkg/matchmaker"
	"github.com/deziss/tasch/pkg/messaging"
	"github.com/deziss/tasch/pkg/scheduler"
	"github.com/google/uuid"
	"github.com/hashicorp/memberlist"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// MasterHandle is returned by StartMaster for shutdown orchestration.
type MasterHandle struct {
	Cancel   func()
	Draining *atomic.Bool
	Queue    *scheduler.GlobalScheduler
}

type schedulerServer struct {
	pb.UnimplementedSchedulerServiceServer
	disc      *discovery.NodeDiscovery
	queue     *scheduler.GlobalScheduler
	eval      *matchmaker.Evaluator
	pub       *messaging.ZMQPublisher
	fairshare *scheduler.FairshareCalculator
	store     *store.Store
	cfg       *config.Config
	draining  *atomic.Bool

	// Circuit breaker
	cb *circuitBreaker

	// GPU resource tracking
	gpuTracker *gpuTracker

	logMu       sync.Mutex
	logStore    map[string][]*pb.LogMessage
	subMu       sync.Mutex
	logChannels map[string][]chan *pb.LogMessage
}

// --- Circuit Breaker ---

type circuitBreaker struct {
	mu       sync.Mutex
	failures map[string]int       // node → consecutive failures
	blocked  map[string]time.Time // node → blocked until
}

func newCircuitBreaker() *circuitBreaker {
	return &circuitBreaker{
		failures: make(map[string]int),
		blocked:  make(map[string]time.Time),
	}
}

func (cb *circuitBreaker) RecordFailure(node string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures[node]++
	if cb.failures[node] >= 3 {
		cb.blocked[node] = time.Now().Add(5 * time.Minute)
		fmt.Printf("[circuit-breaker] Node %s blocked for 5 minutes (%d consecutive failures)\n", node, cb.failures[node])
	}
}

func (cb *circuitBreaker) RecordSuccess(node string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	delete(cb.failures, node)
	delete(cb.blocked, node)
}

func (cb *circuitBreaker) IsBlocked(node string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	until, ok := cb.blocked[node]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(cb.blocked, node)
		delete(cb.failures, node)
		return false
	}
	return true
}

// --- GPU Resource Tracker ---

type gpuTracker struct {
	mu        sync.Mutex
	allocated map[string]int // node → GPUs currently in use
}

func newGPUTracker() *gpuTracker {
	return &gpuTracker{allocated: make(map[string]int)}
}

func (gt *gpuTracker) Allocate(node string, count int) {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	gt.allocated[node] += count
}

func (gt *gpuTracker) Release(node string, count int) {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	gt.allocated[node] -= count
	if gt.allocated[node] <= 0 {
		delete(gt.allocated, node)
	}
}

func (gt *gpuTracker) Available(node string, totalGPUs int) int {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	return totalGPUs - gt.allocated[node]
}

// --- Log helpers ---

func (s *schedulerServer) appendLog(jobID, level, msg string) {
	entry := &pb.LogMessage{
		Timestamp: time.Now().UnixMilli(), Level: level, Message: msg, JobId: jobID,
	}
	s.logMu.Lock()
	s.logStore[jobID] = append(s.logStore[jobID], entry)
	s.logMu.Unlock()

	s.subMu.Lock()
	for _, ch := range s.logChannels[jobID] {
		select {
		case ch <- entry:
		default:
		}
	}
	s.subMu.Unlock()
}

// --- gRPC Handlers ---

func (s *schedulerServer) WorkerStatus(ctx context.Context, req *pb.WorkerStatusRequest) (*pb.WorkerStatusResponse, error) {
	members := s.disc.Members()
	nodes := make(map[string]string)
	for _, member := range members {
		if len(member.Meta) > 0 {
			nodes[member.Name] = string(member.Meta)
		}
	}
	return &pb.WorkerStatusResponse{WorkerNodes: nodes}, nil
}

func (s *schedulerServer) SubmitJob(ctx context.Context, req *pb.SubmitJobRequest) (*pb.SubmitJobResponse, error) {
	if s.draining.Load() {
		return nil, status.Error(codes.Unavailable, "master is draining, not accepting new jobs")
	}

	jobID := uuid.New().String()[:8]
	priority := int(req.Priority)
	if priority == 0 {
		priority = 10
	}
	user := req.User
	if user == "" {
		user = "anonymous"
	}
	penalty := s.fairshare.CalculatePenalty(user)
	effectivePriority := priority + penalty

	job := &scheduler.Job{
		ID: jobID, Requirement: req.CelRequirement, Command: req.Command,
		SubmitTime: time.Now(), Priority: effectivePriority, User: user,
		WalltimeSeconds: int(req.WalltimeSeconds), GPUsRequired: int(req.GpusRequired),
		EnvVars: req.EnvVars, MaxRetries: s.cfg.MaxRetries,
	}

	if err := s.queue.Enqueue(job); err != nil {
		return nil, status.Errorf(codes.ResourceExhausted, "queue full: %v", err)
	}

	jobsSubmittedTotal.WithLabelValues(user).Inc()
	s.appendLog(jobID, "INFO", fmt.Sprintf("Job queued | Priority: %d | GPUs: %d | Retries: %d | Expr: '%s'", effectivePriority, req.GpusRequired, s.cfg.MaxRetries, req.CelRequirement))
	fmt.Printf("Job %s queued | User: %s, Priority: %d, GPUs: %d\n", jobID, user, effectivePriority, req.GpusRequired)
	return &pb.SubmitJobResponse{JobId: jobID, Status: "QUEUED"}, nil
}

func (s *schedulerServer) SubmitDistributedJob(ctx context.Context, req *pb.SubmitDistributedJobRequest) (*pb.SubmitDistributedJobResponse, error) {
	if s.draining.Load() {
		return nil, status.Error(codes.Unavailable, "master is draining")
	}

	groupID := "dj-" + uuid.New().String()[:8]
	masterPort := int(req.MasterPort)
	if masterPort == 0 {
		masterPort = 29500
	}
	priority := int(req.Priority)
	if priority == 0 {
		priority = 10
	}
	user := req.User
	if user == "" {
		user = "anonymous"
	}
	numNodes := int(req.NumNodes)
	if numNodes < 1 {
		numNodes = 1
	}
	gpusPerNode := int(req.GpusPerNode)

	var jobIDs []string
	for rank := 0; rank < numNodes; rank++ {
		jobID := fmt.Sprintf("%s-r%d", groupID, rank)
		envVars := make(map[string]string)
		for k, v := range req.EnvVars {
			envVars[k] = v
		}
		envVars["RANK"] = strconv.Itoa(rank)
		envVars["WORLD_SIZE"] = strconv.Itoa(numNodes)
		envVars["MASTER_PORT"] = strconv.Itoa(masterPort)
		envVars["LOCAL_RANK"] = "0"
		envVars["NPROC_PER_NODE"] = strconv.Itoa(gpusPerNode)

		job := &scheduler.Job{
			ID: jobID, GroupID: groupID, Requirement: req.CelRequirement,
			Command: req.Command, SubmitTime: time.Now(), Priority: priority,
			User: user, WalltimeSeconds: int(req.WalltimeSeconds),
			GPUsRequired: gpusPerNode, EnvVars: envVars, MaxRetries: 0, // No retry for distributed
		}
		if err := s.queue.Enqueue(job); err != nil {
			return nil, status.Errorf(codes.ResourceExhausted, "queue full: %v", err)
		}
		jobIDs = append(jobIDs, jobID)
		s.appendLog(jobID, "INFO", fmt.Sprintf("Distributed job rank %d/%d queued in group %s", rank, numNodes, groupID))
	}

	s.queue.RegisterGroup(&scheduler.JobGroup{
		GroupID: groupID, JobIDs: jobIDs, NumNodes: numNodes,
		GPUsPerNode: gpusPerNode, MasterPort: masterPort, State: "PENDING",
		CreatedAt: time.Now(),
	})
	jobsSubmittedTotal.WithLabelValues(user).Add(float64(numNodes))
	fmt.Printf("Distributed job %s queued | %d nodes x %d GPUs | User: %s\n", groupID, numNodes, gpusPerNode, user)
	return &pb.SubmitDistributedJobResponse{GroupId: groupID, JobIds: jobIDs, Status: "QUEUED"}, nil
}

func (s *schedulerServer) CancelJob(ctx context.Context, req *pb.CancelJobRequest) (*pb.CancelJobResponse, error) {
	job, ok := s.queue.Cancel(req.JobId)
	if !ok {
		if job != nil {
			return &pb.CancelJobResponse{JobId: req.JobId, Status: job.State, Message: fmt.Sprintf("Already %s", job.State)}, nil
		}
		return &pb.CancelJobResponse{JobId: req.JobId, Status: "NOT_FOUND", Message: "Job not found"}, nil
	}
	// Release GPU allocation
	if job.GPUsRequired > 0 && job.WorkerNode != "" {
		s.gpuTracker.Release(job.WorkerNode, job.GPUsRequired)
	}
	if job.WorkerNode != "" {
		payload := messaging.DispatchPayload{TargetNode: job.WorkerNode, JobID: job.ID, Action: "cancel"}
		b, _ := json.Marshal(payload)
		s.pub.Send(string(b))
	}
	s.appendLog(req.JobId, "INFO", "Job cancelled by user")
	return &pb.CancelJobResponse{JobId: req.JobId, Status: "CANCELLED", Message: "Cancelled"}, nil
}

func (s *schedulerServer) GetJobStatus(ctx context.Context, req *pb.GetJobStatusRequest) (*pb.GetJobStatusResponse, error) {
	job, ok := s.queue.GetJob(req.JobId)
	if !ok {
		return &pb.GetJobStatusResponse{JobId: req.JobId, State: "NOT_FOUND"}, nil
	}
	resp := &pb.GetJobStatusResponse{
		JobId: job.ID, State: job.State, WorkerNode: job.WorkerNode,
		Command: job.Command, Output: job.Output, Error: job.Error,
		SubmitTime: job.SubmitTime.Unix(), GroupId: job.GroupID,
	}
	if !job.StartTime.IsZero() {
		resp.StartTime = job.StartTime.Unix()
	}
	if !job.EndTime.IsZero() {
		resp.EndTime = job.EndTime.Unix()
	}
	return resp, nil
}

func (s *schedulerServer) ListJobs(ctx context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	jobs := s.queue.ListJobs(strings.ToUpper(req.StateFilter))
	var infos []*pb.JobInfo
	for _, j := range jobs {
		infos = append(infos, &pb.JobInfo{
			JobId: j.ID, State: j.State, Command: j.Command, Requirement: j.Requirement,
			WorkerNode: j.WorkerNode, Priority: int32(j.Priority), User: j.User,
			SubmitTime: j.SubmitTime.Unix(), GroupId: j.GroupID,
		})
	}
	return &pb.ListJobsResponse{Jobs: infos}, nil
}

func (s *schedulerServer) StreamLogs(req *pb.LogStreamRequest, stream pb.SchedulerService_StreamLogsServer) error {
	jobID := req.JobId
	s.logMu.Lock()
	for _, entry := range s.logStore[jobID] {
		if err := stream.Send(entry); err != nil {
			s.logMu.Unlock()
			return err
		}
	}
	s.logMu.Unlock()

	ch := make(chan *pb.LogMessage, 64)
	s.subMu.Lock()
	s.logChannels[jobID] = append(s.logChannels[jobID], ch)
	s.subMu.Unlock()

	defer func() {
		s.subMu.Lock()
		channels := s.logChannels[jobID]
		for i, c := range channels {
			if c == ch {
				s.logChannels[jobID] = append(channels[:i], channels[i+1:]...)
				break
			}
		}
		s.subMu.Unlock()
		close(ch)
	}()

	for {
		select {
		case entry := <-ch:
			if err := stream.Send(entry); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return nil
		}
	}
}

func (s *schedulerServer) ReportResult(ctx context.Context, req *pb.ReportResultRequest) (*pb.ReportResultResponse, error) {
	s.queue.MarkCompleted(req.JobId, req.Success, req.Output, req.Error)

	job, ok := s.queue.GetJob(req.JobId)
	if !ok {
		return &pb.ReportResultResponse{Acknowledged: true}, nil
	}

	// Release GPU allocation
	if job.GPUsRequired > 0 && job.WorkerNode != "" {
		s.gpuTracker.Release(job.WorkerNode, job.GPUsRequired)
	}

	// Circuit breaker tracking
	if req.Success {
		s.cb.RecordSuccess(req.WorkerNode)
	} else {
		s.cb.RecordFailure(req.WorkerNode)
	}

	// Fairshare usage recording
	dur := job.EndTime.Sub(job.StartTime).Seconds()
	if dur > 0 {
		s.fairshare.RecordUsage(job.User, dur)
	}

	// Job retry logic
	if !req.Success && job.GroupID == "" && job.RetryCount < job.MaxRetries {
		job.RetryCount++
		backoff := time.Duration(job.RetryCount*job.RetryCount*10) * time.Second
		s.appendLog(req.JobId, "WARN", fmt.Sprintf("Retry %d/%d in %s", job.RetryCount, job.MaxRetries, backoff))
		fmt.Printf("Job %s failed, retrying %d/%d in %s\n", req.JobId, job.RetryCount, job.MaxRetries, backoff)
		go func() {
			time.Sleep(backoff)
			job.State = "" // Reset for re-enqueue
			job.WorkerNode = ""
			job.StartTime = time.Time{}
			job.EndTime = time.Time{}
			job.Output = ""
			job.Error = ""
			s.queue.Enqueue(job)
		}()
		return &pb.ReportResultResponse{Acknowledged: true}, nil
	}

	// Dead letter queue for exhausted retries
	if !req.Success && job.RetryCount >= job.MaxRetries && job.MaxRetries > 0 && s.store != nil {
		s.store.SaveDeadLetter(job)
		s.appendLog(req.JobId, "ERROR", fmt.Sprintf("All %d retries exhausted, moved to dead letter queue", job.MaxRetries))
	}

	// Group completion
	if job.GroupID != "" {
		s.handleGroupCompletion(job.GroupID, req.JobId, req.Success)
	}

	resultStatus := "COMPLETED"
	if !req.Success {
		resultStatus = "FAILED"
	}
	jobsCompletedTotal.WithLabelValues(job.User, resultStatus).Inc()
	if dur > 0 {
		jobDuration.WithLabelValues(job.User, resultStatus).Observe(dur)
	}

	s.appendLog(req.JobId, "INFO", fmt.Sprintf("Job %s on worker %s | Output: %s", resultStatus, req.WorkerNode, truncate(req.Output, 200)))
	if req.Error != "" {
		s.appendLog(req.JobId, "ERROR", req.Error)
	}
	fmt.Printf("Job %s result: %s (worker: %s)\n", req.JobId, resultStatus, req.WorkerNode)
	return &pb.ReportResultResponse{Acknowledged: true}, nil
}

func (s *schedulerServer) handleGroupCompletion(groupID, completedJobID string, success bool) {
	group, ok := s.queue.GetGroup(groupID)
	if !ok || group.State == "COMPLETED" || group.State == "FAILED" {
		return
	}
	if !success {
		fmt.Printf("[group] Rank %s failed in group %s — cancelling siblings\n", completedJobID, groupID)
		for _, jid := range group.JobIDs {
			if jid == completedJobID {
				continue
			}
			job, jOk := s.queue.Cancel(jid)
			if jOk && job != nil {
				if job.GPUsRequired > 0 && job.WorkerNode != "" {
					s.gpuTracker.Release(job.WorkerNode, job.GPUsRequired)
				}
				if job.WorkerNode != "" {
					payload := messaging.DispatchPayload{TargetNode: job.WorkerNode, JobID: job.ID, Action: "cancel"}
					b, _ := json.Marshal(payload)
					s.pub.Send(string(b))
				}
			}
		}
		s.queue.SetGroupState(groupID, "FAILED")
		return
	}
	allDone := true
	for _, jid := range group.JobIDs {
		j, jOk := s.queue.GetJob(jid)
		if !jOk {
			continue
		}
		if j.State == scheduler.StateQueued || j.State == scheduler.StateRunning {
			allDone = false
			break
		}
	}
	if allDone {
		s.queue.SetGroupState(groupID, "COMPLETED")
		fmt.Printf("[group] All ranks completed for group %s\n", groupID)
	}
}

// --- Scheduling ---

const gangTimeout = 5 * time.Minute

func nodeGPUCount(memberMeta []byte) int {
	var ad map[string]interface{}
	if json.Unmarshal(memberMeta, &ad) != nil {
		return 0
	}
	count, _ := ad["gpu_count"].(float64)
	return int(count)
}

func nodeMatchesGPU(memberMeta []byte, gpusRequired int) bool {
	if gpusRequired <= 0 {
		return true
	}
	return nodeGPUCount(memberMeta) >= gpusRequired
}

func dispatchLoop(srv *schedulerServer) {
	for {
		time.Sleep(1 * time.Second)

		queueDepth.Set(float64(srv.queue.QueueLen()))
		runningJobs.Set(float64(len(srv.queue.RunningJobs())))
		clusterNodes.Set(float64(len(srv.disc.Members())))
		groupsPending.Set(float64(len(srv.queue.PendingGroups())))

		// Phase 0: Gang scheduling with timeout
		for _, group := range srv.queue.PendingGroups() {
			if !group.CreatedAt.IsZero() && time.Since(group.CreatedAt) > gangTimeout {
				fmt.Printf("[gang] Group %s timed out — failing all ranks\n", group.GroupID)
				for _, jid := range group.JobIDs {
					srv.queue.Cancel(jid)
					srv.appendLog(jid, "ERROR", fmt.Sprintf("Gang group timed out waiting for %d nodes", group.NumNodes))
				}
				srv.queue.SetGroupState(group.GroupID, "FAILED")
				continue
			}
			tryDispatchGroup(srv, group)
		}

		// Phase 1: Direct match for top-priority single job
		topJob := srv.queue.Peek()
		if topJob == nil {
			continue
		}
		if topJob.GroupID != "" {
			continue
		}

		members := srv.disc.Members()
		var selectedNode string
		for _, member := range members {
			if len(member.Meta) == 0 || srv.cb.IsBlocked(member.Name) {
				continue
			}
			if !canDispatchGPUs(srv, member, topJob.GPUsRequired) {
				continue
			}
			match, evalErr := srv.eval.Match(topJob.Requirement, string(member.Meta))
			if evalErr == nil && match {
				selectedNode = member.Name
				break
			}
		}
		if selectedNode != "" {
			job := srv.queue.Dequeue()
			if job != nil {
				dispatchJob(srv, job, selectedNode)
			}
			continue
		}

		// Phase 2: Backfill
		for _, member := range members {
			if len(member.Meta) == 0 || srv.cb.IsBlocked(member.Name) {
				continue
			}
			memberMeta := string(member.Meta)
			memberName := member.Name
			backfillJob := srv.queue.Backfill(func(j *scheduler.Job) bool {
				if j.GroupID != "" {
					return false
				}
				if !canDispatchGPUs(srv, member, j.GPUsRequired) {
					return false
				}
				match, err := srv.eval.Match(j.Requirement, memberMeta)
				return err == nil && match
			})
			if backfillJob != nil {
				fmt.Printf("[backfill] Job %s backfilled onto %s\n", backfillJob.ID, memberName)
				srv.appendLog(backfillJob.ID, "INFO", fmt.Sprintf("Backfilled onto %s", memberName))
				dispatchJob(srv, backfillJob, memberName)
				break
			}
		}
	}
}

// canDispatchGPUs checks both total GPU count and available (unallocated) GPUs.
func canDispatchGPUs(srv *schedulerServer, member *memberlist.Node, gpusRequired int) bool {
	if gpusRequired <= 0 {
		return true
	}
	total := nodeGPUCount(member.Meta)
	if total < gpusRequired {
		return false
	}
	return srv.gpuTracker.Available(member.Name, total) >= gpusRequired
}

func tryDispatchGroup(srv *schedulerServer, group *scheduler.JobGroup) {
	var queuedJobs []*scheduler.Job
	for _, jid := range group.JobIDs {
		job, ok := srv.queue.GetJob(jid)
		if ok && job.State == scheduler.StateQueued {
			queuedJobs = append(queuedJobs, job)
		}
	}
	if len(queuedJobs) != group.NumNodes {
		return
	}

	members := srv.disc.Members()
	var matchedMembers []*memberlist.Node
	usedNodes := make(map[string]bool)

	for _, job := range queuedJobs {
		matched := false
		for _, member := range members {
			if usedNodes[member.Name] || len(member.Meta) == 0 || srv.cb.IsBlocked(member.Name) {
				continue
			}
			if !canDispatchGPUs(srv, member, job.GPUsRequired) {
				continue
			}
			match, err := srv.eval.Match(job.Requirement, string(member.Meta))
			if err == nil && match {
				matchedMembers = append(matchedMembers, member)
				usedNodes[member.Name] = true
				matched = true
				break
			}
		}
		if !matched {
			return
		}
	}

	rank0Addr := matchedMembers[0].Addr.String()
	fmt.Printf("[gang] Dispatching group %s: %d nodes, rank-0 at %s\n", group.GroupID, group.NumNodes, rank0Addr)

	for i, job := range queuedJobs {
		job.EnvVars["MASTER_ADDR"] = rank0Addr
		srv.queue.RemoveByID(job.ID)
		dispatchJob(srv, job, matchedMembers[i].Name)
		srv.appendLog(job.ID, "INFO", fmt.Sprintf("Gang-scheduled: rank %d → %s (master=%s)", i, matchedMembers[i].Name, rank0Addr))
	}
	srv.queue.SetGroupState(group.GroupID, "RUNNING")
}

func dispatchJob(srv *schedulerServer, job *scheduler.Job, nodeName string) {
	dispatchStart := time.Now()
	srv.queue.MarkRunning(job.ID, nodeName)

	// Track GPU allocation
	if job.GPUsRequired > 0 {
		srv.gpuTracker.Allocate(nodeName, job.GPUsRequired)
	}

	envVars := make(map[string]string)
	for k, v := range job.EnvVars {
		envVars[k] = v
	}

	if job.GPUsRequired > 0 {
		gpuEnvVar := "CUDA_VISIBLE_DEVICES"
		for _, member := range srv.disc.Members() {
			if member.Name == nodeName && len(member.Meta) > 0 {
				var ad map[string]interface{}
				if json.Unmarshal(member.Meta, &ad) == nil {
					if vendor, ok := ad["gpu_vendor"].(string); ok && vendor == "amd" {
						gpuEnvVar = "HIP_VISIBLE_DEVICES"
					}
				}
				break
			}
		}
		if _, exists := envVars[gpuEnvVar]; !exists {
			devices := make([]string, job.GPUsRequired)
			for i := range devices {
				devices[i] = strconv.Itoa(i)
			}
			envVars[gpuEnvVar] = strings.Join(devices, ",")
		}
	}

	payload := messaging.DispatchPayload{
		TargetNode: nodeName, JobID: job.ID, Command: job.Command,
		WalltimeSeconds: job.WalltimeSeconds, Action: "execute", EnvVars: envVars,
	}
	b, _ := json.Marshal(payload)
	srv.pub.Send(string(b))

	dispatchDuration.Observe(time.Since(dispatchStart).Seconds())
	srv.appendLog(job.ID, "INFO", fmt.Sprintf("Dispatched to node %s", nodeName))
	fmt.Printf("Job %s → %s\n", job.ID, nodeName)
}

func walltimeEnforcer(srv *schedulerServer) {
	for {
		time.Sleep(5 * time.Second)
		for _, job := range srv.queue.RunningJobs() {
			if job.WalltimeSeconds <= 0 {
				continue
			}
			if time.Now().After(job.StartTime.Add(time.Duration(job.WalltimeSeconds) * time.Second)) {
				fmt.Printf("[walltime] Job %s exceeded %ds — killing\n", job.ID, job.WalltimeSeconds)
				walltimeKillsTotal.Inc()
				srv.queue.Cancel(job.ID)
				if job.GPUsRequired > 0 {
					srv.gpuTracker.Release(job.WorkerNode, job.GPUsRequired)
				}
				payload := messaging.DispatchPayload{TargetNode: job.WorkerNode, JobID: job.ID, Action: "cancel"}
				b, _ := json.Marshal(payload)
				srv.pub.Send(string(b))
				srv.appendLog(job.ID, "WARN", fmt.Sprintf("Walltime exceeded (%ds)", job.WalltimeSeconds))
			}
		}
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// --- Health Endpoints ---

func startHealthAndMetrics(srv *schedulerServer, port int) {
	initMetrics()
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		members := srv.disc.Members()
		if len(members) == 0 {
			w.WriteHeader(503)
			w.Write([]byte(`{"status":"not_ready","reason":"no cluster members"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ready", "members": len(members),
			"queue_depth": srv.queue.QueueLen(), "draining": srv.draining.Load(),
		})
	})
	go func() {
		addr := fmt.Sprintf(":%d", port)
		fmt.Printf("Health + metrics at http://0.0.0.0%s (/health, /ready, /metrics)\n", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			fmt.Printf("Health/metrics server error: %v\n", err)
		}
	}()
}

// --- Startup ---

// StartMaster initializes and runs the master scheduler.
func StartMaster(cfg *config.Config) (*MasterHandle, error) {
	draining := &atomic.Bool{}

	// Open BoltDB store
	db, err := store.Open(config.StorePath())
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}

	queue := scheduler.NewGlobalScheduler()
	queue.MaxQueueSize = cfg.MaxQueueSize
	fairshare := scheduler.NewFairshareCalculator()

	// Wire persistence hooks
	queue.OnJobChange = func(job *scheduler.Job) {
		if err := db.SaveJob(job); err != nil {
			log.Printf("[persist] Failed to save job %s: %v", job.ID, err)
		}
	}
	queue.OnGroupChange = func(group *scheduler.JobGroup) {
		if err := db.SaveGroup(group); err != nil {
			log.Printf("[persist] Failed to save group %s: %v", group.GroupID, err)
		}
	}

	// Restore state from BoltDB
	if jobs, err := db.LoadJobs(); err == nil {
		restored := 0
		for _, job := range jobs {
			switch job.State {
			case scheduler.StateQueued:
				queue.Enqueue(job)
				restored++
			case scheduler.StateRunning:
				// Mark as failed — worker connections lost after restart
				job.State = scheduler.StateFailed
				job.Error = "master restarted"
				job.EndTime = time.Now()
				db.SaveJob(job)
			}
		}
		if restored > 0 {
			fmt.Printf("[restore] Restored %d queued jobs from disk\n", restored)
		}
	}
	if groups, err := db.LoadGroups(); err == nil {
		for _, g := range groups {
			if g.State == "PENDING" || g.State == "RUNNING" {
				g.State = "FAILED" // Can't resume mid-flight groups
				db.SaveGroup(g)
			}
			queue.RegisterGroup(g)
		}
	}
	if usage, err := db.LoadFairshare(); err == nil && len(usage) > 0 {
		fairshare.UserUsage = usage
		fmt.Printf("[restore] Restored fairshare data for %d users\n", len(usage))
	}

	cb := newCircuitBreaker()
	gt := newGPUTracker()

	hooks := &discovery.EventHooks{
		OnLeave: func(nodeName string) {
			jobs := queue.RunningJobsOnNode(nodeName)
			if len(jobs) == 0 {
				return
			}
			workerLostTotal.Inc()
			fmt.Printf("[heartbeat] Worker %s left — failing %d job(s)\n", nodeName, len(jobs))
			for _, job := range jobs {
				if job.GPUsRequired > 0 {
					gt.Release(nodeName, job.GPUsRequired)
				}
				queue.MarkCompleted(job.ID, false, "", "worker node lost")
				if job.GroupID != "" {
					if group, ok := queue.GetGroup(job.GroupID); ok && group.State == "RUNNING" {
						queue.SetGroupState(job.GroupID, "FAILED")
					}
				}
			}
		},
	}

	disc, err := discovery.NewNodeDiscovery("master-node", cfg.Ports.Gossip, nil, "", 0, hooks)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("discovery: %w", err)
	}

	eval, err := matchmaker.NewEvaluator()
	if err != nil {
		disc.Shutdown()
		db.Close()
		return nil, fmt.Errorf("CEL evaluator: %w", err)
	}

	pub, err := messaging.NewZMQPublisher(context.Background(), fmt.Sprintf("tcp://*:%d", cfg.Ports.ZMQ))
	if err != nil {
		disc.Shutdown()
		db.Close()
		return nil, fmt.Errorf("ZMQ: %w", err)
	}

	srv := &schedulerServer{
		disc: disc, queue: queue, eval: eval, pub: pub, fairshare: fairshare,
		store: db, cfg: cfg, draining: draining, cb: cb, gpuTracker: gt,
		logStore: make(map[string][]*pb.LogMessage), logChannels: make(map[string][]chan *pb.LogMessage),
	}

	startHealthAndMetrics(srv, cfg.Ports.Metrics)

	go dispatchLoop(srv)
	go walltimeEnforcer(srv)
	go func() {
		for {
			time.Sleep(60 * time.Second)
			fairshare.DecayUsage(0.95)
			db.SaveFairshare(fairshare.UserUsage)
		}
	}()

	// gRPC server with optional TLS
	var grpcServer *grpc.Server
	if cfg.TLS.Enabled && cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
		creds, err := credentials.NewServerTLSFromFile(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			disc.Shutdown()
			pub.Close()
			db.Close()
			return nil, fmt.Errorf("TLS: %w", err)
		}
		grpcServer = grpc.NewServer(grpc.Creds(creds))
	} else {
		grpcServer = grpc.NewServer()
	}
	pb.RegisterSchedulerServiceServer(grpcServer, srv)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Ports.GRPC))
	if err != nil {
		disc.Shutdown()
		pub.Close()
		db.Close()
		return nil, fmt.Errorf("gRPC listen: %w", err)
	}

	go func() {
		tls := ""
		if cfg.TLS.Enabled {
			tls = " [TLS]"
		}
		fmt.Printf("Master listening on :%d (gRPC%s), :%d (ZMQ), :%d (gossip), :%d (health+metrics)\n",
			cfg.Ports.GRPC, tls, cfg.Ports.ZMQ, cfg.Ports.Gossip, cfg.Ports.Metrics)
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("gRPC error: %v", err)
		}
	}()

	return &MasterHandle{
		Cancel: func() {
			grpcServer.GracefulStop()
			pub.Close()
			disc.Shutdown()
			db.Close()
		},
		Draining: draining,
		Queue:    queue,
	}, nil
}
