package cache

import (
	"context"
	"flag"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/thanos-io/thanos/pkg/discovery/dns"

	"github.com/cortexproject/cortex/pkg/util"
)

var (
	memcacheServersDiscovered = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "cortex",
		Name:      "memcache_client_servers",
		Help:      "The number of memcache servers discovered.",
	}, []string{"name"})
)

// MemcachedClient interface exists for mocking memcacheClient.
type MemcachedClient interface {
	GetMulti(keys []string) (map[string]*memcache.Item, error)
	Set(item *memcache.Item) error
}

type serverSelector interface {
	memcache.ServerSelector
	SetServers(servers ...string) error
}

// memcachedClient is a memcache client that gets its server list from SRV
// records, and periodically updates that ServerList.
type memcachedClient struct {
	*memcache.Client
	serverList serverSelector

	hostname string
	service  string

	addresses []string
	provider  *dns.Provider

	quit chan struct{}
	wait sync.WaitGroup

	numServers prometheus.Gauge
}

// MemcachedClientConfig defines how a MemcachedClient should be constructed.
type MemcachedClientConfig struct {
	Host           string        `yaml:"host,omitempty"`
	Service        string        `yaml:"service,omitempty"`
	Addresses      string        `yaml:"addresses"` // EXPERIMENTAL.
	Timeout        time.Duration `yaml:"timeout,omitempty"`
	MaxIdleConns   int           `yaml:"max_idle_conns,omitempty"`
	UpdateInterval time.Duration `yaml:"update_interval,omitempty"`
	ConsistentHash bool          `yaml:"consistent_hash,omitempty"`
}

// RegisterFlagsWithPrefix adds the flags required to config this to the given FlagSet
func (cfg *MemcachedClientConfig) RegisterFlagsWithPrefix(prefix, description string, f *flag.FlagSet) {
	f.StringVar(&cfg.Host, prefix+"memcached.hostname", "", description+"Hostname for memcached service to use when caching chunks. If empty, no memcached will be used.")
	f.StringVar(&cfg.Service, prefix+"memcached.service", "memcached", description+"SRV service used to discover memcache servers.")
	f.StringVar(&cfg.Addresses, prefix+"memcached.addresses", "", description+"EXPERIMENTAL: Comma separated addresses list in Thanos DNS Service Discovery format: https://thanos.io/service-discovery.md/#dns-service-discovery")
	f.IntVar(&cfg.MaxIdleConns, prefix+"memcached.max-idle-conns", 16, description+"Maximum number of idle connections in pool.")
	f.DurationVar(&cfg.Timeout, prefix+"memcached.timeout", 100*time.Millisecond, description+"Maximum time to wait before giving up on memcached requests.")
	f.DurationVar(&cfg.UpdateInterval, prefix+"memcached.update-interval", 1*time.Minute, description+"Period with which to poll DNS for memcache servers.")
	f.BoolVar(&cfg.ConsistentHash, prefix+"memcached.consistent-hash", false, description+"Use consistent hashing to distribute to memcache servers.")
}

// NewMemcachedClient creates a new MemcacheClient that gets its server list
// from SRV and updates the server list on a regular basis.
func NewMemcachedClient(cfg MemcachedClientConfig, name string) MemcachedClient {
	var selector serverSelector
	if cfg.ConsistentHash {
		selector = &MemcachedJumpHashSelector{}
	} else {
		selector = &memcache.ServerList{}
	}

	client := memcache.NewFromSelector(selector)
	client.Timeout = cfg.Timeout
	client.MaxIdleConns = cfg.MaxIdleConns

	newClient := &memcachedClient{
		Client:     client,
		serverList: selector,
		hostname:   cfg.Host,
		service:    cfg.Service,
		addresses:  strings.Split(cfg.Addresses, ","),
		provider:   dns.NewProvider(util.Logger, prometheus.DefaultRegisterer, dns.GolangResolverType),
		quit:       make(chan struct{}),

		numServers: memcacheServersDiscovered.WithLabelValues(name),
	}

	err := newClient.updateMemcacheServers()
	if err != nil {
		level.Error(util.Logger).Log("msg", "error setting memcache servers to host", "host", cfg.Host, "err", err)
	}

	newClient.wait.Add(1)
	go newClient.updateLoop(cfg.UpdateInterval)
	return newClient
}

// Stop the memcache client.
func (c *memcachedClient) Stop() {
	close(c.quit)
	c.wait.Wait()
}

func (c *memcachedClient) updateLoop(updateInterval time.Duration) {
	defer c.wait.Done()
	ticker := time.NewTicker(updateInterval)
	for {
		select {
		case <-ticker.C:
			err := c.updateMemcacheServers()
			if err != nil {
				level.Warn(util.Logger).Log("msg", "error updating memcache servers", "err", err)
			}
		case <-c.quit:
			ticker.Stop()
			return
		}
	}
}

// updateMemcacheServers sets a memcache server list from SRV records. SRV
// priority & weight are ignored.
func (c *memcachedClient) updateMemcacheServers() error {
	var servers []string

	if len(c.addresses) > 0 {
		c.provider.Resolve(context.Background(), c.addresses)
		servers = c.provider.Addresses()
	} else {
		_, addrs, err := net.LookupSRV(c.service, "tcp", c.hostname)
		if err != nil {
			return err
		}
		for _, srv := range addrs {
			servers = append(servers, fmt.Sprintf("%s:%d", srv.Target, srv.Port))
		}
	}

	// ServerList deterministically maps keys to _index_ of the server list.
	// Since DNS returns records in different order each time, we sort to
	// guarantee best possible match between nodes.
	sort.Strings(servers)
	c.numServers.Set(float64(len(servers)))
	return c.serverList.SetServers(servers...)
}
