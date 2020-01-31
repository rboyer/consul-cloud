package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/rboyer/safeio"
)

type CommandGenerate struct {
	*Core
}

func (c *CommandGenerate) Run() error {
	var verbose bool

	flag.BoolVar(&verbose, "v", false, "verbose")
	flag.Parse()

	if verbose {
		c.topology.WalkSilent(func(node *Node) {
			c.logger.Info("Generating node",
				"name", node.Name,
				"server", node.Server,
				"dc", node.Datacenter,
				"ip", node.LocalAddress(),
			)
		})
	}

	if err := c.generateComposeFile(); err != nil {
		return err
	}

	if c.config.PrometheusEnabled {
		if err := c.generatePrometheusConfigFile(); err != nil {
			return err
		}
		if err := c.generateGrafanaConfigFiles(); err != nil {
			return err
		}
	}

	return nil
}

func (c *CommandGenerate) generateComposeFile() error {
	info := composeInfo{
		Config:   c.config,
		Networks: c.topology.Networks(),
	}

	if c.config.PrometheusEnabled {
		info.Volumes = append(info.Volumes, "prometheus-data")
		info.Volumes = append(info.Volumes, "grafana-data")
	}

	err := c.topology.Walk(func(node *Node) error {
		podName := node.Name + "-pod"

		podHCL, err := c.generateAgentHCL(node)
		if err != nil {
			return err
		}

		extraYAML_1, err := c.generateMeshGatewayYAML(podName, node)
		if err != nil {
			return err
		}

		extraYAML_2, err := c.generatePingPongYAML(podName, node)
		if err != nil {
			return err
		}

		extraYAML := extraYAML_1 + "\n\n" + extraYAML_2

		pod := composePod{
			PodName:        podName,
			ConsulImage:    c.config.ConsulImage,
			Node:           node,
			HCL:            indent(podHCL, 8),
			AgentDependsOn: []string{podName},
			ExtraYAML:      extraYAML,
			Labels:         map[string]string{
				//
			},
		}
		node.AddLabels(pod.Labels)

		if !node.Server {
			pod.AgentDependsOn = append(pod.AgentDependsOn,
				node.Datacenter+"-server1",
			)
		}

		info.Volumes = append(info.Volumes, node.Name)
		info.Pods = append(info.Pods, pod)
		return nil
	})
	if err != nil {
		return err
	}

	var out bytes.Buffer
	if err := dockerComposeT.Execute(&out, &info); err != nil {
		return err
	}

	return c.updateFileIfDifferent(out.Bytes(), "docker-compose.yml", 0644)
}

type composeInfo struct {
	Config *FlatConfig

	Volumes  []string
	Pods     []composePod
	Networks []*Network
}

type composePod struct {
	PodName        string
	ConsulImage    string
	Node           *Node
	HCL            string
	AgentDependsOn []string
	ExtraYAML      string
	Labels         map[string]string
}

var dockerComposeT = template.Must(template.New("docker").Parse(`version: '3.7'

# consul:
#   client_addr is set to 0.0.0.0 to make control from the host easier
#   it should be disabled for real topologies

# envoy:
#   admin-bind is set to 0.0.0.0 to make control from the host easier
#   it should be disabled for real topologies

networks:
{{- range .Networks }}
  consul-{{ .Name }}:
    ipam:
      driver: default
      config:
        - subnet: '{{ .CIDR }}'
{{- end }}

volumes:
{{- range .Volumes }}
  {{.}}:
{{- end }}

# https://yipee.io/2017/06/getting-kubernetes-pod-features-using-native-docker-commands/
services:
{{- if .Config.PrometheusEnabled }}
  prometheus:
    image: prom/prometheus:latest
    labels:
      devconsul.type: "infra"
    restart: always
    dns: 8.8.8.8
    network_mode: host
    volumes:
      - 'prometheus-data:/prometheus-data'
      - './cache/prometheus.yml:/etc/prometheus/prometheus.yml:ro'

  grafana:
    network_mode: 'service:prometheus'
    image: grafana/grafana:latest
    labels:
      devconsul.type: "infra"
    restart: always
    init: true
    volumes:
      - 'grafana-data:/var/lib/grafana'
      - './cache/grafana-prometheus.yml:/etc/grafana/provisioning/datasources/prometheus.yml:ro'
      - './cache/grafana.ini:/etc/grafana/grafana.ini:ro'
{{- end }}

{{- range .Pods }}
  {{.PodName}}:
    container_name: '{{.PodName}}'
    hostname: '{{.PodName}}'
    image: gcr.io/google_containers/pause:1.0
    labels:
      devconsul.type: "pod"
{{- range $k, $v := .Labels }}
      {{ $k }}: "{{ $v }}"
{{- end }}
    restart: always
    dns: 8.8.8.8
    networks:
{{- range .Node.Addresses }}
      consul-{{.Network}}:
        ipv4_address: '{{.IPAddress}}'
{{- end }}

  {{.Node.Name}}:
    network_mode: 'service:{{.PodName}}'
    depends_on:
{{- range .AgentDependsOn }}
      - '{{.}}'
{{- end}}
    volumes:
      - '{{.Node.Name}}:/consul/data'
      - './cache/tls:/tls:ro'
    image: '{{.ConsulImage}}'
    labels:
      devconsul.type: "consul"
{{- range $k, $v := .Labels }}
      {{ $k }}: "{{ $v }}"
{{- end }}
    command:
      - 'agent'
      - '-hcl'
      - |
{{ .HCL }}
{{ .ExtraYAML }}
{{- end}}
`))

