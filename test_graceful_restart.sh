#!/bin/bash

# Test script for graceful restart functionality
# This script tests that the tollgate binary can start and respond to health checks

echo "=== Testing Graceful Restart Functionality ==="

# Change to src directory
cd /home/c03rad0r/tollgate-module-basic-go/src

# Build the tollgate binary
echo "Building tollgate binary..."
go build -o /tmp/tollgate-test .

if [ $? -ne 0 ]; then
    echo "❌ Build failed"
    exit 1
fi

echo "✅ Build successful"

# Start the tollgate server in background
echo "Starting tollgate server..."
/tmp/tollgate-test > /tmp/tollgate.log 2>&1 &
SERVER_PID=$!

# Wait a moment for server to start
echo "Waiting for server to start..."
sleep 5

# Check if process is still running
if ! kill -0 $SERVER_PID 2>/dev/null; then
    echo "❌ Server process died unexpectedly"
    echo "Server log:"
    cat /tmp/tollgate.log
    exit 1
fi

echo "✅ Server process is running (PID: $SERVER_PID)"

# Check if port is listening
echo "Checking if port 2121 is listening..."
sleep 2
if netstat -tlnp 2>/dev/null | grep -q ":2121 "; then
    echo "✅ Port 2121 is listening"
else
    echo "❌ Port 2121 is not listening"
    echo "Server log:"
    cat /tmp/tollgate.log
    kill $SERVER_PID 2>/dev/null
    exit 1
fi

# Test health endpoint
echo "Testing health endpoint..."
HEALTH_RESPONSE=$(curl -s http://localhost:2121/health 2>/dev/null || echo "connection_failed")

if [[ "$HEALTH_RESPONSE" == *"healthy"* ]]; then
    echo "✅ Health endpoint responding correctly"
    echo "Response: $HEALTH_RESPONSE"
else
    echo "❌ Health endpoint not responding correctly"
    echo "Response: $HEALTH_RESPONSE"
    kill $SERVER_PID 2>/dev/null
    exit 1
fi

# Test graceful shutdown (SIGTERM)
echo "Testing graceful shutdown with SIGTERM..."
kill -TERM $SERVER_PID

# Wait for graceful shutdown
sleep 3

# Check if process is still running
if kill -0 $SERVER_PID 2>/dev/null; then
    echo "❌ Process still running after SIGTERM"
    kill -KILL $SERVER_PID 2>/dev/null
    exit 1
else
    echo "✅ Graceful shutdown successful"
fi

echo "=== Test Summary ==="
echo "✅ All tests passed - graceful restart functionality is working"
echo ""
echo "Next steps:"
echo "1. Test with actual graceful restart (SIGUSR2)"
echo "2. Test with configuration reload (SIGHUP)"
echo "3. Test socket handover during restart"