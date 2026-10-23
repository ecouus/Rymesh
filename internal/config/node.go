package config

import (
	"net"
	"os"
	"strconv"
	"strings"
)

// NodeConfig is one remote device. A direct device is reached at URL (a LAN
// or VPN address); an SSH device through an SSH connection to its loopback
// port (internal/sshconn), with HostKey pinned on first contact.
type NodeConfig struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url,omitempty"`
	Token      string `json:"token"`
	Transport  string `json:"transport,omitempty"` // "" (direct) or "ssh"
	SSHHost    string `json:"ssh_host,omitempty"`
	SSHPort    int    `json:"ssh_port,omitempty"`
	SSHUser    string `json:"ssh_user,omitempty"`
	KeyPath    string `json:"key_path,omitempty"` // a key file (~/.ssh)
	KeyID      string `json:"key_id,omitempty"`   // or a key in the vault (internal/store/vault.go)
	UseAgent   bool   `json:"use_agent,omitempty"`
	HostKey    string `json:"host_key,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"`
}

// LocalNodeID names this instance in the device list; it is never proxied.
const LocalNodeID = "local"

func (c *Config) DisplayName() string {
	if c.NodeName != "" {
		return c.NodeName
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "本机"
}

func (c *Config) FindNode(id string) (NodeConfig, bool) {
	for _, n := range c.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return NodeConfig{}, false
}

const TransportSSH = "ssh"

func (n NodeConfig) SSHAddr() string {
	port := n.SSHPort
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(n.SSHHost, strconv.Itoa(port))
}

func (n NodeConfig) EffectiveRemotePort() int {
	if n.RemotePort == 0 {
		return DefaultPort
	}
	return n.RemotePort
}

// BaseURL is where the relay sends requests. For SSH devices the host part is
// only a label: the dial goes through the tunnel to the node's loopback.
func (n NodeConfig) BaseURL() string {
	if n.Transport == TransportSSH {
		return "http://127.0.0.1:" + strconv.Itoa(n.EffectiveRemotePort())
	}
	return n.URL
}

// key identifies a device's connection settings; editing a device makes a
// fresh entry instead of reusing a connection made with old credentials.
func (n NodeConfig) SSHKey() string {
	return strings.Join([]string{n.ID, n.SSHHost, strconv.Itoa(n.SSHPort), n.SSHUser,
		n.KeyPath, n.KeyID, strconv.FormatBool(n.UseAgent), n.HostKey}, "|")
}
