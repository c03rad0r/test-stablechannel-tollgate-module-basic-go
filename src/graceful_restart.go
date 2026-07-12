package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

// GracefulShutdown handles graceful shutdown and restart of the tollgate proxy
type GracefulShutdown struct {
	server        *http.Server
	configManager interface{} // Will be *config_manager.ConfigManager
	merchant      interface{} // Will be merchant.MerchantInterface
	cliServer     interface{} // Will be *cli.CLIServer
	upstreamDet   interface{} // Will be upstream detector
	logger        *logrus.Logger

	shutdownTimeout time.Duration
	restartDelay    time.Duration
	mu              sync.RWMutex
	isShuttingDown  bool
	isRestarting    bool
	done            chan struct{}
	listener        net.Listener
}

// NewGracefulShutdown creates a new graceful shutdown handler
func NewGracefulShutdown(server *http.Server, logger *logrus.Logger) *GracefulShutdown {
	return &GracefulShutdown{
		server:          server,
		logger:          logger,
		shutdownTimeout: 30 * time.Second,       // Default 30 second timeout
		restartDelay:    100 * time.Millisecond, // Small delay between shutdown and restart
		done:            make(chan struct{}),
	}
}

// SetListener sets the listener for socket handover during restart
func (gs *GracefulShutdown) SetListener(listener net.Listener) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	gs.listener = listener
}

// SetComponents sets the components that need graceful shutdown
func (gs *GracefulShutdown) SetComponents(configManager, merchant, cliServer, upstreamDet interface{}) {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	gs.configManager = configManager
	gs.merchant = merchant
	gs.cliServer = cliServer
	gs.upstreamDet = upstreamDet
}

// SetShutdownTimeout sets the shutdown timeout
func (gs *GracefulShutdown) SetShutdownTimeout(timeout time.Duration) {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	gs.shutdownTimeout = timeout
}

// SetRestartDelay sets the delay between shutdown and restart
func (gs *GracefulShutdown) SetRestartDelay(delay time.Duration) {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	gs.restartDelay = delay
}

// WaitForShutdown waits for shutdown signal and performs graceful shutdown/restart
func (gs *GracefulShutdown) WaitForShutdown() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGUSR2)

	// Wait for signal
	sig := <-sigChan
	gs.logger.WithField("signal", sig).Info("Received shutdown signal")

	gs.mu.Lock()
	gs.isShuttingDown = true
	gs.mu.Unlock()

	switch sig {
	case syscall.SIGHUP:
		gs.handleReload()
	case syscall.SIGUSR2:
		gs.handleRestart()
	case syscall.SIGINT, syscall.SIGTERM:
		gs.handleShutdown()
	}
}

// handleShutdown performs graceful shutdown
func (gs *GracefulShutdown) handleShutdown() {
	gs.logger.Info("Starting graceful shutdown...")

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), gs.shutdownTimeout)
	defer cancel()

	// Shutdown HTTP server first (stop accepting new connections)
	if gs.server != nil {
		gs.logger.Info("Shutting down HTTP server...")
		if err := gs.server.Shutdown(ctx); err != nil {
			gs.logger.WithError(err).Error("Failed to shutdown HTTP server gracefully")
		}
	}

	// Cleanup merchant resources
	if gs.merchant != nil {
		gs.cleanupMerchant()
	}

	// Cleanup CLI server
	if gs.cliServer != nil {
		gs.cleanupCLIServer()
	}

	// Cleanup upstream detector
	if gs.upstreamDet != nil {
		gs.cleanupUpstreamDetector()
	}

	gs.logger.Info("Graceful shutdown completed")
	close(gs.done)
}

// handleReload handles configuration reload (SIGHUP)
func (gs *GracefulShutdown) handleReload() {
	gs.logger.Info("Reloading configuration...")

	// For config reload, we can do a graceful restart
	gs.handleRestart()
}

// handleRestart performs graceful restart with socket handover
func (gs *GracefulShutdown) handleRestart() {
	gs.logger.Info("Starting graceful restart...")

	gs.mu.Lock()
	if gs.isRestarting {
		gs.mu.Unlock()
		gs.logger.Warn("Restart already in progress")
		return
	}
	gs.isRestarting = true
	gs.mu.Unlock()

	// Get the listener file descriptor for socket handover
	var listenerFD uintptr
	if gs.listener != nil {
		// Type assert to *net.TCPListener to get File() method
		if tcpListener, ok := gs.listener.(*net.TCPListener); ok {
			file, err := tcpListener.File()
			if err != nil {
				gs.logger.WithError(err).Error("Failed to get listener file descriptor")
				gs.handleShutdown()
				return
			}
			listenerFD = file.Fd()
			defer file.Close()
		} else {
			gs.logger.Error("Listener is not a TCPListener, cannot get file descriptor for graceful restart")
			gs.handleShutdown()
			return
		}
	}

	// Shutdown the current server gracefully
	ctx, cancel := context.WithTimeout(context.Background(), gs.shutdownTimeout)
	defer cancel()

	if gs.server != nil {
		gs.logger.Info("Shutting down HTTP server for restart...")
		if err := gs.server.Shutdown(ctx); err != nil {
			gs.logger.WithError(err).Error("Failed to shutdown HTTP server gracefully")
		}
	}

	// Cleanup resources
	if gs.merchant != nil {
		gs.cleanupMerchant()
	}
	if gs.cliServer != nil {
		gs.cleanupCLIServer()
	}
	if gs.upstreamDet != nil {
		gs.cleanupUpstreamDetector()
	}

	// Start the new process
	if err := gs.startNewProcess(listenerFD); err != nil {
		gs.logger.WithError(err).Error("Failed to start new process")
		gs.handleShutdown()
		return
	}

	gs.logger.Info("Graceful restart initiated - new process started")
	os.Exit(0)
}

