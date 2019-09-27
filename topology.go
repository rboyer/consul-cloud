package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
)

type NetworkShape string

const (
	// NetworkShapeSplit = NetworkShape("split")
	NetworkShapeDual = NetworkShape("dual")
	NetworkShapeFlat = NetworkShape("flat")
)

func (s NetworkShape) GetNetworkName(dc string) string {
	switch s {
	case NetworkShapeDual:
		return dc
	case NetworkShapeFlat:
		return "lan"
	default:
		panic("unknown shape: " + s)
	}
}

func InferTopology(uc *userConfig) (*Topology, error) {
	t := &uc.Topology

	topology := &Topology{
		nm: make(map[string]Node),
	}

	switch uc.Topology.NetworkShape {
	case "dual":
		topology.NetworkShape = NetworkShapeDual
	case "flat", "":
		topology.NetworkShape = NetworkShapeFlat
	default:
		return nil, fmt.Errorf("unknown network_shape: %s", uc.Topology.NetworkShape)
	}

	rawTopology := uc.Topology
	nodeConfigs := rawTopology.NodeConfig

	addNode := func(node Node) {
		topology.nm[node.Name] = node
		if node.Server {
			topology.servers = append(topology.servers, node.Name)
		} else {
			topology.clients = append(topology.clients, node.Name)
		}
	}

	forDC := func(dc, baseIP, wanBaseIP string, servers, clients int) {
		for idx := 1; idx <= servers; idx++ {
			id := strconv.Itoa(idx)
			ip := baseIP + "." + strconv.Itoa(10+idx)
			wanIP := wanBaseIP + "." + strconv.Itoa(10+idx)

			node := Node{
				Datacenter: dc,
				Name:       dc + "-server" + id,
				Server:     true,
				Addresses: []Address{
					{
						Network:   topology.NetworkShape.GetNetworkName(dc),
						IPAddress: ip,
					},
				},
				Index: idx - 1,
			}

			switch topology.NetworkShape {
			case NetworkShapeDual:
				node.Addresses = append(node.Addresses, Address{
					Network:   "wan",
					IPAddress: wanIP,
				})
			case NetworkShapeFlat:
			default:
				panic("unknown shape: " + topology.NetworkShape)
			}
			addNode(node)
		}

		for idx := 1; idx <= clients; idx++ {
			id := strconv.Itoa(idx)
			ip := baseIP + "." + strconv.Itoa(20+idx)
			wanIP := wanBaseIP + "." + strconv.Itoa(20+idx)

			nodeName := dc + "-client" + id
			node := Node{
				Datacenter: dc,
				Name:       nodeName,
				Server:     false,
				Addresses: []Address{
					{
						Network:   topology.NetworkShape.GetNetworkName(dc),
						IPAddress: ip,
					},
				},
				Index: idx - 1,
			}

			nodeConfig := userConfigTopologyNodeConfig{} // yay zero value!
			if nodeConfigs != nil {
				if c, ok := nodeConfigs[nodeName]; ok {
					nodeConfig = c
				}
			}

			if nodeConfig.MeshGateway {
				node.MeshGateway = true

				switch topology.NetworkShape {
				case NetworkShapeDual:
					node.Addresses = append(node.Addresses, Address{
						Network:   "wan",
						IPAddress: wanIP,
					})
				case NetworkShapeFlat:
				default:
					panic("unknown shape: " + topology.NetworkShape)
				}
			} else {
				if nodeConfig.UseBuiltinProxy {
					node.UseBuiltinProxy = true
				}
				svc := Service{
					Port:              8080,
					UpstreamLocalPort: 9090,
					UpstreamExtraHCL:  nodeConfig.UpstreamExtraHCL,
					Meta:              nodeConfig.Meta(),
				}
				if idx%2 == 1 {
					svc.Name = "ping"
					svc.UpstreamName = "pong"
				} else {
					svc.Name = "pong"
					svc.UpstreamName = "ping"
				}

				if nodeConfig.UpstreamName != "" {
					svc.UpstreamName = nodeConfig.UpstreamName
				}
				if nodeConfig.UpstreamDatacenter != "" {
					svc.UpstreamDatacenter = nodeConfig.UpstreamDatacenter
				}

				node.Service = &svc
			}

			addNode(node)
		}
	}

	if _, ok := t.Datacenters[PrimaryDC]; !ok {
		return nil, fmt.Errorf("primary datacenter %q is missing from config", PrimaryDC)
	}

	dcPatt := regexp.MustCompile(`^dc([0-9]+)$`)

	for dc, v := range t.Datacenters {
		if v.Servers <= 0 {
			return nil, fmt.Errorf("%s: must always have at least one server", dc)
		}
		if v.Clients <= 0 {
			return nil, fmt.Errorf("%s: must always have at least one client", dc)
		}

		m := dcPatt.FindStringSubmatch(dc)
		if m == nil {
			return nil, fmt.Errorf("%s: not a valid datacenter name", dc)
		}
		i, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("%s: not a valid datacenter name", dc)
		}

		topology.dcs = append(topology.dcs, &Datacenter{
			Name:      dc,
			Primary:   dc == PrimaryDC,
			Index:     i,
			Servers:   v.Servers,
			Clients:   v.Clients,
			BaseIP:    fmt.Sprintf("10.0.%d", i),
			WANBaseIP: fmt.Sprintf("10.1.%d", i),
		})
	}
	sort.Slice(topology.dcs, func(i, j int) bool {
		return topology.dcs[i].Name < topology.dcs[j].Name
	})

	for _, dc := range topology.dcs {
		forDC(dc.Name, dc.BaseIP, dc.WANBaseIP, dc.Servers, dc.Clients)
	}

	return topology, nil
}

