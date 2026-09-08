package discovery

import (
	"fmt"
	"log"
	"net"

	"github.com/hashicorp/memberlist"
)

// metaDelegate implements memberlist.Delegate to attach dynamic ClassAd payload.
type metaDelegate struct {
	meta []byte

	// oversizeLogged keeps the once-per-process warning from repeating: memberlist calls
	// NodeMeta on every gossip round.
	oversizeLogged bool
}

// NodeMeta returns the node's gossip metadata, never exceeding limit.
//
// memberlist panics — it does not return an error — when a delegate hands back metadata
// longer than MetaMaxSize, so honoring limit here is load-bearing. Callers are expected to
// have sized the payload already (see profiler.MaxMetaBytes); this is the backstop that
// turns a would-be panic into a degraded node. Dropping the metadata entirely is deliberate:
// truncating JSON mid-document would produce a payload that parses to an all-zero ClassAd,
// which reads as a node with no resources and silently matches jobs it cannot run.
func (m *metaDelegate) NodeMeta(limit int) []byte {
	if len(m.meta) <= limit {
		return m.meta
	}
	if !m.oversizeLogged {
		m.oversizeLogged = true
		log.Printf("class ad is %d bytes, over the %d byte gossip limit — advertising no metadata; "+
			"this node will not match any job", len(m.meta), limit)
	}
	return nil
}

func (m *metaDelegate) NotifyMsg(b []byte)                         {}
func (m *metaDelegate) GetBroadcasts(overhead, limit int) [][]byte { return nil }
func (m *metaDelegate) LocalState(join bool) []byte                { return nil }
func (m *metaDelegate) MergeRemoteState(buf []byte, join bool)     {}

// eventDelegate implements memberlist.EventDelegate to detect node join/leave.
type eventDelegate struct {
	onJoin  func(name string)
	onLeave func(name string)
}

func (e *eventDelegate) NotifyJoin(node *memberlist.Node) {
	if e.onJoin != nil {
		e.onJoin(node.Name)
	}
}

func (e *eventDelegate) NotifyLeave(node *memberlist.Node) {
	if e.onLeave != nil {
		e.onLeave(node.Name)
	}
}

func (e *eventDelegate) NotifyUpdate(node *memberlist.Node) {}

// NodeDiscovery manages the SWIM-gossip protocol for automatic worker discovery
type NodeDiscovery struct {
	list *memberlist.Memberlist
}

// EventHooks holds optional callbacks for cluster membership events.
type EventHooks struct {
	OnJoin  func(name string)
	OnLeave func(name string)
}

// NewNodeDiscovery initializes a new memberlist agent on the local node.
// advertiseAddr/advertisePort allow remote workers to advertise their real IP
// to the cluster instead of 127.0.0.1. Pass empty string / 0 to skip.
// hooks is optional — pass nil if no event callbacks are needed.
// Options configures gossip beyond the basics.
type Options struct {
	// EncryptionKey enables authenticated encryption of gossip traffic when set (16, 24, or 32
	// bytes). Without it, membership is open: any host can join, declare its own name, and
	// advertise fabricated resources to attract every job in the cluster. It also lets anyone
	// on the path forge failure messages that evict healthy nodes.
	EncryptionKey []byte

	// Profile selects memberlist timing: "lan" (default), "wan", or "local".
	Profile string
}

// profileConfig returns memberlist timings for a named profile.
//
// The default used to be DefaultLocalConfig, which is tuned for loopback: sub-second probe
// timeouts produce false failure detections on any real network, and each one marks every job
// on the wrongly-declared-dead node as failed.
func profileConfig(profile string) *memberlist.Config {
	switch profile {
	case "wan":
		return memberlist.DefaultWANConfig()
	case "local":
		return memberlist.DefaultLocalConfig()
	default:
		return memberlist.DefaultLANConfig()
	}
}

func NewNodeDiscovery(nodeName string, bindPort int, meta []byte, advertiseAddr string, advertisePort int, hooks *EventHooks, opts *Options) (*NodeDiscovery, error) {
	if opts == nil {
		opts = &Options{}
	}
	config := profileConfig(opts.Profile)
	config.Name = nodeName
	config.BindPort = bindPort

	if len(opts.EncryptionKey) > 0 {
		config.SecretKey = opts.EncryptionKey
	} else {
		log.Printf("WARNING: gossip is unencrypted and unauthenticated on port %d — any host "+
			"that can reach it can join the cluster. Set gossip.encryption_key in the config.", bindPort)
	}

	if advertiseAddr != "" {
		config.AdvertiseAddr = advertiseAddr
		if advertisePort > 0 {
			config.AdvertisePort = advertisePort
		} else {
			config.AdvertisePort = bindPort
		}
	}

	if meta != nil {
		config.Delegate = &metaDelegate{meta: meta}
	}

	if hooks != nil {
		config.Events = &eventDelegate{onJoin: hooks.OnJoin, onLeave: hooks.OnLeave}
	}

	list, err := memberlist.Create(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create memberlist: %w", err)
	}

	return &NodeDiscovery{list: list}, nil
}

// Join connects to an existing cluster by contacting a known seed node (e.g., master)
func (d *NodeDiscovery) Join(existingNodes []string) error {
	numJoined, err := d.list.Join(existingNodes)
	if err != nil {
		return err
	}
	log.Printf("Successfully joined cluster with %d nodes", numJoined)
	return nil
}

// Members returns a list of all currently known active nodes in the cluster
func (d *NodeDiscovery) Members() []*memberlist.Node {
	return d.list.Members()
}

// Shutdown gracefully leaves the cluster
func (d *NodeDiscovery) Shutdown() error {
	return d.list.Shutdown()
}

// GetLocalIP returns the first non-loopback IPv4 address of this machine.
// Used by workers to determine their advertise address for remote clusters.
func GetLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
			return ipNet.IP.String()
		}
	}
	return ""
}
