package cache

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"time"

	galaxycache "github.com/vimeo/galaxycache"
	gchttp "github.com/vimeo/galaxycache/http"

	"github.com/Anthony-Bible/sre-bible/internal/metrics"
	"github.com/Anthony-Bible/sre-bible/internal/server"
)

// galaxyName is the single galaxy this tier owns. It is also the bounded value of
// the "galaxy" metric attribute (see metrics.RegisterSessionCacheObservers).
const galaxyName = "session-verified"

// Config holds the resolved galaxycache tier configuration. All values are derived
// from SESSION_CACHE_* env vars in cmd/server; see CLAUDE.md for the table.
type Config struct {
	// SelfIP is this pod's own IP (Downward-API MY_POD_IP). Required: it forms the
	// galaxycache self ID, which must match the self entry produced by peer refresh.
	SelfIP string
	// ListenAddr is the peer HTTP listener address, e.g. ":9091". Its port is reused
	// when building peer URLs from resolved DNS addresses.
	ListenAddr string
	// MaxBytes is the galaxy main-cache byte budget per replica.
	MaxBytes int64
	// TTL is the WithGetTTL max time-to-live — eviction hygiene only, not a
	// correctness mechanism. An evicted "true" is simply re-fetched and re-cached.
	TTL time.Duration
	// PeerRefresh is the interval between headless-DNS re-resolves.
	PeerRefresh time.Duration
	// HeadlessDNS is the headless Service DNS name to resolve peer IPs from.
	HeadlessDNS string
}

// Tier owns the galaxycache universe + galaxy and their lifecycle: the read-through
// decorator (Store), the peer HTTP endpoint (Handler), the DNS-driven peer refresh
// loop (RefreshPeers), the metrics bridge (RegisterMetrics), and shutdown (Close).
type Tier struct {
	universe *galaxycache.Universe
	galaxy   *galaxycache.Galaxy
	store    *CachingSessionStore
	cfg      Config
	selfURL  string
	port     string
	log      *slog.Logger
	// lastPeers is the sorted peer-URL set most recently handed to universe.Set,
	// used to skip redundant ring rebuilds. Touched only by the RefreshPeers
	// goroutine, so it needs no lock.
	lastPeers []string
}

// New builds a Tier wrapping store. The universe is created with an HTTP fetch
// protocol and this pod's self URL as its ID; the galaxy reads through the
// verified-set getter (where the DB read lives) and evicts on TTL (with ~10%
// jitter to avoid synchronized expiry). No listener is started and no peers are
// set here — the caller mounts Handler() and runs RefreshPeers().
func New(cfg Config, store server.SessionRepository, log *slog.Logger) (*Tier, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.SelfIP == "" {
		return nil, fmt.Errorf("session cache: self IP (MY_POD_IP) is required")
	}
	_, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("session cache: invalid listen addr %q: %w", cfg.ListenAddr, err)
	}
	if port == "" {
		return nil, fmt.Errorf("session cache: listen addr %q has no port", cfg.ListenAddr)
	}
	selfURL := peerURL(cfg.SelfIP, port)

	universe := galaxycache.NewUniverse(gchttp.NewHTTPFetchProtocol(nil), selfURL)
	galaxy := universe.NewGalaxy(
		galaxyName,
		cfg.MaxBytes,
		newVerifiedGetter(store),
		galaxycache.WithGetTTL(cfg.TTL, cfg.TTL/10),
	)

	t := &Tier{
		universe: universe,
		galaxy:   galaxy,
		cfg:      cfg,
		selfURL:  selfURL,
		port:     port,
		log:      log,
	}
	t.store = newCachingSessionStore(store, galaxy, log)
	return t, nil
}

// Store returns the read-through decorator to wire in place of the raw
// SessionRepository. The decorator is transparent: 11 of 12 methods pass straight
// through, only IsSessionVerified consults the cache.
func (t *Tier) Store() server.SessionRepository { return t.store }

