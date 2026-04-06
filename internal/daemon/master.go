package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/pkg/discovery"
	"github.com/deziss/tasch/pkg/matchmaker"
	"github.com/deziss/tasch/pkg/messaging"
	"github.com/deziss/tasch/pkg/scheduler"
	"github.com/google/uuid"
	"github.com/hashicorp/memberlist"
	"google.golang.org/grpc"
)

type schedulerServer struct {
	pb.UnimplementedSchedulerServiceServer
	disc      *discovery.NodeDiscovery
	queue     *scheduler.GlobalScheduler
	eval      *matchmaker.Evaluator
	pub       *messaging.ZMQPublisher
	fairshare *scheduler.FairshareCalculator

	logMu       sync.Mutex
	logStore    map[string][]*pb.LogMessage
	subMu       sync.Mutex
	logChannels map[string][]chan *pb.LogMessage
}

func (s *schedulerServer) appendLog(jobID, level, msg string) {
	entry := &pb.LogMessage{
		Timestamp: time.Now().UnixMilli(),
		Level:     level,
		Message:   msg,
		JobId:     jobID,
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
		ID:              jobID,
		Requirement:     req.CelRequirement,
		Command:         req.Command,
		SubmitTime:      time.Now(),
		Priority:        effectivePriority,
		User:            user,
		WalltimeSeconds: int(req.WalltimeSeconds),
		GPUsRequired:    int(req.GpusRequired),
		EnvVars:         req.EnvVars,
	}
	s.queue.Enqueue(job)
	jobsSubmittedTotal.WithLabelValues(user).Inc()
	s.appendLog(jobID, "INFO", fmt.Sprintf("Job queued | Priority: %d (base: %d, penalty: +%d) | GPUs: %d | Expr: '%s'", effectivePriority, priority, penalty, req.GpusRequired, req.CelRequirement))
	fmt.Printf("Job %s queued | User: %s, Priority: %d, GPUs: %d\n", jobID, user, effectivePriority, req.GpusRequired)
	return &pb.SubmitJobResponse{JobId: jobID, Status: "QUEUED"}, nil
}

