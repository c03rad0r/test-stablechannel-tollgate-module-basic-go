package merchant

import (
	"sync"
	"time"
)

// RateLimitKey identifies the entity being rate limited
type RateLimitKey struct {
	Identifier string // MAC address, IP address, etc.
	Operation  string // "purchase", "create_token", "fund", etc.
}

// RateLimitConfig defines rate limit parameters
type RateLimitConfig struct {
	Window      time.Duration // Time window for rate limiting
	MaxRequests int          // Maximum allowed requests in window
}

// RateLimitRecord tracks rate limit state for a key
type RateLimitRecord struct {
	requests []int64 // Timestamps of requests
	mu       sync.Mutex
}

// RateLimiter provides shared rate limiting across operations
type RateLimiter struct {
	records map[RateLimitKey]*RateLimitRecord
	configs map[string]RateLimitConfig // Operation -> config
	mu      sync.RWMutex
}

// NewRateLimiter creates a new rate limiter with default configurations
func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{
		records: make(map[RateLimitKey]*RateLimitRecord),
		configs: make(map[string]RateLimitConfig),
	}

	// Default rate limit configurations
	rl.configs["purchase"] = RateLimitConfig{
		Window:      time.Minute,
		MaxRequests: 10, // 10 purchases per minute per MAC
	}
	rl.configs["create_token"] = RateLimitConfig{
		Window:      time.Minute,
		MaxRequests: 30, // 30 token creations per minute per MAC
	}
	rl.configs["fund"] = RateLimitConfig{
		Window:      time.Minute,
		MaxRequests: 20, // 20 fund operations per minute per MAC
	}
	rl.configs["lightning_invoice"] = RateLimitConfig{
		Window:      time.Minute,
		MaxRequests: 5, // 5 Lightning invoice requests per minute per MAC
	}

	return rl
}

// CheckRateLimit checks if a request is allowed
func (rl *RateLimiter) CheckRateLimit(key RateLimitKey) (bool, time.Duration) {
	rl.mu.RLock()
	config, exists := rl.configs[key.Operation]
	rl.mu.RUnlock()

	if !exists {
		// No rate limit configured for this operation
		return true, 0
	}

	rl.mu.Lock()
	record, exists := rl.records[key]
	if !exists {
		record = &RateLimitRecord{
			requests: make([]int64, 0),
		}
		rl.records[key] = record
	}
	rl.mu.Unlock()

	return record.check(config)
}

// checkRateLimitMAC is a convenience function for MAC-based rate limiting
func (rl *RateLimiter) CheckRateLimitMAC(macAddress, operation string) (bool, time.Duration) {
	key := RateLimitKey{
		Identifier: macAddress,
		Operation:  operation,
	}
	return rl.CheckRateLimit(key)
}

// check verifies if a request is allowed against the configuration
func (r *RateLimitRecord) check(config RateLimitConfig) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().Unix()
	cutoff := now - int64(config.Window.Seconds())

	// Remove expired requests
	validRequests := make([]int64, 0)
	for _, timestamp := range r.requests {
		if timestamp > cutoff {
			validRequests = append(validRequests, timestamp)
		}
	}
	r.requests = validRequests

	// Check if limit exceeded
	if len(r.requests) >= config.MaxRequests {
		// Calculate time until oldest request expires
		if len(r.requests) > 0 {
			oldestRequest := r.requests[0]
			waitTime := time.Duration(oldestRequest-cutoff) * time.Second
			return false, waitTime
		}
		return false, config.Window
	}

	// Record this request
	r.requests = append(r.requests, now)
	return true, 0
}

// Cleanup removes expired records to prevent memory leaks
func (rl *RateLimiter) Cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now().Unix()
	for key, record := range rl.records {
		record.mu.Lock()
		
		// Remove expired requests
		validRequests := make([]int64, 0)
		for _, timestamp := range record.requests {
			// Keep requests from the last 5 minutes to avoid immediate re-creation
			if timestamp > now-300 { // 5 minutes
				validRequests = append(validRequests, timestamp)
			}
		}
		
		// If no recent requests, remove the record
		if len(validRequests) == 0 {
			delete(rl.records, key)
		} else {
			record.requests = validRequests
		}
		
		record.mu.Unlock()
	}
}

// GetRecordCount returns the number of active rate limit records (for monitoring)
func (rl *RateLimiter) GetRecordCount() int {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	return len(rl.records)
}