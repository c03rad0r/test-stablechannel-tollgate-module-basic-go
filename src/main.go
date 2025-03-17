package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/elnosh/gonuts/cashu"
	"github.com/nbd-wtf/go-nostr"

	"github.com/OpenTollgate/tip01/modules"
)

var tollgateMerchantPubkey string = "c1f4c025e746fd307203ac3d1a1886e343bea76ceec5e286c96fb353be6cadea"
var tollgateDetails string = "{\n  \"id\": \"dacb1643bc64a4d58732c2c4cc3a81e5b85320953cb3cd5daf3331cd6b163e96\",\n  \"pubkey\": \"714161f55b3198b6f95f1d23ca9ee8132052574f7785fcc859cb1f3cf2a2cf5f\",\n  \"created_at\": 1742065776,\n  \"kind\": 21021,\n  \"tags\": [\n    [\n      \"allocation_type\",\n      \"sec\"\n    ],\n    [\n      \"allocation_per_1024\",\n      \"2048\"\n    ],\n    [\n      \"mint\",\n      \"https://testnut.cashu.space\",\n      \"sat\"\n    ]\n  ],\n  \"content\": \"\",\n  \"sig\": \"cbd66538a7911e76b5132df6a2d3886811cb54a8e4cec6c45fcbb13f0bc94f86ae89b3ab2998bb8dc4455355546b27e301f7397fd5afe9956a0ebf282ac30b3e\"\n}"

func getMacAddress(ipAddress string) (string, error) {
	cmdIn := `cat /tmp/dhcp.leases | cut -f 2,3,4 -s -d" " | grep -i ` + ipAddress + ` | cut -f 1 -s -d" "`
	commandOutput, err := exec.Command("sh", "-c", cmdIn).Output()

	var commandOutputString = string(commandOutput)
	if err != nil {
		fmt.Println(err, "Error when getting client's mac address. Command output: "+commandOutputString)
		return "nil", err
	}

	return strings.Trim(commandOutputString, "\n"), nil
}

