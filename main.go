package main

import (
	"fmt"
	"log"
	"net"
	"os/exec"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/songgao/water"
)

// Global variables for IP management and clients
var (
	ipPool    = []string{"10.0.0.2", "10.0.0.3", "10.0.0.4"} // IP pool
	clientIPs = make(map[string]string)                      // Map of client ID to assigned IP
	clients   = make(map[string]net.Conn)                    // Map of client ID to connections
	clientMux sync.Mutex                                     // Mutex to protect client maps
	ipMux     sync.Mutex                                     // Mutex to protect IP pool
)

const clientTimeout = 60 * time.Second // Timeout period for inactive clients

func main() {
	// Create and configure the TAP device
	config := water.Config{
		DeviceType: water.TAP,
	}
	config.Name = "tap0"

	iface, err := water.New(config)
	if err != nil {
		log.Fatalf("Failed to create TAP device: %v", err)
	}
	fmt.Printf("TAP device %s created successfully!\n", iface.Name())
	setupTapDevice(iface.Name())

	// Start the server
	listener, err := net.Listen("tcp", ":8080")
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
	defer listener.Close()
	fmt.Println("Server listening on port 8080...")

	go handleTapTraffic(iface)

	go func() {
		for {
			time.Sleep(clientTimeout / 2)
			cleanupInactiveClients()
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept client connection: %v", err)
			continue
		}
		fmt.Printf("New client connected: %s\n", conn.RemoteAddr().String())
		go handleClient(conn, iface)
	}
}

// Helper to configure the TAP device
func setupTapDevice(ifaceName string) {
	cmd := exec.Command("sudo", "ip", "addr", "add", "10.0.0.1/24", "dev", ifaceName)
	if err := cmd.Run(); err != nil {
		log.Fatalf("Failed to assign IP to TAP device: %v", err)
	}

	cmd = exec.Command("sudo", "ip", "link", "set", ifaceName, "up")
	if err := cmd.Run(); err != nil {
		log.Fatalf("Failed to bring TAP device up: %v", err)
	}
	fmt.Printf("TAP device %s configured successfully!\n", ifaceName)
}

func cleanupInactiveClients() {
	clientMux.Lock()
	defer clientMux.Unlock()

	for clientID, conn := range clients {
		// Check if the client connection is still active
		if netErr, ok := conn.(net.Error); ok && netErr.Timeout() {
			fmt.Printf("Client %s inactive. Removing.\n", clientID)
			releaseIP(clientID)
			delete(clients, clientID)
			conn.Close()
		}
	}
}

// Updated handleClient function with activity tracking
func handleClient(conn net.Conn, iface *water.Interface) {
	defer conn.Close()

	clientID := conn.RemoteAddr().String()
	fmt.Printf("Client connected: %s\n", clientID)

	ip := assignIP(clientID)
	fmt.Printf("Assigned IP %s to client %s\n", ip, clientID)

	clientMux.Lock()
	clients[clientID] = conn
	clientMux.Unlock()

	buf := make([]byte, 1500)
	for {
		conn.SetReadDeadline(time.Now().Add(clientTimeout))
		n, err := conn.Read(buf)
		if err != nil {
			if err.Error() == "EOF" || isTimeoutError(err) {
				fmt.Printf("Client %s timed out or disconnected.\n", clientID)
				break
			}
			log.Printf("Error reading from client %s: %v\n", clientID, err)
			continue
		}

		// Write client packets to the TAP device
		_, err = iface.Write(buf[:n])
		if err != nil {
			log.Printf("Error writing to TAP device: %v", err)
		}
	}

	// Clean up on disconnect
	releaseIP(clientID)
	clientMux.Lock()
	delete(clients, clientID)
	clientMux.Unlock()
}

// Helper to check for timeout errors
func isTimeoutError(err error) bool {
	netErr, ok := err.(net.Error)
	return ok && netErr.Timeout()
}

func assignIP(clientID string) string {
	ipMux.Lock()
	defer ipMux.Unlock()

	// Check if the client already has an IP
	if ip, exists := clientIPs[clientID]; exists {
		return ip
	}

	// Assign the next available IP
	if len(ipPool) == 0 {
		log.Fatalf("No more IPs available in the pool!")
	}
	ip := ipPool[0]
	ipPool = ipPool[1:] // Remove assigned IP from the pool
	clientIPs[clientID] = ip
	return ip
}

func releaseIP(clientID string) {
	ipMux.Lock()
	defer ipMux.Unlock()

	// Check if the client has an IP assigned
	if ip, exists := clientIPs[clientID]; exists {
		delete(clientIPs, clientID) // Remove from map
		ipPool = append(ipPool, ip) // Return IP to the pool
	}
}

func handleTapTraffic(iface *water.Interface) {
	packet := make([]byte, 1500) // Buffer for incoming packets
	for {
		n, err := iface.Read(packet)
		if err != nil {
			log.Printf("Error reading from TAP device: %v", err)
			break
		}

		// Parse the packet to check its destination
		dstIP, isBroadcast := parsePacket(packet[:n])

		if isBroadcast {
			fmt.Println("Broadcast packet detected")
			broadcastPacket(packet[:n]) // Send to all connected clients
		} else if dstIP != nil {
			fmt.Printf("Unicast packet detected for IP: %s\n", dstIP.String())
			unicastPacket(packet[:n], dstIP) // Send to the specific client
		} else {
			log.Println("Unknown or unsupported packet type, skipping...")
		}
	}
}

// Parse the packet to determine its destination IP and type
func parsePacket(packet []byte) (net.IP, bool) {
	parser := gopacket.NewPacket(packet, layers.LayerTypeEthernet, gopacket.Default)
	ipLayer := parser.Layer(layers.LayerTypeIPv4)

	if ipLayer != nil {
		ip := ipLayer.(*layers.IPv4)
		// Check if it's a broadcast
		if ip.DstIP.Equal(net.IPv4bcast) || isBroadcastIP(ip.DstIP) {
			return nil, true
		}
		return ip.DstIP, false // Return destination IP
	}

	// Return nil if the packet is not an IPv4 packet
	return nil, false
}

// Check if the IP is the subnet broadcast address
func isBroadcastIP(ip net.IP) bool {
	return ip.Equal(net.ParseIP("10.0.0.255")) // Example broadcast IP for 10.0.0.0/24
}

// Forward broadcast packets to all connected clients
func broadcastPacket(packet []byte) {
	clientMux.Lock()
	defer clientMux.Unlock()
	for _, conn := range clients {
		_, err := conn.Write(packet)
		if err != nil {
			log.Printf("Error forwarding broadcast packet to client: %v", err)
		}
	}
}

// Forward unicast packets to the specific client
func unicastPacket(packet []byte, dstIP net.IP) {
	clientMux.Lock()
	defer clientMux.Unlock()
	for clientID, ip := range clientIPs {
		if ip == dstIP.String() {
			if conn, exists := clients[clientID]; exists {
				_, err := conn.Write(packet)
				if err != nil {
					log.Printf("Error forwarding unicast packet to client: %v", err)
				}
			}
			break
		}
	}
}
