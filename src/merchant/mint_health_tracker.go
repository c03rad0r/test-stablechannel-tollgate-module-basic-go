package merchant

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OpenTollGate/tollgate-module-basic-go/src/config_manager"
)

const (
	defaultRecoveryThreshold uint8 = 3
	// lnRecoveryThreshold controls how many consecutive successful LN probe
	// responses are required before re-declaring a mint Lightning-capable after
	// an LN failure. It is lower than defaultRecoveryThreshold (3 for Cashu)
	// because Cashu failures block ALL payment methods — requiring a higher bar
	// for recovery. LN failures only affect one payment method (Lightning)
	// while Cashu payments continue working. A threshold of 2 provides noise
	// immunity against transient network blips (single-packet-loss false
	// positives) while still recovering faster than the Cashu path.
	lnRecoveryThreshold uint8 = 2
	probeTimeout              = 30 * time.Second
	probeInterval             = 5 * time.Minute

	// Aggressive retry: when no mints are reachable at startup (e.g. WiFi STA
	// not yet connected), probe every 15s with immediate recovery (threshold=1)
	// for up to 5 minutes. This complements the OpenWrt hotplug script that
	// restarts tollgate when the wwan interface comes up.
	aggressiveProbeInterval = 15 * time.Second
	aggressiveProbeTimeout  = 10 * time.Second
	aggressiveDuration      = 5 * time.Minute

	// Emergency scaling fix: add cooldown periods to prevent oscillations
	aggressiveModeCooldown            = 2 * time.Minute  // Cooldown before re-entering aggressive mode
	normalModeCooldown                = 1 * time.Minute  // Cooldown before exiting aggressive mode
	minLogInterval                    = 30 * time.Second // Minimum interval between similar log messages
	aggressiveRecoveryThreshold uint8 = 2                // More conservative recovery in aggressive mode (was effectively 1)

	// Lightning capability probe. We verify a mint's LN backend is actually
	// working by requesting a minimal 1-sat mint quote (NUT-04). The mint's
	// /v1/info only advertises protocol-level NUT-04 support — it does NOT tell
	// us whether the backing Lightning node (e.g. coinos.io) is reachable, so a
	// real quote request is the only reliable signal. A 1-sat invoice is the
	// smallest side effect that proves end-to-end LN availability.
	lnProbeTimeout  = 15 * time.Second
	lnProbeAmount   = 1
	lnQuoteEndpoint = "/v1/mint/quote/bolt11"
)

type mintConfigProvider interface {
	GetConfig() *config_manager.Config
}

type MintHealthTracker struct {
	mu             sync.RWMutex
	reachableMints map[string]bool

	// Emergency scaling fix: fields to prevent oscillations
	lastAggressiveExit   time.Time
	lastNormalExit       time.Time
	lastLogTime          map[string]time.Time
	inAggressiveMode     bool
	aggressiveModeExitCh chan struct{}

	supportsLN             map[string]bool
	consecutiveSuccesses   map[string]uint8
	lnConsecutiveSuccesses map[string]uint8
	httpClient             *http.Client
	lnProbeClient          *http.Client
	configProvider         mintConfigProvider
	recoveryThreshold      uint8
	onFirstReachable       func()
	hadReachableMint       bool
	onReachableSetChanged  func()
	reachableCount         int
	stopCh                 chan struct{}
}

func NewMintHealthTracker(configProvider mintConfigProvider) *MintHealthTracker {
	return &MintHealthTracker{
		reachableMints: make(map[string]bool),
		// Emergency scaling fix: initialize oscillation prevention fields
		lastLogTime:      make(map[string]time.Time),
		inAggressiveMode: false,

		supportsLN:             make(map[string]bool),
		consecutiveSuccesses:   make(map[string]uint8),
		lnConsecutiveSuccesses: make(map[string]uint8),
		httpClient: &http.Client{
			Timeout: probeTimeout,
		},
		lnProbeClient: &http.Client{
			Timeout: lnProbeTimeout,
		},
		configProvider:    configProvider,
		recoveryThreshold: defaultRecoveryThreshold,
	}
}