func (c *CommandGenerate) generatePingPongYAML(podName string, node *Node) (string, error) {
	var extraYAML bytes.Buffer
	if node.Service != nil {
		svc := node.Service

		switch svc.Name {
		case "ping", "pong":
		default:
			return "", errors.New("unexpected service: " + svc.Name)
		}

		ppi := pingpongInfo{
			PodName:         podName,
			NodeName:        node.Name,
			PingPong:        svc.Name,
			UseBuiltinProxy: node.UseBuiltinProxy,
			EnvoyLogLevel:   c.config.EnvoyLogLevel,
		}
		if len(svc.Meta) > 0 {
			ppi.MetaString = fmt.Sprintf("--%q", svc.Meta)
		}

		proxyType := "envoy"
		if node.UseBuiltinProxy {
			proxyType = "builtin"
		}

		if c.config.KubernetesEnabled {
			ppi.SidecarBootArgs = []string{
				"/secrets/ready.val",
				proxyType,
				"login",
				"-t",
				"/secrets/k8s/service_jwt_token." + svc.Name,
				"-s",
				"/tmp/consul.token",
				"-r",
				"/secrets/servicereg__" + node.Name + "__" + svc.Name + ".hcl",
			}
		} else {
			ppi.SidecarBootArgs = []string{
				"/secrets/ready.val",
				proxyType,
				"direct",
				"-t",
				"/secrets/service-token--" + svc.Name + ".val",
				"-r",
				"/secrets/servicereg__" + node.Name + "__" + svc.Name + ".hcl",
			}
		}

		if err := pingpongT.Execute(&extraYAML, &ppi); err != nil {
			return "", err
		}
	}

	return extraYAML.String(), nil
}

type pingpongInfo struct {
	PodName         string
	NodeName        string
	PingPong        string // ping or pong
	MetaString      string
	SidecarBootArgs []string
	UseBuiltinProxy bool
	EnvoyLogLevel   string
}

var pingpongT = template.Must(template.New("pingpong").Parse(`  #####################
  {{.NodeName}}-{{.PingPong}}:
    network_mode: 'service:{{.PodName}}'
    depends_on:
      - {{.NodeName}}
    image: rboyer/pingpong:latest
    labels:
      devconsul.type: "app"
    init: true
    command:
      - '-bind'
      # - '127.0.0.1:8080'
      - '0.0.0.0:8080'
      - '-dial'
      - '127.0.0.1:9090'
      - '-name'
      - '{{.PingPong}}{{.MetaString}}'

  {{.NodeName}}-{{.PingPong}}-sidecar:
    network_mode: 'service:{{.PodName}}'
    depends_on:
      - {{.NodeName}}-{{.PingPong}}
    image: local/consul-envoy
    labels:
      devconsul.type: "sidecar"
    init: true
    restart: on-failure
    volumes:
      - './cache:/secrets:ro'
      - './sidecar-boot.sh:/bin/sidecar-boot.sh:ro'
    command:
      - '/bin/sidecar-boot.sh'
{{- range .SidecarBootArgs }}
      - '{{.}}'
{{- end}}
      - '--'
      #################
      - '-sidecar-for'
      - '{{.PingPong}}'
{{- if not .UseBuiltinProxy }}
      - '-admin-bind'
      # for demo purposes
      - '0.0.0.0:19000'
      - '--'
      - '-l'
      - '{{ .EnvoyLogLevel }}'
{{- end }}
`))

