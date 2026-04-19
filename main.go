package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

type Machine struct {
	Label             string `yaml:"label"`
	Port              int    `yaml:"port"`
	IP                string `yaml:"ip"`
	MAC               string `yaml:"mac"`
	Broadcast         string `yaml:"broadcast"`
	SSHPort           int    `yaml:"ssh_port"`
	WOLPort           int    `yaml:"wol_port"`
	KeepAlivePackets  bool   `yaml:"keepalive_packets"`
}

type Config struct {
	ListenHost                string    `yaml:"listen_host"`
	WakeTimeout               int       `yaml:"wake_timeout"`
	PollInterval              int       `yaml:"poll_interval"`
	KeepAlivePacketsInterval  int       `yaml:"keepalive_packets_interval"`
	Machines                  []Machine `yaml:"machines"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.ListenHost == "" {
		c.ListenHost = "0.0.0.0"
	}
	if c.WakeTimeout == 0 {
		c.WakeTimeout = 120
	}
	if c.PollInterval == 0 {
		c.PollInterval = 3
	}
	if c.KeepAlivePacketsInterval == 0 {
		c.KeepAlivePacketsInterval = 30
	}
	for i := range c.Machines {
		if c.Machines[i].SSHPort == 0 {
			c.Machines[i].SSHPort = 22
		}
		if c.Machines[i].WOLPort == 0 {
			c.Machines[i].WOLPort = 9
		}
	}
	return &c, nil
}

// buildMagicPacket creates a Wake-on-LAN magic packet:
// 6 bytes of 0xFF followed by the target MAC repeated 16 times.
func buildMagicPacket(mac string) ([]byte, error) {
	clean := strings.ReplaceAll(strings.ReplaceAll(mac, ":", ""), "-", "")
	macBytes, err := hex.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("invalid MAC %q: %w", mac, err)
	}
	if len(macBytes) != 6 {
		return nil, fmt.Errorf("MAC %q must be 6 bytes", mac)
	}
	packet := make([]byte, 6+16*6)
	for i := 0; i < 6; i++ {
		packet[i] = 0xFF
	}
	for i := 0; i < 16; i++ {
		copy(packet[6+i*6:], macBytes)
	}
	return packet, nil
}

func sendWOL(m Machine) error {
	packet, err := buildMagicPacket(m.MAC)
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("%s:%d", m.Broadcast, m.WOLPort)
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(packet)
	return err
}

func isUp(m Machine, timeout time.Duration) bool {
	addr := fmt.Sprintf("%s:%d", m.IP, m.SSHPort)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func waitForWake(m Machine, wakeTimeout, pollInterval time.Duration) bool {
	deadline := time.Now().Add(wakeTimeout)
	for time.Now().Before(deadline) {
		if isUp(m, 2*time.Second) {
			return true
		}
		time.Sleep(pollInterval)
	}
	return false
}

func proxyConn(client net.Conn, m Machine) {
	addr := fmt.Sprintf("%s:%d", m.IP, m.SSHPort)
	upstream, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		log.Printf("[%s] failed to dial upstream %s: %v", m.Label, addr, err)
		return
	}
	defer upstream.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(upstream, client)
		if tc, ok := upstream.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(client, upstream)
		if tc, ok := client.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	wg.Wait()
}

func keepaliveWorker(m Machine, intervalSec int, counter *atomic.Int64) {
	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if n := counter.Load(); n > 0 {
			log.Printf("[%s] keepalive: sending WOL packet (%d active connection(s))", m.Label, n)
			if err := sendWOL(m); err != nil {
				log.Printf("[%s] keepalive: WOL send failed: %v", m.Label, err)
			}
		}
	}
}

func handleConnection(client net.Conn, m Machine, cfg *Config, counter *atomic.Int64) {
	defer client.Close()
	if counter != nil {
		counter.Add(1)
		defer counter.Add(-1)
	}
	remote := client.RemoteAddr().String()
	log.Printf("[%s] new connection from %s", m.Label, remote)

	if isUp(m, 2*time.Second) {
		log.Printf("[%s] already awake, proxying immediately", m.Label)
		proxyConn(client, m)
		return
	}

	log.Printf("[%s] offline, sending WOL to %s (broadcast %s:%d)", m.Label, m.MAC, m.Broadcast, m.WOLPort)
	if err := sendWOL(m); err != nil {
		log.Printf("[%s] WOL send failed: %v", m.Label, err)
		return
	}

	wakeTimeout := time.Duration(cfg.WakeTimeout) * time.Second
	pollInterval := time.Duration(cfg.PollInterval) * time.Second
	log.Printf("[%s] waiting up to %s for SSH port %d...", m.Label, wakeTimeout, m.SSHPort)
	if !waitForWake(m, wakeTimeout, pollInterval) {
		log.Printf("[%s] timeout waiting for wake, closing connection from %s", m.Label, remote)
		return
	}

	log.Printf("[%s] awake, proxying connection from %s", m.Label, remote)
	proxyConn(client, m)
}

func listenMachine(m Machine, cfg *Config, counter *atomic.Int64) {
	addr := fmt.Sprintf("%s:%d", cfg.ListenHost, m.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("[%s] failed to listen on %s: %v", m.Label, addr, err)
	}
	log.Printf("[%s] listening on %s -> %s:%d (MAC %s)", m.Label, addr, m.IP, m.SSHPort, m.MAC)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[%s] accept error: %v", m.Label, err)
			continue
		}
		go handleConnection(conn, m, cfg, counter)
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	configPath := "/etc/ssh-wol/config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("failed to load config %s: %v", configPath, err)
	}
	if len(cfg.Machines) == 0 {
		log.Fatalf("no machines defined in %s", configPath)
	}

	log.Printf("loaded %d machine(s) from %s", len(cfg.Machines), configPath)

	// Validate MACs and uniqueness of ports up front.
	seenPorts := map[int]string{}
	for _, m := range cfg.Machines {
		if _, err := buildMagicPacket(m.MAC); err != nil {
			log.Fatalf("[%s] %v", m.Label, err)
		}
		if other, ok := seenPorts[m.Port]; ok {
			log.Fatalf("port %d used by both %q and %q", m.Port, other, m.Label)
		}
		seenPorts[m.Port] = m.Label
	}

	for _, m := range cfg.Machines {
		var counter *atomic.Int64
		if m.KeepAlivePackets {
			counter = &atomic.Int64{}
			go keepaliveWorker(m, cfg.KeepAlivePacketsInterval, counter)
			log.Printf("[%s] keepalive packets enabled (interval %ds)", m.Label, cfg.KeepAlivePacketsInterval)
		}
		go listenMachine(m, cfg, counter)
	}

	select {}
}