func (t *MintHealthTracker) StartProactiveChecks() {
	t.mu.Lock()
	if t.stopCh != nil {
		t.mu.Unlock()
		return
	}
	t.stopCh = make(chan struct{})
	stopCh := t.stopCh

	// Emergency scaling fix: initialize cooldown timers
	now := time.Now()
	t.lastAggressiveExit = now.Add(-aggressiveModeCooldown) // Allow immediate aggressive mode at startup
	t.lastNormalExit = now.Add(-normalModeCooldown)

	needAggressive := t.reachableCount == 0 && t.canEnterAggressiveMode()
	t.mu.Unlock()

	// Emergency scaling fix: use rate limited logging
	t.rateLimitedLog("StartProactiveChecks", "starting aggressive retry (no reachable mints at startup)")

	go func() {
		var aggressiveDone chan struct{}
		if needAggressive {
			// Emergency scaling fix: removed redundant log, using rateLimitedLog above
			aggressiveDone = t.runAggressiveRetry(stopCh)
			go func() {
				<-aggressiveDone
				t.rateLimitedLog("StartProactiveChecks", "aggressive retry completed")
			}()
		}

		ticker := time.NewTicker(probeInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				t.runProactiveCheck()
			case <-stopCh:
				return
			}
		}
	}()
}

func (t *MintHealthTracker) runAggressiveRetry(stopCh chan struct{}) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		aggressiveClient := &http.Client{Timeout: aggressiveProbeTimeout}
		ticker := time.NewTicker(aggressiveProbeInterval)
		defer ticker.Stop()
		timer := time.NewTimer(aggressiveDuration)
		defer timer.Stop()

		for {
			// Emergency scaling fix: mark that we're in aggressive mode
			t.mu.Lock()
			t.inAggressiveMode = true
			t.mu.Unlock()

			select {
			case <-ticker.C:
				if t.runAggressiveCheck(aggressiveClient) {
					t.rateLimitedLog("runAggressiveRetry", "mint became reachable, stopping aggressive mode")
					return
				}
			case <-timer.C:
				t.rateLimitedLog("runAggressiveRetry", "aggressive period ended (%v), falling back to normal interval", aggressiveDuration)
				return
			case <-stopCh:
				return
			}
		}
	}()
	return done
}

func (t *MintHealthTracker) Stop() {
	t.mu.Lock()
	if t.stopCh != nil {
		close(t.stopCh)
		t.stopCh = nil
	}
	t.mu.Unlock()
}

// Emergency scaling fix: check if we can enter aggressive mode (respects cooldown)
func (t *MintHealthTracker) canEnterAggressiveMode() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return time.Since(t.lastAggressiveExit) >= aggressiveModeCooldown
}

// Emergency scaling fix: check if we can exit aggressive mode (respects cooldown)
func (t *MintHealthTracker) canExitAggressiveMode() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return time.Since(t.lastNormalExit) >= normalModeCooldown
}

// Emergency scaling fix: rate limited logging to prevent log spam
func (t *MintHealthTracker) rateLimitedLog(prefix string, format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	key := prefix + ":" + message
	t.mu.RLock()
	lastLog, exists := t.lastLogTime[key]
	t.mu.RUnlock()

	if !exists || time.Since(lastLog) >= minLogInterval {
		log.Printf(prefix+": "+message, args...)
		t.mu.Lock()
		t.lastLogTime[key] = time.Now()
		t.mu.Unlock()
	}
}

func (t *MintHealthTracker) IsReachable(mintURL string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.reachableMints[mintURL]
}

// SupportsLN reports whether a mint's Lightning backend was verified working
// during the most recent probe. It is only meaningful for reachable mints; an
// unreachable mint is always Lightning-incapable. Lightning capability is
// probed by requesting a minimal mint quote (NUT-04), which exercises the
// mint's backing Lightning node (e.g. coinos.io) end-to-end.
func (t *MintHealthTracker) SupportsLN(mintURL string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.supportsLN[mintURL]
}

// MarkLNUnavailable reactively degrades a mint's Lightning capability without
// affecting its reachability. Call this when a real invoice request fails at
// runtime (e.g. the mint returned an error mid-purchase) so Lightning is no
// longer advertised until the next proactive probe re-verifies it.
//
// The LN recovery counter is reset so re-enabling Lightning takes the full
// lnRecoveryThreshold consecutive successful probes — the same conservative
// window used when a failure is detected proactively. Without the reset, a
// reactive degradation (a real purchase just failed) would recover after a
// single successful probe, which would be less conservative than a proactively
// detected failure that resets the counter to zero.
func (t *MintHealthTracker) MarkLNUnavailable(mintURL string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.supportsLN[mintURL] {
		log.Printf("MarkLNUnavailable: degrading Lightning capability for mint %s (reactive)", mintURL)
	}
	t.supportsLN[mintURL] = false
	t.lnConsecutiveSuccesses[mintURL] = 0
}