// CORS middleware to handle Cross-Origin Resource Sharing
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("cors middleware %s request from %s", r.Method, r.RemoteAddr)

		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*") // Allow any origin, or specify domains like "https://yourdomain.com"
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		// Handle preflight OPTIONS requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Call the next handler
		next(w, r)
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	var ip = getIP(r)
	var mac, err = getMacAddress(ip)

	if err != nil {
		log.Println("Error getting MAC address:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	fmt.Println("mac", mac)
	fmt.Fprintf(w, "mac="+mac)
}

func handleDetails(w http.ResponseWriter, r *http.Request) {
	fmt.Println("Details requested")
	fmt.Fprintf(w, tollgateDetails)
}

// handleRootPost handles POST requests to the root endpoint
func handleRootPost(w http.ResponseWriter, r *http.Request) {
	// Log the request details
	log.Printf("Received handleRootPost %s request from %s", r.Method, r.RemoteAddr)
	// Only process POST requests
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Println("Error reading request body:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// Print the request body to console
	bodyStr := string(body)
	log.Println("Received POST request with body:", bodyStr)

	// Parse the request body as a nostr event
	var event nostr.Event
	err = json.Unmarshal(body, &event)
	if err != nil {
		log.Println("Error parsing nostr event:", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Verify the event signature
	ok, err := event.CheckSignature()
	if err != nil || !ok {
		log.Println("Invalid signature for nostr event:", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	log.Println("Parsed nostr event:", event.ID)
	log.Println("  - Created at:", event.CreatedAt)
	log.Println("  - Kind:", event.Kind)
	log.Println("  - Pubkey:", event.PubKey)

	// Extract MAC address from device-identifier tag
	var macAddress string
	for _, tag := range event.Tags {
		if len(tag) > 0 && tag[0] == "device-identifier" && len(tag) >= 3 {
			macAddress = tag[2]
			break
		}
	}
	if macAddress == "" {
		log.Println("Missing or invalid device-identifier tag")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Extract payment token from payment tag
	var paymentToken string
	for _, tag := range event.Tags {
		if len(tag) > 0 && tag[0] == "payment" && len(tag) >= 2 {
			paymentToken = tag[1]
			break
		}
	}
	if paymentToken == "" {
		log.Println("Missing or invalid payment tag")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	log.Printf("Extracted MAC address: %s", macAddress)
	log.Printf("Extracted payment token: %s", paymentToken)

	// Decode the Cashu token using gonuts
	tokenValue, err := decodeCashuToken(paymentToken)
	if err != nil {
		log.Printf("Error decoding Cashu token: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	log.Printf("Cashu token value: %d sats", tokenValue)

	// Calculate duration based on token value (1 minute per sat)
	duration := int64(tokenValue * 60) // convert to seconds
	if duration < 60 {
		duration = 60 // minimum 1 minute
	}

	// Open gate for the specified duration using the valve module
	err = modules.OpenGate(macAddress, duration)
	if err != nil {
		log.Printf("Error opening gate: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Return a success status with token info
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Access granted for %d minutes", duration/60)
}

// decodeCashuToken decodes a Cashu token and returns the total value in sats
func decodeCashuToken(token string) (int, error) {
	fmt.Println("Decoding Cashu token:", token)

	// Only support cashuB tokens
	if !strings.HasPrefix(strings.ToLower(token), "cashub") {
		return 0, fmt.Errorf("only cashuB tokens are supported")
	}

	// Remove the "cashuB" prefix
	// token = token[6:]

	// Log the token length for debugging
	log.Printf("Attempting to decode cashuB token of length: %d", len(token))

	// Try V4 specific decode - cashuB tokens use this format
	v4Token, err := cashu.DecodeTokenV4(token)
	if err != nil {
		log.Printf("Failed to decode cashuB token: %v", err)
		return 0, fmt.Errorf("error decoding cashuB token: %w", err)
	}

	// Get the amount directly from the token
	amount := int(v4Token.Amount())
	log.Printf("Successfully decoded cashuB token with amount: %d sats", amount)

	return amount, nil
}

// handleRoot routes requests based on method
func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		handleRootPost(w, r)
	} else {
		handleDetails(w, r)
	}
}

func testHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("Received %s request from %s to %s\n", r.Method, getIP(r), r.URL.Path)
}

func main() {
	var port = ":2121" // Change from "0.0.0.0:2121" to just ":2121"
	fmt.Println("Starting Tollgate - TIP-01")
	fmt.Println("Listening on all interfaces on port", port)

	// Add verbose logging for debugging
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("Registering handlers...")

	http.HandleFunc("/x", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("DEBUG: Hit /x endpoint from %s", r.RemoteAddr)
		testHandler(w, r)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("DEBUG: Hit / endpoint from %s", r.RemoteAddr)
		corsMiddleware(handleRoot)(w, r)
	})

	http.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("DEBUG: Hit /whoami endpoint from %s", r.RemoteAddr)
		corsMiddleware(handler)(w, r)
	})

	log.Println("Starting HTTP server on all interfaces...")
	server := &http.Server{
		Addr: port,
		// Add explicit timeouts to avoid potential deadlocks in Go 1.24
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Fatal(server.ListenAndServe())

	fmt.Println("Shutting down Tollgate - Whoami")
}

func getIP(r *http.Request) string {
	// Check if the IP is set in the X-Real-Ip header
	ip := r.Header.Get("X-Real-Ip")
	if ip != "" {
		return ip
	}

	// Check if the IP is set in the X-Forwarded-For header
	ips := r.Header.Get("X-Forwarded-For")
	if ips != "" {
		return strings.Split(ips, ",")[0]
	}

	// Fallback to the remote address, removing the port
	ip = r.RemoteAddr
	if colon := strings.LastIndex(ip, ":"); colon != -1 {
		ip = ip[:colon]
	}

	return ip
}