func (s *schedulerServer) SubmitDistributedJob(ctx context.Context, req *pb.SubmitDistributedJobRequest) (*pb.SubmitDistributedJobResponse, error) {
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
			GPUsRequired: gpusPerNode, EnvVars: envVars,
		}
		s.queue.Enqueue(job)
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
			return &pb.CancelJobResponse{JobId: req.JobId, Status: job.State, Message: fmt.Sprintf("Job already in terminal state: %s", job.State)}, nil
		}
		return &pb.CancelJobResponse{JobId: req.JobId, Status: "NOT_FOUND", Message: "Job not found"}, nil
	}
	if job.WorkerNode != "" {
		payload := messaging.DispatchPayload{TargetNode: job.WorkerNode, JobID: job.ID, Action: "cancel"}
		b, _ := json.Marshal(payload)
		s.pub.Send(string(b))
	}
	s.appendLog(req.JobId, "INFO", "Job cancelled by user")
	fmt.Printf("Job %s cancelled\n", req.JobId)
	return &pb.CancelJobResponse{JobId: req.JobId, Status: "CANCELLED", Message: "Job cancelled successfully"}, nil
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
	if job, ok := s.queue.GetJob(req.JobId); ok {
		dur := job.EndTime.Sub(job.StartTime).Seconds()
		if dur > 0 {
			s.fairshare.RecordUsage(job.User, dur)
		}
		if job.GroupID != "" {
			s.handleGroupCompletion(job.GroupID, req.JobId, req.Success)
		}
	}
	status := "COMPLETED"
	if !req.Success {
		status = "FAILED"
	}
	if job, ok := s.queue.GetJob(req.JobId); ok {
		jobsCompletedTotal.WithLabelValues(job.User, status).Inc()
		dur := job.EndTime.Sub(job.StartTime).Seconds()
		if dur > 0 {
			jobDuration.WithLabelValues(job.User, status).Observe(dur)
		}
	}
	s.appendLog(req.JobId, "INFO", fmt.Sprintf("Job %s on worker %s | Output: %s", status, req.WorkerNode, truncate(req.Output, 200)))
	if req.Error != "" {
		s.appendLog(req.JobId, "ERROR", req.Error)
	}
	fmt.Printf("Job %s result: %s (worker: %s)\n", req.JobId, status, req.WorkerNode)
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
			if jOk && job != nil && job.WorkerNode != "" {
				payload := messaging.DispatchPayload{TargetNode: job.WorkerNode, JobID: job.ID, Action: "cancel"}
				b, _ := json.Marshal(payload)
				s.pub.Send(string(b))
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

func nodeMatchesGPU(memberMeta []byte, gpusRequired int) bool {
	if gpusRequired <= 0 {
		return true
	}
	var ad map[string]interface{}
	if err := json.Unmarshal(memberMeta, &ad); err != nil {
		return false
	}
	gpuCount, _ := ad["gpu_count"].(float64)
	return int(gpuCount) >= gpusRequired
}

func dispatchLoop(srv *schedulerServer) {
	for {
		time.Sleep(1 * time.Second)

		// Update gauges each cycle
		queueDepth.Set(float64(srv.queue.QueueLen()))
		runningJobs.Set(float64(len(srv.queue.RunningJobs())))
		clusterNodes.Set(float64(len(srv.disc.Members())))
		groupsPending.Set(float64(len(srv.queue.PendingGroups())))

		for _, group := range srv.queue.PendingGroups() {
			if !group.CreatedAt.IsZero() && time.Since(group.CreatedAt) > gangTimeout {
				fmt.Printf("[gang] Group %s timed out after %s — failing all ranks\n", group.GroupID, gangTimeout)
				for _, jid := range group.JobIDs {
					job, ok := srv.queue.Cancel(jid)
					if ok && job != nil && job.WorkerNode != "" {
						payload := messaging.DispatchPayload{TargetNode: job.WorkerNode, JobID: job.ID, Action: "cancel"}
						b, _ := json.Marshal(payload)
						srv.pub.Send(string(b))
					}
					srv.appendLog(jid, "ERROR", fmt.Sprintf("Gang group %s timed out waiting for %d matching nodes", group.GroupID, group.NumNodes))
				}
				srv.queue.SetGroupState(group.GroupID, "FAILED")
				continue
			}
			tryDispatchGroup(srv, group)
		}

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
			if len(member.Meta) == 0 {
				continue
			}
			if !nodeMatchesGPU(member.Meta, topJob.GPUsRequired) {
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

		for _, member := range members {
			if len(member.Meta) == 0 {
				continue
			}
			memberMeta := string(member.Meta)
			memberName := member.Name
			backfillJob := srv.queue.Backfill(func(j *scheduler.Job) bool {
				if j.GroupID != "" {
					return false
				}
				if !nodeMatchesGPU([]byte(memberMeta), j.GPUsRequired) {
					return false
				}
				match, err := srv.eval.Match(j.Requirement, memberMeta)
				return err == nil && match
			})
			if backfillJob != nil {
				fmt.Printf("[backfill] Job %s (priority %d) backfilled onto node %s\n", backfillJob.ID, backfillJob.Priority, memberName)
				srv.appendLog(backfillJob.ID, "INFO", fmt.Sprintf("Backfilled onto node %s", memberName))
				dispatchJob(srv, backfillJob, memberName)
				break
			}
		}
	}
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
			if usedNodes[member.Name] || len(member.Meta) == 0 {
				continue
			}
			if !nodeMatchesGPU(member.Meta, job.GPUsRequired) {
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
	fmt.Printf("[gang] Dispatching group %s: %d nodes, rank-0 master at %s\n", group.GroupID, group.NumNodes, rank0Addr)

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

	envVars := make(map[string]string)
	for k, v := range job.EnvVars {
		envVars[k] = v
	}

	// Auto-set GPU device visibility based on vendor
	if job.GPUsRequired > 0 {
		// Check the target worker's ClassAd for GPU vendor
		gpuEnvVar := "CUDA_VISIBLE_DEVICES" // default to NVIDIA
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
			for i := 0; i < job.GPUsRequired; i++ {
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
	fmt.Printf("Job %s matched with node %s. Dispatching...\n", job.ID, nodeName)
}

func walltimeEnforcer(srv *schedulerServer) {
	for {
		time.Sleep(5 * time.Second)
		for _, job := range srv.queue.RunningJobs() {
			if job.WalltimeSeconds <= 0 {
				continue
			}
			if time.Now().After(job.StartTime.Add(time.Duration(job.WalltimeSeconds) * time.Second)) {
				fmt.Printf("[walltime] Job %s exceeded %ds limit. Cancelling...\n", job.ID, job.WalltimeSeconds)
				walltimeKillsTotal.Inc()
				srv.queue.Cancel(job.ID)
				payload := messaging.DispatchPayload{TargetNode: job.WorkerNode, JobID: job.ID, Action: "cancel"}
				b, _ := json.Marshal(payload)
				srv.pub.Send(string(b))
				srv.appendLog(job.ID, "WARN", fmt.Sprintf("Job killed: exceeded walltime of %ds", job.WalltimeSeconds))
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

// StartMaster initializes and runs the master scheduler.
// It blocks until the returned cancel function is called.
func StartMaster(cfg *config.Config) (cancel func(), err error) {
	queue := scheduler.NewGlobalScheduler()
	fairshare := scheduler.NewFairshareCalculator()

	hooks := &discovery.EventHooks{
		OnLeave: func(nodeName string) {
			jobs := queue.RunningJobsOnNode(nodeName)
			if len(jobs) == 0 {
				return
			}
			workerLostTotal.Inc()
			fmt.Printf("[heartbeat] Worker %s left cluster — failing %d running job(s)\n", nodeName, len(jobs))
			for _, job := range jobs {
				queue.MarkCompleted(job.ID, false, "", "worker node lost")
				if job.GroupID != "" {
					if group, ok := queue.GetGroup(job.GroupID); ok && group.State == "RUNNING" {
						queue.SetGroupState(job.GroupID, "FAILED")
						fmt.Printf("[heartbeat] Group %s failed due to worker %s loss\n", job.GroupID, nodeName)
					}
				}
			}
		},
	}

	disc, err := discovery.NewNodeDiscovery("master-node", cfg.Ports.Gossip, nil, "", 0, hooks)
	if err != nil {
		return nil, fmt.Errorf("discovery init: %w", err)
	}

	eval, err := matchmaker.NewEvaluator()
	if err != nil {
		disc.Shutdown()
		return nil, fmt.Errorf("CEL evaluator: %w", err)
	}

	pubEndpoint := fmt.Sprintf("tcp://*:%d", cfg.Ports.ZMQ)
	pub, err := messaging.NewZMQPublisher(context.Background(), pubEndpoint)
	if err != nil {
		disc.Shutdown()
		return nil, fmt.Errorf("ZMQ publisher: %w", err)
	}

	srv := &schedulerServer{
		disc: disc, queue: queue, eval: eval, pub: pub, fairshare: fairshare,
		logStore:    make(map[string][]*pb.LogMessage),
		logChannels: make(map[string][]chan *pb.LogMessage),
	}

	initMetrics()
	startMetricsServer(cfg.Ports.Metrics)

	go dispatchLoop(srv)
	go walltimeEnforcer(srv)
	go func() {
		for {
			time.Sleep(60 * time.Second)
			fairshare.DecayUsage(0.95)
		}
	}()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Ports.GRPC))
	if err != nil {
		disc.Shutdown()
		pub.Close()
		return nil, fmt.Errorf("gRPC listen: %w", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterSchedulerServiceServer(grpcServer, srv)

	go func() {
		fmt.Printf("Master scheduler listening on :%d (gRPC), :%d (ZMQ), :%d (gossip), :%d (metrics)\n", cfg.Ports.GRPC, cfg.Ports.ZMQ, cfg.Ports.Gossip, cfg.Ports.Metrics)
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("gRPC server error: %v", err)
		}
	}()

	return func() {
		grpcServer.GracefulStop()
		pub.Close()
		disc.Shutdown()
	}, nil
}