func (t *MintHealthTracker) GetReachableMintConfigs() []config_manager.MintConfig {
	config := t.configProvider.GetConfig()
	if config == nil {
		return nil
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	var reachable []config_manager.MintConfig
	for _, mint := range config.AcceptedMints {
		if t.reachableMints[mint.URL] {
			reachable = append(reachable, mint)
		}
	}
	return reachable
}

func (t *MintHealthTracker) GetAllConfiguredMintConfigs() []config_manager.MintConfig {
	config := t.configProvider.GetConfig()
	if config == nil {
		return nil
	}
	return config.AcceptedMints
}

func (t *MintHealthTracker) MarkUnreachable(mintURL string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.reachableMints[mintURL] {
		t.reachableCount--
	}
	t.reachableMints[mintURL] = false
	t.consecutiveSuccesses[mintURL] = 0
}

// SetOnFirstReachableForDegraded registers a callback that fires once when a mint
// becomes reachable after starting with none. The hadReachableMint flag is reset to
// false so the callback fires on the first mint recovery — this is only meaningful
// for the degraded merchant path which starts with all mints unreachable.
func (t *MintHealthTracker) SetOnFirstReachableForDegraded(callback func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onFirstReachable = callback
	t.hadReachableMint = false
}

func (t *MintHealthTracker) SetOnReachableSetChanged(callback func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onReachableSetChanged = callback
}

func (t *MintHealthTracker) RunInitialProbe() {
	config := t.configProvider.GetConfig()
	if config == nil {
		return
	}

	log.Printf("RunInitialProbe: probing %d mint(s)", len(config.AcceptedMints))
	reachable := make(map[string]bool, len(config.AcceptedMints))
	lnSupported := make(map[string]bool, len(config.AcceptedMints))
	for _, mint := range config.AcceptedMints {
		ok := t.probeMint(mint.URL)
		reachable[mint.URL] = ok
		// Only reachable mints can be Lightning-capable; probing LN for a mint
		// we can't even reach would just add latency with no signal.
		if ok {
			lnSupported[mint.URL] = t.probeLightningCapability(mint.URL, t.lnProbeClient)
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for url, ok := range reachable {
		if ok {
			t.reachableMints[url] = true
			t.consecutiveSuccesses[url] = t.recoveryThreshold
		} else {
			t.reachableMints[url] = false
			t.consecutiveSuccesses[url] = 0
		}
	}

	for url, lnOK := range lnSupported {
		if !reachable[url] {
			t.supportsLN[url] = false
			t.lnConsecutiveSuccesses[url] = 0
			continue
		}
		t.supportsLN[url] = lnOK
		if lnOK {
			t.lnConsecutiveSuccesses[url] = lnRecoveryThreshold
			log.Printf("RunInitialProbe: mint %s supports Lightning", url)
		} else {
			t.lnConsecutiveSuccesses[url] = 0
			log.Printf("RunInitialProbe: mint %s Lightning backend DEGRADED (reachable but LN quote probe failed) — Lightning will not be advertised", url)
		}
	}

	t.reachableCount = 0
	for _, mint := range config.AcceptedMints {
		if t.reachableMints[mint.URL] {
			t.hadReachableMint = true
			t.reachableCount++
		}
	}
}

func (t *MintHealthTracker) RunProactiveCheck() {
	t.runProactiveCheck()
}

func (t *MintHealthTracker) runProactiveCheck() {
	config := t.configProvider.GetConfig()
	if config == nil {
		return
	}

	t.rateLimitedLog("runProactiveCheck", "probing %d mint(s)", len(config.AcceptedMints))
	reachable := make(map[string]bool, len(config.AcceptedMints))
	lnSupported := make(map[string]bool, len(config.AcceptedMints))
	for _, mint := range config.AcceptedMints {
		ok := t.probeMint(mint.URL)
		reachable[mint.URL] = ok
		if ok {
			lnSupported[mint.URL] = t.probeLightningCapability(mint.URL, t.lnProbeClient)
		}
	}

	t.mu.Lock()

	// Emergency scaling fix: check if we need to enter aggressive mode
	shouldEnterAggressive := false
	for _, mint := range config.AcceptedMints {
		if !reachable[mint.URL] && t.reachableMints[mint.URL] {
			// A mint that was reachable is now unreachable
			shouldEnterAggressive = true
			break
		}
	}

	for _, mint := range config.AcceptedMints {
		if reachable[mint.URL] {
			t.consecutiveSuccesses[mint.URL]++
			if !t.reachableMints[mint.URL] && t.consecutiveSuccesses[mint.URL] >= t.recoveryThreshold {
				t.reachableMints[mint.URL] = true
			}
		} else {
			t.consecutiveSuccesses[mint.URL] = 0
			t.reachableMints[mint.URL] = false
		}
	}

	// Refresh Lightning capability. A mint is only LN-capable when it has
	// accumulated lnRecoveryThreshold consecutive successful LN probes, providing
	// noise immunity against transient network blips. If the LN probe fails the
	// counter resets and Lightning is degraded immediately.
	for _, mint := range config.AcceptedMints {
		if !t.reachableMints[mint.URL] {
			t.supportsLN[mint.URL] = false
			t.lnConsecutiveSuccesses[mint.URL] = 0
			continue
		}
		wasLN := t.supportsLN[mint.URL]
		nowLN := lnSupported[mint.URL]
		if nowLN {
			t.lnConsecutiveSuccesses[mint.URL]++
		} else {
			t.lnConsecutiveSuccesses[mint.URL] = 0
		}
		t.supportsLN[mint.URL] = t.lnConsecutiveSuccesses[mint.URL] >= lnRecoveryThreshold
		if t.supportsLN[mint.URL] && !wasLN {
			log.Printf("runProactiveCheck: mint %s Lightning backend recovered — Lightning re-advertised", mint.URL)
		} else if !t.supportsLN[mint.URL] && wasLN {
			log.Printf("runProactiveCheck: mint %s Lightning backend DEGRADED (reachable but LN quote probe failed) — Lightning will not be advertised", mint.URL)
		}
	}

	newCount := 0
	for _, mint := range config.AcceptedMints {
		if t.reachableMints[mint.URL] {
			newCount++
		}
	}

	setChanged := newCount != t.reachableCount
	t.reachableCount = newCount

	// Emergency scaling fix: update normal mode exit time
	t.lastNormalExit = time.Now()

	// Emergency scaling fix: start aggressive mode if needed and cooldown allows
	if shouldEnterAggressive && newCount == 0 && t.canEnterAggressiveMode() && !t.inAggressiveMode {
		t.inAggressiveMode = true
		go func() {
			aggressiveDone := t.runAggressiveRetry(t.stopCh)
			<-aggressiveDone
		}()
	}

	var callbacks []func()

	if !t.hadReachableMint && t.onFirstReachable != nil {
		for _, mint := range config.AcceptedMints {
			if t.reachableMints[mint.URL] {
				t.hadReachableMint = true
				callbacks = append(callbacks, t.onFirstReachable)
				break
			}
		}
	}

	if setChanged && t.onReachableSetChanged != nil {
		callbacks = append(callbacks, t.onReachableSetChanged)
	}

	t.mu.Unlock()

	for _, cb := range callbacks {
		t.rateLimitedLog("runProactiveCheck", "firing callback (hadReachable=%v, setChanged=%v)", t.hadReachableMint, setChanged)
		go cb()
	}
}

// runAggressiveCheck probes mints with conservative recovery (threshold=2).
// Returns true if a previously-unreachable mint became reachable.
func (t *MintHealthTracker) runAggressiveCheck(aggressiveClient *http.Client) bool {
	config := t.configProvider.GetConfig()
	if config == nil {
		return false
	}

	t.rateLimitedLog("runAggressiveCheck", "probing %d mint(s) with conservative recovery", len(config.AcceptedMints))
	reachable := make(map[string]bool, len(config.AcceptedMints))
	lnSupported := make(map[string]bool, len(config.AcceptedMints))
	for _, mint := range config.AcceptedMints {
		ok := t.probeMintWith(mint.URL, aggressiveClient)
		reachable[mint.URL] = ok
		if ok {
			lnSupported[mint.URL] = t.probeLightningCapability(mint.URL, t.lnProbeClient)
		}
	}

	t.mu.Lock()

	recovered := false
	for _, mint := range config.AcceptedMints {
		if reachable[mint.URL] {
			t.consecutiveSuccesses[mint.URL]++

			// Emergency scaling fix: use conservative recovery threshold in aggressive mode
			if !t.reachableMints[mint.URL] && t.consecutiveSuccesses[mint.URL] >= aggressiveRecoveryThreshold {
				t.reachableMints[mint.URL] = true
				recovered = true
			}
		} else {
			t.consecutiveSuccesses[mint.URL] = 0
			t.reachableMints[mint.URL] = false
		}
	}

	for _, mint := range config.AcceptedMints {
		if !t.reachableMints[mint.URL] {
			t.supportsLN[mint.URL] = false
			t.lnConsecutiveSuccesses[mint.URL] = 0
			continue
		}
		t.supportsLN[mint.URL] = lnSupported[mint.URL]
		if lnSupported[mint.URL] {
			t.lnConsecutiveSuccesses[mint.URL] = lnRecoveryThreshold
		} else {
			t.lnConsecutiveSuccesses[mint.URL] = 0
		}
	}

	newCount := 0
	for _, mint := range config.AcceptedMints {
		if t.reachableMints[mint.URL] {
			newCount++
		}
	}

	setChanged := newCount != t.reachableCount
	t.reachableCount = newCount

	var callbacks []func()

	if !t.hadReachableMint && t.onFirstReachable != nil {
		for _, mint := range config.AcceptedMints {
			if t.reachableMints[mint.URL] {
				t.hadReachableMint = true
				callbacks = append(callbacks, t.onFirstReachable)
				break
			}
		}
	}

	if setChanged && t.onReachableSetChanged != nil {
		callbacks = append(callbacks, t.onReachableSetChanged)
	}

	// Emergency scaling fix: update aggressive mode exit time and state
	if recovered && t.canExitAggressiveMode() {
		t.lastAggressiveExit = time.Now()
		t.inAggressiveMode = false
	}

	t.mu.Unlock()

	for _, cb := range callbacks {
		t.rateLimitedLog("runAggressiveCheck", "firing callback (hadReachable=%v, setChanged=%v)", t.hadReachableMint, setChanged)
		go cb()
	}

	return recovered
}

func (t *MintHealthTracker) probeMint(mintURL string) bool {
	return t.probeMintWith(mintURL, t.httpClient)
}

func (t *MintHealthTracker) probeMintWith(mintURL string, client *http.Client) bool {
	url := strings.TrimRight(mintURL, "/") + "/v1/info"

	start := time.Now()
	resp, err := client.Get(url)
	elapsed := time.Since(start)
	if err != nil {
		t.rateLimitedLog("mint probe", "FAILED: url=%s elapsed=%s error=%v", url, elapsed, err)
		return false
	}
	defer resp.Body.Close()

	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	t.rateLimitedLog("mint probe", "url=%s status=%d elapsed=%s ok=%v", url, resp.StatusCode, elapsed, ok)
	return ok
}

// probeLightningCapability verifies that a mint can actually issue a Lightning
// invoice by requesting a minimal mint quote (Cashu NUT-04,
// POST /v1/mint/quote/bolt11). This exercises the mint's backing Lightning
// node end-to-end — a mint whose /v1/info is healthy but whose LN backend
// (e.g. coinos.io) is down will fail here, letting us withhold Lightning as a
// payment option instead of failing silently at purchase time.
//
// The probe creates a real 1-sat invoice on the mint; that is the smallest
// side effect that proves end-to-end LN availability and is the documented
// trade-off (per the LN capability probe task). A 2xx response carrying a
// non-empty bolt11 invoice ("request" field) counts as success.
func (t *MintHealthTracker) probeLightningCapability(mintURL string, client *http.Client) bool {
	url := strings.TrimRight(mintURL, "/") + lnQuoteEndpoint

	body := fmt.Sprintf(`{"amount":%d,"unit":"sat"}`, lnProbeAmount)

	start := time.Now()
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	elapsed := time.Since(start)
	if err != nil {
		t.rateLimitedLog("ln probe", "FAILED: url=%s elapsed=%s error=%v", url, elapsed, err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.rateLimitedLog("ln probe", "url=%s status=%d elapsed=%s ok=false (non-2xx; LN backend likely down)", url, resp.StatusCode, elapsed)
		return false
	}

	var quote struct {
		Request string `json:"request"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&quote); err != nil {
		t.rateLimitedLog("ln probe", "url=%s decode error=%v", url, err)
		return false
	}

	ok := quote.Request != ""
	t.rateLimitedLog("ln probe", "url=%s elapsed=%s ok=%v (invoice_len=%d)", url, elapsed, ok, len(quote.Request))
	return ok
}
