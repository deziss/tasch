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
}

func (m *metaDelegate) NodeMeta(limit int) []byte {
	return m.meta
}

func (m *metaDelegate) NotifyMsg(b []byte) {}
func (m *metaDelegate) GetBroadcasts(overhead, limit int) [][]byte { return nil }
func (m *metaDelegate) LocalState(join bool) []byte { return nil }
func (m *metaDelegate) MergeRemoteState(buf []byte, join bool) {}

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
func NewNodeDiscovery(nodeName string, bindPort int, meta []byte, advertiseAddr string, advertisePort int, hooks *EventHooks) (*NodeDiscovery, error) {
	config := memberlist.DefaultLocalConfig()
	config.Name = nodeName
	config.BindPort = bindPort

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
