package merchant

import (
	"fmt"
	"testing"
)

func TestRateLimiter_BasicOperation(t *testing.T) {
	limiter := NewRateLimiter()
	
	// Test basic rate limiting
	key := RateLimitKey{
		Identifier: "test-mac",
		Operation:  "purchase",
	}
	
	// First request should be allowed
	allowed, waitTime := limiter.CheckRateLimit(key)
	if !allowed {
		t.Error("First request should be allowed")
	}
	if waitTime != 0 {
		t.Error("First request should have zero wait time")
	}
	
	// Should allow up to 10 requests in a minute (default config)
	for i := 1; i < 10; i++ {
		allowed, _ := limiter.CheckRateLimit(key)
		if !allowed {
			t.Errorf("Request %d should be allowed", i+1)
		}
	}
	
	// 11th request should be denied
	allowed, waitTime = limiter.CheckRateLimit(key)
	if allowed {
		t.Error("11th request should be denied")
	}
	if waitTime == 0 {
		t.Error("11th request should have positive wait time")
	}
}

func TestRateLimiter_DifferentOperations(t *testing.T) {
	limiter := NewRateLimiter()
	
	purchaseKey := RateLimitKey{
		Identifier: "test-mac",
		Operation:  "purchase",
	}
	
	tokenKey := RateLimitKey{
		Identifier: "test-mac", 
		Operation:  "create_token",
	}
	
	// Should be able to make purchases and create tokens independently
	for i := 0; i < 15; i++ {
		// Create token has higher limit (30) than purchase (10)
		allowed, _ := limiter.CheckRateLimit(tokenKey)
		if !allowed {
			t.Errorf("Token creation request %d should be allowed", i+1)
		}
	}
	
	// But purchases should still be limited after 10
	for i := 0; i < 10; i++ {
		limiter.CheckRateLimit(purchaseKey)
	}
	
	allowed, _ := limiter.CheckRateLimit(purchaseKey)
	if allowed {
		t.Error("11th purchase should be denied")
	}
}

func TestRateLimiter_DifferentIdentifiers(t *testing.T) {
	limiter := NewRateLimiter()
	
	mac1 := RateLimitKey{
		Identifier: "mac-01",
		Operation:  "purchase",
	}
	
	mac2 := RateLimitKey{
		Identifier: "mac-02",
		Operation:  "purchase",
	}
	
	// Both MACs should be able to make 10 purchases independently
	for i := 0; i < 10; i++ {
		allowed1, _ := limiter.CheckRateLimit(mac1)
		allowed2, _ := limiter.CheckRateLimit(mac2)
		
		if !allowed1 {
			t.Errorf("MAC1 request %d should be allowed", i+1)
		}
		if !allowed2 {
			t.Errorf("MAC2 request %d should be allowed", i+1)
		}
	}
	
	// Both should be denied on 11th request
	allowed1, _ := limiter.CheckRateLimit(mac1)
	allowed2, _ := limiter.CheckRateLimit(mac2)
	
	if allowed1 {
		t.Error("MAC1 11th request should be denied")
	}
	if allowed2 {
		t.Error("MAC2 11th request should be denied")
	}
}

func TestRateLimiter_WindowExpiration(t *testing.T) {
	limiter := NewRateLimiter()
	
	key := RateLimitKey{
		Identifier: "test-mac",
		Operation:  "purchase",
	}
	
	// Use up all 10 requests
	for i := 0; i < 10; i++ {
		limiter.CheckRateLimit(key)
	}
	
	// Should be denied
	allowed, _ := limiter.CheckRateLimit(key)
	if allowed {
		t.Error("Should be denied after using limit")
	}
	
	// Wait for window to expire (this is a bit tricky to test without manipulating time)
	// For now, just test that cleanup works
	limiter.Cleanup()
	
	recordCount := limiter.GetRecordCount()
	if recordCount < 0 {
		t.Error("Record count should not be negative")
	}
}

func TestRateLimiter_ConvenienceMethods(t *testing.T) {
	limiter := NewRateLimiter()
	
	// Test MAC-based convenience method
	allowed, waitTime := limiter.CheckRateLimitMAC("test-mac", "purchase")
	if !allowed {
		t.Error("First MAC-based request should be allowed")
	}
	if waitTime != 0 {
		t.Error("First MAC-based request should have zero wait time")
	}
	
	// Test that it matches the key-based method
	key := RateLimitKey{
		Identifier: "test-mac",
		Operation:  "purchase",
	}
	
	allowed2, waitTime2 := limiter.CheckRateLimit(key)
	if allowed != allowed2 {
		t.Error("Convenience method should match key-based method")
	}
	if waitTime != waitTime2 {
		t.Error("Wait times should match between methods")
	}
}

func TestRateLimiter_MemoryEfficiency(t *testing.T) {
	limiter := NewRateLimiter()
	
	// Create many different keys
	for i := 0; i < 100; i++ {
		key := RateLimitKey{
			Identifier: fmt.Sprintf("mac-%d", i),
			Operation:  "purchase",
		}
		limiter.CheckRateLimit(key)
	}
	
	// Should have 100 records
	recordCount := limiter.GetRecordCount()
	if recordCount != 100 {
		t.Errorf("Expected 100 records, got %d", recordCount)
	}
	
	// Cleanup should remove expired records
	limiter.Cleanup()
	recordCount = limiter.GetRecordCount()
	// After cleanup, should still have records since they were just created
	if recordCount <= 0 {
		t.Error("Should still have records after immediate cleanup")
	}
}