// Handler returns the galaxycache peer endpoint to mount on the cache listener.
// It serves the cluster-internal /_galaxycache/ fetch routes; it is never exposed
// publicly. A fresh mux isolates these routes from the public and metrics muxes.
func (t *Tier) Handler() http.Handler {
	mux := http.NewServeMux()
	gchttp.RegisterHTTPHandler(t.universe, nil, mux)
	return mux
}

// RegisterMetrics bridges this galaxy's cumulative stats into OTel observable
// counters. The closure snapshots galaxy.Stats at each metric collection.
func (t *Tier) RegisterMetrics() error {
	return metrics.RegisterSessionCacheObservers(func() metrics.SessionCacheStats {
		return metrics.SessionCacheStats{
			MaincacheHits:     t.galaxy.Stats.MaincacheHits.Get(),
			BackendLoads:      t.galaxy.Stats.BackendLoads.Get(),
			PeerLoads:         t.galaxy.Stats.PeerLoads.Get(),
			BackendLoadErrors: t.galaxy.Stats.BackendLoadErrors.Get(),
		}
	})
}

// RefreshPeers blocks, re-resolving the headless Service DNS every PeerRefresh and
// updating the universe's peer set, until ctx is cancelled. It resolves once
// immediately so the peer set is populated before the first tick. A DNS failure is
// logged and the current peer set is kept (fail-open: stale peers only ever cost an
// extra DB read). Run it in its own goroutine.
func (t *Tier) RefreshPeers(ctx context.Context) {
	t.refreshOnce(ctx)
	ticker := time.NewTicker(t.cfg.PeerRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.refreshOnce(ctx)
		}
	}
}

// refreshOnce resolves the headless DNS and sets the universe peer set. Self is
// always included so this replica stays authoritative for its hash range even
// before the headless Service reports it ready. The DNS lookup is bounded by
// PeerRefresh so a hung resolver can never wedge the refresh loop, and the
// universe is only re-set when the resolved peer set actually changed — in steady
// state (stable replica count) this skips the consistent-hash ring rebuild every
// tick. refreshOnce runs only on the single RefreshPeers goroutine, so lastPeers
// needs no synchronisation.
func (t *Tier) refreshOnce(ctx context.Context) {
	lookupCtx, cancel := context.WithTimeout(ctx, t.cfg.PeerRefresh)
	ips, err := net.DefaultResolver.LookupHost(lookupCtx, t.cfg.HeadlessDNS)
	cancel()
	if err != nil {
		t.log.WarnContext(ctx, "session cache peer DNS lookup failed; keeping current peer set",
			slog.String("dns", t.cfg.HeadlessDNS), slog.Any("err", err))
		return
	}
	urls := t.peerURLs(ips)
	if slices.Equal(urls, t.lastPeers) {
		return // peer set unchanged — skip the ring rebuild
	}
	if err := t.universe.Set(urls...); err != nil {
		t.log.WarnContext(ctx, "session cache set peers failed; keeping current peer set",
			slog.Any("err", err), slog.Int("peers", len(urls)))
		return
	}
	t.lastPeers = urls
	t.log.DebugContext(ctx, "session cache peers refreshed", slog.Int("peers", len(urls)))
}

// peerURLs builds the sorted, deduplicated peer-URL set from the resolved IPs,
// always including selfURL. Sorting makes the result order-stable regardless of
// DNS answer ordering, so refreshOnce's unchanged-set comparison is reliable.
func (t *Tier) peerURLs(ips []string) []string {
	set := map[string]struct{}{t.selfURL: {}}
	for _, ip := range ips {
		set[peerURL(ip, t.port)] = struct{}{}
	}
	urls := make([]string, 0, len(set))
	for u := range set {
		urls = append(urls, u)
	}
	slices.Sort(urls)
	return urls
}

// Close shuts down the universe, closing peer fetchers.
func (t *Tier) Close() error { return t.universe.Shutdown() }

// peerURL builds the galaxycache peer URL (== peer ID) for host:port. net.JoinHostPort
// brackets IPv6 literals so both families produce a valid URL.
func peerURL(host, port string) string {
	return "http://" + net.JoinHostPort(host, port)
}