// startNewProcess starts a new process with socket handover
func (gs *GracefulShutdown) startNewProcess(listenerFD uintptr) error {
	// Get the current executable path
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	// Prepare environment variables
	env := os.Environ()
	if gs.listener != nil {
		env = append(env, fmt.Sprintf("TOLLGATE_LISTENER_FD=%d", listenerFD))
	}

	// Prepare command arguments
	args := []string{filepath.Base(execPath)}
	if gs.listener != nil {
		args = append(args, "--graceful-restart")
		args = append(args, "--listener-fd="+strconv.Itoa(int(listenerFD)))
	}

	gs.logger.WithFields(logrus.Fields{
		"executable": execPath,
		"args":       args,
	}).Info("Starting new process")

	// Start the new process
	cmd := exec.Command(execPath, args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Start()
}

// cleanupMerchant cleans up merchant resources
func (gs *GracefulShutdown) cleanupMerchant() {
	gs.logger.Info("Cleaning up merchant resources...")

	// Type assertion to check if merchant has Close method
	if merchant, ok := gs.merchant.(interface{ Close() error }); ok {
		if err := merchant.Close(); err != nil {
			gs.logger.WithError(err).Error("Failed to close merchant")
		}
	} else {
		gs.logger.Debug("Merchant does not have Close method")
	}
}

// cleanupCLIServer cleans up CLI server
func (gs *GracefulShutdown) cleanupCLIServer() {
	gs.logger.Info("Cleaning up CLI server...")

	// Type assertion to check if CLI server has Close method
	if cliServer, ok := gs.cliServer.(interface{ Close() error }); ok {
		if err := cliServer.Close(); err != nil {
			gs.logger.WithError(err).Error("Failed to close CLI server")
		}
	} else {
		gs.logger.Debug("CLI server does not have Close method")
	}
}

// cleanupUpstreamDetector cleans up upstream detector
func (gs *GracefulShutdown) cleanupUpstreamDetector() {
	gs.logger.Info("Cleaning up upstream detector...")

	// Type assertion to check if upstream detector has Stop method
	if upstreamDet, ok := gs.upstreamDet.(interface{ Stop() error }); ok {
		if err := upstreamDet.Stop(); err != nil {
			gs.logger.WithError(err).Error("Failed to stop upstream detector")
		}
	} else {
		gs.logger.Debug("Upstream detector does not have Stop method")
	}
}

// IsShuttingDown returns true if shutdown is in progress
func (gs *GracefulShutdown) IsShuttingDown() bool {
	gs.mu.RLock()
	defer gs.mu.RUnlock()
	return gs.isShuttingDown
}

// IsRestarting returns true if restart is in progress
func (gs *GracefulShutdown) IsRestarting() bool {
	gs.mu.RLock()
	defer gs.mu.RUnlock()
	return gs.isRestarting
}

// Done returns a channel that's closed when shutdown is complete
func (gs *GracefulShutdown) Done() <-chan struct{} {
	return gs.done
}

// Health check handler
func (gs *GracefulShutdown) healthHandler(w http.ResponseWriter, r *http.Request) {
	if gs.IsShuttingDown() {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"status": "shutting_down"}`)
		return
	}

	if gs.IsRestarting() {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"status": "restarting"}`)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status": "healthy"}`)
}

// RegisterHealthHandler registers the health check endpoint
func (gs *GracefulShutdown) RegisterHealthHandler(mux *http.ServeMux) {
	mux.HandleFunc("/health", gs.healthHandler)
	gs.logger.Info("Health check endpoint registered at /health")
}

// CreateListenerFromFileDescriptor creates a net.Listener from a file descriptor
func CreateListenerFromFileDescriptor(fdStr string) (net.Listener, error) {
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		return nil, fmt.Errorf("invalid file descriptor: %w", err)
	}

	// Reopen the file descriptor
	file := os.NewFile(uintptr(fd), "")
	if file == nil {
		return nil, fmt.Errorf("failed to create file from descriptor")
	}
	defer file.Close()

	// Create a file listener
	listener, err := net.FileListener(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create listener from file: %w", err)
	}

	return listener, nil
}
