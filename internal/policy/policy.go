// Package policy turns the partition and account configuration into admission decisions.
//
// It answers three questions the scheduler cannot answer from a job alone:
//
//	Which partition does this job belong to, and is it allowed there?
//	Which account does it count against?
//	Would starting it now exceed a limit that account, or an account above it, is under?
//
// The rules live here rather than in the master because they are pure: given a job, a set of
// running jobs, and the configuration, the answer is the same on every replica and in a test.
// The master supplies the running jobs; nothing here reaches for live cluster state.
package policy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/pkg/matchmaker"
	"github.com/deziss/tasch/pkg/scheduler"
)

// Policy holds the resolved partitions and account tree.
type Policy struct {
	partitions       []config.PartitionConfig
	partitionByName  map[string]*config.PartitionConfig
	defaultPartition string

	accounts    map[string]*config.AccountConfig
	accountsFor map[string][]string // user -> accounts, in configuration order
	eval        *matchmaker.Evaluator
}

// New builds a Policy, validating the parts of the configuration that need a CEL compiler.
//
// Node selectors are compiled here rather than at config load because an invalid one has the
// same failure mode an invalid job requirement used to have: it matches nothing, silently, on
// every scheduling cycle forever. Rejecting it at startup is the only way an operator finds out.
func New(cfg *config.Config, eval *matchmaker.Evaluator) (*Policy, error) {
	p := &Policy{
		partitionByName: make(map[string]*config.PartitionConfig, len(cfg.Partitions)),
		accounts:        make(map[string]*config.AccountConfig, len(cfg.Accounts)),
		accountsFor:     make(map[string][]string),
		eval:            eval,
	}

	p.partitions = make([]config.PartitionConfig, len(cfg.Partitions))
	copy(p.partitions, cfg.Partitions)
	for i := range p.partitions {
		part := &p.partitions[i]
		if part.NodeSelector != "" && eval != nil {
			if err := eval.Validate(part.NodeSelector); err != nil {
				return nil, fmt.Errorf("partition %q has an invalid node_selector: %w", part.Name, err)
			}
		}
		p.partitionByName[part.Name] = part
		if part.Default {
			p.defaultPartition = part.Name
		}
	}

	for i := range cfg.Accounts {
		acct := cfg.Accounts[i]
		p.accounts[acct.Name] = &acct
		for _, user := range acct.Users {
			p.accountsFor[user] = append(p.accountsFor[user], acct.Name)
		}
	}
	return p, nil
}

// Enabled reports whether any partition or account is configured. When nothing is, the policy
// is inert and the scheduler behaves exactly as it did before either existed.
func (p *Policy) Enabled() bool {
	return p != nil && (len(p.partitions) > 0 || len(p.accounts) > 0)
}

// AccessDenied marks a refusal that is about permission rather than a malformed request. The
// distinction reaches the caller as PermissionDenied rather than InvalidArgument, which is the
// difference between "you cannot use that" and "there is no such thing".
type AccessDenied struct{ Msg string }

func (e AccessDenied) Error() string { return e.Msg }

func denied(format string, args ...any) error {
	return AccessDenied{Msg: fmt.Sprintf(format, args...)}
}

// Usage is what a set of jobs is holding, or what one job would take.
type Usage struct {
	Jobs     int
	GPUs     int
	CPUs     int
	MemoryMB int
}

func (u Usage) plus(o Usage) Usage {
	return Usage{u.Jobs + o.Jobs, u.GPUs + o.GPUs, u.CPUs + o.CPUs, u.MemoryMB + o.MemoryMB}
}

// UsageOf is what one job occupies.
func UsageOf(job *scheduler.Job) Usage {
	return Usage{Jobs: 1, GPUs: job.GPUsRequired, CPUs: job.CPUsRequired, MemoryMB: job.MemoryRequiredMB}
}