func (c *CommandGenerate) generateMeshGatewayYAML(podName string, node *Node) (string, error) {
	if !node.MeshGateway {
		return "", nil
	}

	mgi := meshGatewayInfo{
		PodName:       podName,
		NodeName:      node.Name,
		EnvoyLogLevel: c.config.EnvoyLogLevel,
		Labels:        map[string]string{
			//
		},
	}
	node.AddLabels(mgi.Labels)

	switch c.topology.NetworkShape {
	case NetworkShapeIslands, NetworkShapeDual:
		mgi.EnableWAN = true
		mgi.ExposeServers = true
	case NetworkShapeFlat:
	default:
		panic("unknown shape: " + c.topology.NetworkShape)
	}

	var extraYAML bytes.Buffer
	if err := meshGatewayT.Execute(&extraYAML, &mgi); err != nil {
		return "", err
	}
	return extraYAML.String(), nil
}

type meshGatewayInfo struct {
	PodName       string
	NodeName      string
	EnvoyLogLevel string
	EnableWAN     bool
	ExposeServers bool
	Labels        map[string]string
}

var meshGatewayT = template.Must(template.New("mesh-gateway").Parse(`  #####################
  {{.NodeName}}-mesh-gateway:
    network_mode: 'service:{{.PodName}}'
    depends_on:
      - {{.NodeName}}
    image: local/consul-envoy
    labels:
      devconsul.type: "gateway"
{{- range $k, $v := .Labels }}
      {{ $k }}: "{{ $v }}"
{{- end }}
    init: true
    restart: on-failure
    volumes:
      - './cache:/secrets:ro'
      - './mesh-gateway-sidecar-boot.sh:/bin/mesh-gateway-sidecar-boot.sh:ro'
    command:
      - '/bin/mesh-gateway-sidecar-boot.sh'
      - "/secrets/ready.val"
      - "-t"
      - "/secrets/mesh-gateway.val"
      - '--'
      #################
{{- if .ExposeServers }}
      - '-expose-servers'
{{- end }}
{{- if .EnableWAN }}
      - '-wan-address'
      - '{{ "{{ GetInterfaceIP \"eth1\" }}:443" }}'
{{- end }}
      - '-admin-bind'
      # for demo purposes
      - '0.0.0.0:19000'
      - '--'
      - '-l'
      - '{{ .EnvoyLogLevel }}'
`))

func (c *CommandGenerate) generateAgentHCL(node *Node) (string, error) {
	configInfo := consulAgentConfigInfo{
		AdvertiseAddr:    node.LocalAddress(),
		RetryJoin:        `"` + strings.Join(c.topology.ServerIPs(node.Datacenter), `", "`) + `"`,
		Datacenter:       node.Datacenter,
		AgentMasterToken: c.config.AgentMasterToken,
		Server:           node.Server,
		GossipKey:        c.config.GossipKey,
		TLS:              c.config.EncryptionTLS,
		Prometheus:       c.config.PrometheusEnabled,
	}

	if node.Server {
		configInfo.MasterToken = c.config.InitialMasterToken

		wanIP := false
		wanfed := false
		switch c.topology.NetworkShape {
		case NetworkShapeIslands:
			wanfed = true
			if node.MeshGateway {
				wanIP = true
				configInfo.AdvertiseAddrWAN = node.PublicAddress()
			}
		case NetworkShapeDual:
			wanIP = true
			configInfo.AdvertiseAddrWAN = node.PublicAddress()
		case NetworkShapeFlat:
			// n/a
		default:
			panic("unknown shape: " + c.topology.NetworkShape)
		}

		var ips []string
		for _, dc := range c.topology.Datacenters() {
			ips = append(ips, c.topology.LeaderIP(dc.Name, wanIP))
		}

		if wanfed {
			configInfo.FederateViaGateway = true
			if node.Datacenter != PrimaryDC {
				primaryGateways := c.topology.GatewayAddrs(PrimaryDC)
				configInfo.PrimaryGateways = `"` + strings.Join(primaryGateways, `", "`) + `"`
			}
		} else {
			configInfo.RetryJoinWAN = `"` + strings.Join(ips, `", "`) + `"`
		}

		configInfo.SecondaryServer = node.Datacenter != PrimaryDC
		configInfo.BootstrapExpect = len(c.topology.ServerIPs(node.Datacenter))

		configInfo.TLSFilePrefix = node.Datacenter + "-server-consul-" + strconv.Itoa(node.Index)
	} else {
		configInfo.TLSFilePrefix = node.Datacenter + "-client-consul-" + strconv.Itoa(node.Index)
	}

	var buf bytes.Buffer
	if err := consulAgentConfigT.Execute(&buf, &configInfo); err != nil {
		return "", err
	}

	return buf.String(), nil
}