type Topology struct {
	servers      []string // node names
	clients      []string // node names
	nm           map[string]Node
	dcs          []*Datacenter
	NetworkShape NetworkShape
}

func (t *Topology) LeaderIP_new(datacenter string, wan bool) string {
	if wan {
		return t.WANLeaderIP(datacenter)
	} else {
		return t.LeaderIP(datacenter)
	}
}

func (t *Topology) LeaderIP(datacenter string) string {
	for _, name := range t.servers {
		n := t.Node(name)
		if n.Datacenter == datacenter {
			return n.LocalAddress()
		}
	}
	panic("no such dc")
}

func (t *Topology) WANLeaderIP(datacenter string) string {
	for _, name := range t.servers {
		n := t.Node(name)
		if n.Datacenter == datacenter {
			return n.PublicAddress()
		}
	}
	panic("no such dc")
}

func (t *Topology) Datacenters() []Datacenter {
	out := make([]Datacenter, len(t.dcs))
	for i, dc := range t.dcs {
		out[i] = *dc
	}
	return out
}

func (t *Topology) DC(name string) *Datacenter {
	for _, dc := range t.dcs {
		if dc.Name == name {
			return dc
		}
	}
	panic("no such dc")
}

func (t *Topology) ServerIPs(datacenter string) []string {
	var out []string
	for _, name := range t.servers {
		n := t.Node(name)
		if n.Datacenter == datacenter {
			out = append(out, n.LocalAddress())
		}
	}
	return out
}

func (t *Topology) all() []string {
	o := make([]string, 0, len(t.servers)+len(t.clients))
	o = append(o, t.servers...)
	o = append(o, t.clients...)
	return o
}

func (t *Topology) Node(name string) Node {
	if t.nm == nil {
		panic("node not found: " + name)
	}
	n, ok := t.nm[name]
	if !ok {
		panic("node not found: " + name)
	}
	return n
}

func (t *Topology) Walk(f func(n Node) error) error {
	for _, nodeName := range t.all() {
		node := t.Node(nodeName)
		if err := f(node); err != nil {
			return err
		}
	}
	return nil
}
func (t *Topology) WalkSilent(f func(n Node)) {
	for _, nodeName := range t.all() {
		node := t.Node(nodeName)
		f(node)
	}
}

type Datacenter struct {
	Name    string
	Primary bool

	Index   int
	Servers int
	Clients int

	BaseIP    string
	WANBaseIP string
}

type Node struct {
	Datacenter      string
	Name            string
	Server          bool
	Addresses       []Address
	Service         *Service
	MeshGateway     bool
	UseBuiltinProxy bool
	Index           int
}

func (n *Node) TokenName() string { return "agent--" + n.Name }

func (n *Node) LocalAddress() string {
	for _, a := range n.Addresses {
		switch a.Network {
		case n.Datacenter, "lan":
			return a.IPAddress
		}
	}
	panic("node has no local address")
}

func (n *Node) PublicAddress() string {
	for _, a := range n.Addresses {
		if a.Network == "wan" {
			return a.IPAddress
		}
	}
	panic("node has no public address")
}

type Address struct {
	Network   string
	IPAddress string
}

type Service struct {
	Name               string
	Port               int
	UpstreamName       string
	UpstreamDatacenter string
	UpstreamLocalPort  int
	UpstreamExtraHCL   string
	Meta               map[string]string
}