// AccountUsage totals what each account holds, including everything in the accounts beneath it.
//
// The rollup is the whole point of nesting: a job in a team counts against the department too,
// so a department's quota cannot be exceeded by splitting work across its teams.
func (p *Policy) AccountUsage(jobs []*scheduler.Job) map[string]Usage {
	totals := make(map[string]Usage)
	for _, job := range jobs {
		use := UsageOf(job)
		for _, name := range p.ancestry(job.Account) {
			totals[name] = totals[name].plus(use)
		}
	}
	return totals
}

// PartitionUsage counts jobs per partition.
func PartitionUsage(jobs []*scheduler.Job) map[string]int {
	counts := make(map[string]int)
	for _, job := range jobs {
		if job.Partition != "" {
			counts[job.Partition]++
		}
	}
	return counts
}

// ancestry returns the account and every account above it, nearest first.
func (p *Policy) ancestry(account string) []string {
	if account == "" {
		return nil
	}
	var chain []string
	seen := make(map[string]bool)
	for name := account; name != "" && !seen[name]; {
		seen[name] = true
		chain = append(chain, name)
		acct, ok := p.accounts[name]
		if !ok {
			break
		}
		name = acct.Parent
	}
	return chain
}

// ResolveAccount picks the account a job counts against.
//
// A requested account must be one the user actually belongs to. Without that check, quotas are
// advisory: anyone short of their own budget could spend someone else's by naming it.
func (p *Policy) ResolveAccount(user, requested string) (string, error) {
	if len(p.accounts) == 0 {
		return "", nil
	}
	memberships := p.accountsFor[user]

	if requested == "" {
		if len(memberships) == 0 {
			return "", nil
		}
		return memberships[0], nil
	}

	if _, known := p.accounts[requested]; !known {
		return "", fmt.Errorf("account %q does not exist", requested)
	}
	for _, name := range memberships {
		if name == requested {
			return requested, nil
		}
	}
	if len(memberships) == 0 {
		return "", denied("user %q belongs to no account and cannot submit to %q", user, requested)
	}
	return "", denied("user %q is not a member of account %q (member of: %s)",
		user, requested, strings.Join(memberships, ", "))
}

// ResolvePartition picks the partition a job runs in and checks the submitter may use it.
func (p *Policy) ResolvePartition(requested, user, account string) (*config.PartitionConfig, error) {
	if len(p.partitions) == 0 {
		if requested != "" {
			return nil, fmt.Errorf("no partitions are configured, so %q cannot be selected", requested)
		}
		return nil, nil
	}

	name := requested
	if name == "" {
		name = p.defaultPartition
		if name == "" {
			// Partitions exist but none is default: an unnamed job stays unrestricted rather
			// than being pushed into whichever happens to be listed first.
			return nil, nil
		}
	}

	part, ok := p.partitionByName[name]
	if !ok {
		return nil, fmt.Errorf("partition %q does not exist (available: %s)", name, p.partitionNames())
	}
	if err := p.checkPartitionAccess(part, user, account); err != nil {
		return nil, err
	}
	return part, nil
}

func (p *Policy) checkPartitionAccess(part *config.PartitionConfig, user, account string) error {
	if len(part.AllowedUsers) > 0 && !contains(part.AllowedUsers, user) {
		return denied("user %q is not allowed to submit to partition %q", user, part.Name)
	}
	if len(part.AllowedAccounts) > 0 {
		// An account inherits access from its ancestors: granting a department a partition
		// should not require re-listing every team inside it.
		for _, name := range p.ancestry(account) {
			if contains(part.AllowedAccounts, name) {
				return nil
			}
		}
		return denied("account %q is not allowed to submit to partition %q (allowed: %s)",
			account, part.Name, strings.Join(part.AllowedAccounts, ", "))
	}
	return nil
}