type consulAgentConfigInfo struct {
	AdvertiseAddr    string
	AdvertiseAddrWAN string
	RetryJoin        string
	RetryJoinWAN     string
	Datacenter       string
	SecondaryServer  bool
	MasterToken      string
	AgentMasterToken string
	Server           bool
	BootstrapExpect  int
	GossipKey        string
	TLS              bool
	TLSFilePrefix    string
	Prometheus       bool

	FederateViaGateway bool
	PrimaryGateways    string
}

var consulAgentConfigT = template.Must(template.New("consul-agent-config").Parse(`
{{ if .Server -}}
bootstrap_expect       = {{.BootstrapExpect}}
{{- end}}
client_addr            = "0.0.0.0"
advertise_addr         = "{{.AdvertiseAddr }}"
{{ if .AdvertiseAddrWAN -}}
advertise_addr_wan     = "{{.AdvertiseAddrWAN }}"
{{- end}}
translate_wan_addrs    = true
client_addr            = "0.0.0.0"
datacenter             = "{{.Datacenter}}"
disable_update_check   = true
log_level              = "debug"

enable_debug                  = true
enable_central_service_config = true

primary_datacenter     = "dc1"
retry_join             = [ {{.RetryJoin}} ]
{{ if .FederateViaGateway -}}
{{ if .SecondaryServer -}}
primary_gateways          = [ {{ .PrimaryGateways }} ]
primary_gateways_interval = "5s"
{{- end}}
{{ else -}}
{{ if .Server -}}
retry_join_wan         = [ {{.RetryJoinWAN}} ]
{{- end}}
{{- end}}
server                 = {{.Server}}
ui                     = true

{{ if .Prometheus }}
telemetry {
  prometheus_retention_time = "168h"
}
{{- end }}

{{ if .GossipKey }}
encrypt                = "{{.GossipKey}}"
{{- end }}

{{ if .TLS -}}
ca_file                = "/tls/consul-agent-ca.pem"
cert_file              = "/tls/{{.TLSFilePrefix}}.pem"
key_file               = "/tls/{{.TLSFilePrefix}}-key.pem"
verify_incoming        = true
verify_outgoing        = true
verify_server_hostname = true
{{- end }}

connect {
  enabled = true
  {{ if .FederateViaGateway -}}
  enable_mesh_gateway_wan_federation = true
  {{- end}}
}

{{ if not .Server -}}
ports {
  grpc = 8502
}
{{- end }}

acl {
  enabled                  = true
  default_policy           = "deny"
  down_policy              = "extend-cache"
  enable_token_persistence = true
  {{ if .SecondaryServer -}}
  enable_token_replication = true
  {{- end}}
  tokens {
    {{ if and .MasterToken .Server (not .SecondaryServer) -}}
    master       = "{{.MasterToken}}"
    {{- end }}
    agent_master = "{{.AgentMasterToken}}"
  }
}
`))

func indent(s string, n int) string {
	prefix := strings.Repeat(" ", n)

	var buf bytes.Buffer

	scan := bufio.NewScanner(strings.NewReader(s))
	for scan.Scan() {
		line := scan.Text()
		if strings.TrimSpace(scan.Text()) != "" {
			buf.WriteString(prefix + line + "\n")
		}
	}
	if scan.Err() != nil {
		panic("impossible to indent: " + scan.Err().Error())
	}

	return buf.String()
}

func (c *CommandGenerate) generatePrometheusConfigFile() error {
	type kv struct {
		Key, Val string
	}
	type job struct {
		Name        string
		MetricsPath string
		Params      map[string][]string
		Targets     []string
		Labels      []kv
	}

	jobs := make(map[string]*job)
	add := func(j *job) {
		prev, ok := jobs[j.Name]
		if ok {
			// only retain targets
			prev.Targets = append(prev.Targets, j.Targets...)
			j = prev
		} else {
			sort.Slice(j.Labels, func(a, b int) bool {
				return j.Labels[a].Key < j.Labels[b].Key
			})
			jobs[j.Name] = j
		}
		sort.Strings(j.Targets)
	}

	err := c.topology.Walk(func(node *Node) error {
		if node.Server {
			add(&job{
				Name:        "consul-servers-" + node.Datacenter,
				MetricsPath: "/v1/agent/metrics",
				Params: map[string][]string{
					"format": []string{"prometheus"},
					"token":  []string{c.config.AgentMasterToken},
				},
				Targets: []string{
					net.JoinHostPort(node.LocalAddress(), "8500"),
				},
				Labels: []kv{
					{"dc", node.Datacenter},
					// {"node", node.Name},
					{"role", "consul-server"},
				},
			})
		} else {
			add(&job{
				Name:        "consul-clients-" + node.Datacenter,
				MetricsPath: "/v1/agent/metrics",
				Params: map[string][]string{
					"format": []string{"prometheus"},
					"token":  []string{c.config.AgentMasterToken},
				},
				Targets: []string{
					net.JoinHostPort(node.LocalAddress(), "8500"),
				},
				Labels: []kv{
					{"dc", node.Datacenter},
					// {"node", node.Name},
					{"role", "consul-client"},
				},
			})

			if node.MeshGateway {
				add(&job{
					Name:        "mesh-gateways-" + node.Datacenter,
					MetricsPath: "/metrics",
					Targets: []string{
						net.JoinHostPort(node.LocalAddress(), "9102"),
					},
					Labels: []kv{
						{"dc", node.Datacenter},
						// {"node", node.Name},
						{"role", "mesh-gateway"},
					},
				})
			} else if node.Service != nil {
				add(&job{
					Name:        node.Service.Name + "-proxy",
					MetricsPath: "/metrics",
					Targets: []string{
						net.JoinHostPort(node.LocalAddress(), "9102"),
					},
					Labels: []kv{
						{"dc", node.Datacenter},
						// {"node", node.Name},
						{"role", node.Service.Name + "-proxy"},
					},
				})
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	info := struct {
		Jobs []*job
	}{}
	for _, j := range jobs {
		info.Jobs = append(info.Jobs, j)
	}
	sort.Slice(info.Jobs, func(i, j int) bool {
		return info.Jobs[i].Name < info.Jobs[j].Name
	})

	var out bytes.Buffer
	if err := prometheusConfigT.Execute(&out, &info); err != nil {
		return err
	}

	return c.updateFileIfDifferent(out.Bytes(), "cache/prometheus.yml", 0644)
}

var prometheusConfigT = template.Must(template.New("prometheus").Parse(`
# my global config
global:
  scrape_interval:     5s
  evaluation_interval: 5s

# Alertmanager configuration
alerting:
  alertmanagers:
  - static_configs:
    - targets:
      # - alertmanager:9093

# Load rules once and periodically evaluate them according to the global 'evaluation_interval'.
rule_files:
  # - "first_rules.yml"
  # - "second_rules.yml"

# A scrape configuration containing exactly one endpoint to scrape:
# Here it's Prometheus itself.
scrape_configs:
  - job_name: 'prometheus'

    # metrics_path defaults to '/metrics'
    # scheme defaults to 'http'.

    static_configs:
    - targets: ['localhost:9090']

{{- range .Jobs }}

  - job_name: {{.Name}}
    metrics_path: "{{.MetricsPath}}"
    params:
{{- range $k, $vl := .Params }}
      {{ $k }}:
{{- range $vl }}
      - {{ . }}
{{- end}}
{{- end}}
    static_configs:
    - targets:
{{- range .Targets }}
      - "{{ . }}"
{{- end }}
      labels:
{{- range .Labels }}
        {{ .Key }}: "{{ .Val }}"
{{- end }}
{{- end }}
`))

func (c *CommandGenerate) generateGrafanaConfigFiles() error {
	files := map[string]string{
		"grafana-prometheus.yml": `
apiVersion: 1

datasources:
- name: Prometheus
  type: prometheus
  access: proxy
  url: http://localhost:9090
  isDefault: true
  version: 1
  editable: false
`,
		"grafana.ini": `
[auth.anonymous]
enabled = true

# Organization name that should be used for unauthenticated users
org_name = Main Org.

# Role for unauthenticated users, other valid values are 'Editor' and 'Admin'
org_role = Admin
`,
	}

	for name, body := range files {
		if err := c.updateFileIfDifferent([]byte(body), filepath.Join("cache", name), 0644); err != nil {
			return err
		}
	}
	return nil
}

func (c *CommandGenerate) updateFileIfDifferent(body []byte, path string, perm os.FileMode) error {
	prev, err := ioutil.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		c.logger.Info("writing new file", "path", path)
	} else {
		// loaded
		if bytes.Equal(body, prev) {
			return nil
		}
		c.logger.Info("file has changed", "path", path)
	}

	_, err = safeio.WriteToFile(bytes.NewReader(body), path, perm)
	return err
}