func (p *Policy) partitionNames() string {
	names := make([]string, 0, len(p.partitions))
	for _, part := range p.partitions {
		names = append(names, part.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// ApplyPartitionDefaults returns the walltime and priority a job takes on entering a partition.
func ApplyPartitionDefaults(part *config.PartitionConfig, walltime, priority int) (int, int, error) {
	if part == nil {
		return walltime, priority, nil
	}
	if walltime == 0 && part.DefaultWalltimeSeconds > 0 {
		walltime = part.DefaultWalltimeSeconds
	}
	if part.MaxWalltimeSeconds > 0 {
		if walltime == 0 {
			// A ceiling with no default would otherwise admit jobs that never end, which is
			// rarely what the ceiling was for.
			walltime = part.MaxWalltimeSeconds
		} else if walltime > part.MaxWalltimeSeconds {
			return 0, 0, fmt.Errorf("partition %q allows at most %ds of walltime, and this job asks for %ds",
				part.Name, part.MaxWalltimeSeconds, walltime)
		}
	}
	return walltime, priority + part.PriorityBoost, nil
}

// AdmitQueued reports whether an account may add another queued job.
//
// Checked at submit so a runaway loop is refused at the door, rather than after it has filled
// the queue for everybody else.
func (p *Policy) AdmitQueued(account string, queuedByAccount map[string]Usage) error {
	for _, name := range p.ancestry(account) {
		acct := p.accounts[name]
		if acct == nil || acct.MaxQueuedJobs == 0 {
			continue
		}
		if queuedByAccount[name].Jobs >= acct.MaxQueuedJobs {
			return fmt.Errorf("account %q already has %d queued jobs, its limit",
				name, acct.MaxQueuedJobs)
		}
	}
	return nil
}

// AdmitDispatch reports whether a job may start now, given what is already running.
//
// It returns the reason rather than a bare false: "over quota" is only actionable if it says
// which account and which limit.
func (p *Policy) AdmitDispatch(job *scheduler.Job, running map[string]Usage,
	partitionRunning map[string]int) (bool, string) {

	if job.Partition != "" {
		if part, ok := p.partitionByName[job.Partition]; ok && part.MaxRunningJobs > 0 {
			if partitionRunning[job.Partition] >= part.MaxRunningJobs {
				return false, fmt.Sprintf("partition %s is at its limit of %d running jobs",
					job.Partition, part.MaxRunningJobs)
			}
		}
	}

	need := UsageOf(job)
	for _, name := range p.ancestry(job.Account) {
		acct := p.accounts[name]
		if acct == nil {
			continue
		}
		have := running[name]
		for _, limit := range []struct {
			name  string
			have  int
			need  int
			limit int
		}{
			{"running jobs", have.Jobs, need.Jobs, acct.MaxRunningJobs},
			{"GPUs", have.GPUs, need.GPUs, acct.MaxGPUs},
			{"CPUs", have.CPUs, need.CPUs, acct.MaxCPUs},
			{"MB of memory", have.MemoryMB, need.MemoryMB, acct.MaxMemoryMB},
		} {
			if limit.limit > 0 && limit.have+limit.need > limit.limit {
				return false, fmt.Sprintf("account %s would exceed its quota of %d %s (%d in use, %d needed)",
					name, limit.limit, limit.name, limit.have, limit.need)
			}
		}
	}
	return true, ""
}

// MatchesPartition reports whether a node belongs to the job's partition.
func (p *Policy) MatchesPartition(partitionName, nodeAdJSON string) bool {
	if partitionName == "" {
		return true
	}
	part, ok := p.partitionByName[partitionName]
	if !ok {
		// The partition was removed from the configuration while a job that named it was still
		// queued. Refusing every node is right: silently widening it to the whole cluster would
		// place the job exactly where the operator had decided it should not go.
		return false
	}
	if part.NodeSelector == "" || p.eval == nil {
		return true
	}
	match, err := p.eval.Match(part.NodeSelector, nodeAdJSON)
	return err == nil && match
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// PartitionByName returns a partition's configuration.
func (p *Policy) PartitionByName(name string) (*config.PartitionConfig, bool) {
	part, ok := p.partitionByName[name]
	return part, ok
}
