/*
 * Copyright 2018 Comcast Cable Communications Management, LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package config provides Trickster configuration abilities, including
// parsing and printing configuration files, command line parameters, and
// environment variables, as well as default values and state.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tricksterproxy/trickster/pkg/cache/evictionmethods"
	cache "github.com/tricksterproxy/trickster/pkg/cache/options"
	"github.com/tricksterproxy/trickster/pkg/cache/types"
	d "github.com/tricksterproxy/trickster/pkg/config/defaults"
	reload "github.com/tricksterproxy/trickster/pkg/config/reload/options"
	"github.com/tricksterproxy/trickster/pkg/proxy/forwarding"
	"github.com/tricksterproxy/trickster/pkg/proxy/headers"
	origins "github.com/tricksterproxy/trickster/pkg/proxy/origins/options"
	rule "github.com/tricksterproxy/trickster/pkg/proxy/origins/rule/options"
	"github.com/tricksterproxy/trickster/pkg/proxy/paths/matching"
	rewriter "github.com/tricksterproxy/trickster/pkg/proxy/request/rewriter"
	rwopts "github.com/tricksterproxy/trickster/pkg/proxy/request/rewriter/options"
	to "github.com/tricksterproxy/trickster/pkg/proxy/tls/options"
	tracing "github.com/tricksterproxy/trickster/pkg/tracing/options"

	"github.com/BurntSushi/toml"
)

// Config is the main configuration object
type Config struct {
	// Main is the primary MainConfig section
	Main *MainConfig `toml:"main"`
	// Origins is a map of OriginConfigs
	Origins map[string]*origins.Options `toml:"origins"`
	// Caches is a map of CacheConfigs
	Caches map[string]*cache.Options `toml:"caches"`
	// ProxyServer is provides configurations about the Proxy Front End
	Frontend *FrontendConfig `toml:"frontend"`
	// Logging provides configurations that affect logging behavior
	Logging *LoggingConfig `toml:"logging"`
	// Metrics provides configurations for collecting Metrics about the application
	Metrics *MetricsConfig `toml:"metrics"`
	// TracingConfigs provides the distributed tracing configuration
	TracingConfigs map[string]*tracing.Options `toml:"tracing"`
	// NegativeCacheConfigs is a map of NegativeCacheConfigs
	NegativeCacheConfigs map[string]NegativeCacheConfig `toml:"negative_caches"`
	// Rules is a map of the Rules
	Rules map[string]*rule.Options `toml:"rules"`
	// RequestRewriters is a map of the Rewriters
	RequestRewriters map[string]*rwopts.Options `toml:"request_rewriters"`
	// ReloadConfig provides configurations for in-process config reloading
	ReloadConfig *reload.Options `toml:"reloading"`

	// Resources holds runtime resources uses by the Config
	Resources *Resources `toml:"-"`

	CompiledRewriters  map[string]rewriter.RewriteInstructions `toml:"-"`
	activeCaches       map[string]bool
	providedOriginURL  string
	providedOriginType string

	LoaderWarnings []string `toml:"-"`
}

// MainConfig is a collection of general configuration values.
type MainConfig struct {
	// InstanceID represents a unique ID for the current instance, when multiple instances on the same host
	InstanceID int `toml:"instance_id"`
	// ConfigHandlerPath provides the path to register the Config Handler for outputting the running configuration
	ConfigHandlerPath string `toml:"config_handler_path"`
	// PingHandlerPath provides the path to register the Ping Handler for checking that Trickster is running
	PingHandlerPath string `toml:"ping_handler_path"`
	// ReloadHandlerPath provides the path to register the Config Reload Handler
	ReloadHandlerPath string `toml:"reload_handler_path"`
	// HeatlHandlerPath provides the base Health Check Handler path
	HealthHandlerPath string `toml:"health_handler_path"`
	// PprofServer provides the name of the http listener that will host the pprof debugging routes
	// Options are: "metrics", "reload", "both", or "off"; default is both
	PprofServer string `toml:"pprof_server"`
	// ServerName represents the server name that is conveyed in Via headers to upstream origins
	// defaults to os.Hostname
	ServerName string `toml:"server_name"`

	// ReloaderLock is used to lock the config for reloading
	ReloaderLock sync.Mutex `toml:"-"`

	configFilePath      string
	configLastModified  time.Time
	configRateLimitTime time.Time
	stalenessCheckLock  sync.Mutex
}

// FrontendConfig is a collection of configurations for the main http frontend for the application
type FrontendConfig struct {
	// ListenAddress is IP address for the main http listener for the application
	ListenAddress string `toml:"listen_address"`
	// ListenPort is TCP Port for the main http listener for the application
	ListenPort int `toml:"listen_port"`
	// TLSListenAddress is IP address for the tls  http listener for the application
	TLSListenAddress string `toml:"tls_listen_address"`
	// TLSListenPort is the TCP Port for the tls http listener for the application
	TLSListenPort int `toml:"tls_listen_port"`
	// ConnectionsLimit indicates how many concurrent front end connections trickster will handle at any time
	ConnectionsLimit int `toml:"connections_limit"`

	// ServeTLS indicates whether to listen and serve on the TLS port, meaning
	// at least one origin configuration has a valid certificate and key file configured.
	ServeTLS bool `toml:"-"`
}

// LoggingConfig is a collection of Logging configurations
type LoggingConfig struct {
	// LogFile provides the filepath to the instances's logfile. Set as empty string to Log to Console
	LogFile string `toml:"log_file"`
	// LogLevel provides the most granular level (e.g., DEBUG, INFO, ERROR) to log
	LogLevel string `toml:"log_level"`
}

// MetricsConfig is a collection of Metrics Collection configurations
type MetricsConfig struct {
	// ListenAddress is IP address from which the Application Metrics are available for pulling at /metrics
	ListenAddress string `toml:"listen_address"`
	// ListenPort is TCP Port from which the Application Metrics are available for pulling at /metrics
	ListenPort int `toml:"listen_port"`
}

// Resources is a collection of values used by configs at runtime that are not part of the config itself
type Resources struct {
	QuitChan chan bool `toml:"-"`
	metadata *toml.MetaData
}

// NegativeCacheConfig is a collection of response codes and their TTLs
type NegativeCacheConfig map[string]int

// Clone returns an exact copy of a NegativeCacheConfig
func (nc NegativeCacheConfig) Clone() NegativeCacheConfig {
	nc2 := make(NegativeCacheConfig)
	for k, v := range nc {
		nc2[k] = v
	}
	return nc2
}

// NewConfig returns a Config initialized with default values.
func NewConfig() *Config {
	hn, _ := os.Hostname()
	return &Config{
		Caches: map[string]*cache.Options{
			"default": cache.NewOptions(),
		},
		Logging: &LoggingConfig{
			LogFile:  d.DefaultLogFile,
			LogLevel: d.DefaultLogLevel,
		},
		Main: &MainConfig{
			ConfigHandlerPath: d.DefaultConfigHandlerPath,
			PingHandlerPath:   d.DefaultPingHandlerPath,
			ReloadHandlerPath: d.DefaultReloadHandlerPath,
			HealthHandlerPath: d.DefaultHealthHandlerPath,
			PprofServer:       d.DefaultPprofServerName,
			ServerName:        hn,
		},
		Metrics: &MetricsConfig{
			ListenPort: d.DefaultMetricsListenPort,
		},
		Origins: map[string]*origins.Options{
			"default": origins.NewOptions(),
		},
		Frontend: &FrontendConfig{
			ListenPort:       d.DefaultProxyListenPort,
			ListenAddress:    d.DefaultProxyListenAddress,
			TLSListenPort:    d.DefaultTLSProxyListenPort,
			TLSListenAddress: d.DefaultTLSProxyListenAddress,
		},
		NegativeCacheConfigs: map[string]NegativeCacheConfig{
			"default": NewNegativeCacheConfig(),
		},
		TracingConfigs: map[string]*tracing.Options{
			"default": tracing.NewOptions(),
		},
		ReloadConfig:   reload.NewOptions(),
		LoaderWarnings: make([]string, 0),
		Resources: &Resources{
			QuitChan: make(chan bool, 1),
		},
	}
}

// NewNegativeCacheConfig returns an empty NegativeCacheConfig
func NewNegativeCacheConfig() NegativeCacheConfig {
	return NegativeCacheConfig{}
}

// loadFile loads application configuration from a TOML-formatted file.
func (c *Config) loadFile(flags *Flags) error {
	b, err := ioutil.ReadFile(flags.ConfigPath)
	if err != nil {
		c.setDefaults(&toml.MetaData{})
		return err
	}
	return c.loadTOMLConfig(string(b), flags)
}

// loadTOMLConfig loads application configuration from a TOML-formatted byte slice.
func (c *Config) loadTOMLConfig(tml string, flags *Flags) error {
	md, err := toml.Decode(tml, c)
	if err != nil {
		c.setDefaults(&toml.MetaData{})
		return err
	}
	err = c.setDefaults(&md)
	if err == nil {
		c.Main.configFilePath = flags.ConfigPath
		c.Main.configLastModified = c.CheckFileLastModified()
	}
	return err
}

// CheckFileLastModified returns the last modified date of the running config file, if present
func (c *Config) CheckFileLastModified() time.Time {
	if c.Main == nil || c.Main.configFilePath == "" {
		return time.Time{}
	}
	file, err := os.Stat(c.Main.configFilePath)
	if err != nil {
		return time.Time{}
	}
	return file.ModTime()
}

func (c *Config) setDefaults(metadata *toml.MetaData) error {

	c.Resources.metadata = metadata

	var err error

	if err = c.processPprofConfig(); err != nil {
		return err
	}

	if c.RequestRewriters != nil {
		if c.CompiledRewriters, err = rewriter.ProcessConfigs(c.RequestRewriters); err != nil {
			return err
		}
	}

	if err = c.processOriginConfigs(metadata); err != nil {
		return err
	}

	tracing.ProcessTracingOptions(c.TracingConfigs, metadata)

	if err = c.processCachingConfigs(metadata); err != nil {
		return err
	}

	if err = c.validateConfigMappings(); err != nil {
		return err
	}

	if err = c.validateTLSConfigs(); err != nil {
		return err
	}

	return nil
}

// ErrInvalidPprofServerName returns an error for invalid pprof server name
var ErrInvalidPprofServerName = errors.New("invalid pprof server name")

func (c *Config) processPprofConfig() error {
	switch c.Main.PprofServer {
	case "metrics", "reload", "off", "both":
		return nil
	case "":
		c.Main.PprofServer = d.DefaultPprofServerName
		return nil
	}
	return ErrInvalidPprofServerName
}

func (c *Config) validateTLSConfigs() error {
	for _, oc := range c.Origins {
		if oc.TLS != nil {
			b, err := oc.TLS.Validate()
			if err != nil {
				return err
			}
			if b {
				c.Frontend.ServeTLS = true
			}
		}
	}
	return nil
}

var pathMembers = []string{"path", "match_type", "handler", "methods", "cache_key_params",
	"cache_key_headers", "default_ttl_secs", "request_headers", "response_headers",
	"response_headers", "response_code", "response_body", "no_metrics", "collapsed_forwarding",
	"req_rewriter_name",
}

func (c *Config) validateConfigMappings() error {
	for k, oc := range c.Origins {

		if err := origins.ValidateOriginName(k); err != nil {
			return err
		}

		if oc.OriginType == "rule" {
			// Rule Type Validations
			r, ok := c.Rules[oc.RuleName]
			if !ok {
				return fmt.Errorf("invalid rule name [%s] provided in origin config [%s]", oc.RuleName, k)
			}
			r.Name = oc.RuleName
			oc.RuleOptions = r
		} else // non-Rule Type Validations
		if _, ok := c.Caches[oc.CacheName]; !ok {
			return fmt.Errorf("invalid cache name [%s] provided in origin config [%s]", oc.CacheName, k)
		}

	}
	return nil
}

func (c *Config) processOriginConfigs(metadata *toml.MetaData) error {

	if metadata == nil {
		return errors.New("invalid config metadata")
	}

	c.activeCaches = make(map[string]bool)

	for k, v := range c.Origins {

		oc := origins.NewOptions()
		oc.Name = k

		if metadata.IsDefined("origins", k, "req_rewriter_name") && v.ReqRewriterName != "" {
			oc.ReqRewriterName = v.ReqRewriterName
			ri, ok := c.CompiledRewriters[oc.ReqRewriterName]
			if !ok {
				return fmt.Errorf("invalid rewriter name %s in origin config %s",
					oc.ReqRewriterName, k)
			}
			oc.ReqRewriter = ri
		}

		if metadata.IsDefined("origins", k, "origin_type") {
			oc.OriginType = v.OriginType
		}

		if metadata.IsDefined("origins", k, "rule_name") {
			oc.RuleName = v.RuleName
		}

		if metadata.IsDefined("origins", k, "path_routing_disabled") {
			oc.PathRoutingDisabled = v.PathRoutingDisabled
		}

		if metadata.IsDefined("origins", k, "hosts") && v != nil {
			oc.Hosts = make([]string, len(v.Hosts))
			copy(oc.Hosts, v.Hosts)
		}

		if metadata.IsDefined("origins", k, "is_default") {
			oc.IsDefault = v.IsDefault
		}
		// If there is only one origin and is_default is not explicitly false, make it true
		if len(c.Origins) == 1 && (!metadata.IsDefined("origins", k, "is_default")) {
			oc.IsDefault = true
		}

		if metadata.IsDefined("origins", k, "forwarded_headers") {
			oc.ForwardedHeaders = v.ForwardedHeaders
		}

		if metadata.IsDefined("origins", k, "require_tls") {
			oc.RequireTLS = v.RequireTLS
		}

		if metadata.IsDefined("origins", k, "cache_name") {
			oc.CacheName = v.CacheName
		}
		c.activeCaches[oc.CacheName] = true

		if metadata.IsDefined("origins", k, "cache_key_prefix") {
			oc.CacheKeyPrefix = v.CacheKeyPrefix
		}

		if metadata.IsDefined("origins", k, "origin_url") {
			oc.OriginURL = v.OriginURL
		}

		if metadata.IsDefined("origins", k, "compressable_types") {
			oc.CompressableTypeList = v.CompressableTypeList
		}

		if metadata.IsDefined("origins", k, "timeout_secs") {
			oc.TimeoutSecs = v.TimeoutSecs
		}

		if metadata.IsDefined("origins", k, "max_idle_conns") {
			oc.MaxIdleConns = v.MaxIdleConns
		}

		if metadata.IsDefined("origins", k, "keep_alive_timeout_secs") {
			oc.KeepAliveTimeoutSecs = v.KeepAliveTimeoutSecs
		}

		if metadata.IsDefined("origins", k, "timeseries_retention_factor") {
			oc.TimeseriesRetentionFactor = v.TimeseriesRetentionFactor
		}

		if metadata.IsDefined("origins", k, "timeseries_eviction_method") {
			oc.TimeseriesEvictionMethodName = strings.ToLower(v.TimeseriesEvictionMethodName)
			if p, ok := evictionmethods.Names[oc.TimeseriesEvictionMethodName]; ok {
				oc.TimeseriesEvictionMethod = p
			}
		}

		if metadata.IsDefined("origins", k, "timeseries_ttl_secs") {
			oc.TimeseriesTTLSecs = v.TimeseriesTTLSecs
		}

		if metadata.IsDefined("origins", k, "max_ttl_secs") {
			oc.MaxTTLSecs = v.MaxTTLSecs
		}

		if metadata.IsDefined("origins", k, "fastforward_ttl_secs") {
			oc.FastForwardTTLSecs = v.FastForwardTTLSecs
		}

		if metadata.IsDefined("origins", k, "fast_forward_disable") {
			oc.FastForwardDisable = v.FastForwardDisable
		}

		if metadata.IsDefined("origins", k, "backfill_tolerance_secs") {
			oc.BackfillToleranceSecs = v.BackfillToleranceSecs
		}

		if metadata.IsDefined("origins", k, "paths") {
			var j = 0
			for l, p := range v.Paths {
				if metadata.IsDefined("origins", k, "paths", l, "req_rewriter_name") &&
					p.ReqRewriterName != "" {
					ri, ok := c.CompiledRewriters[p.ReqRewriterName]
					if !ok {
						return fmt.Errorf("invalid rewriter name %s in path %s of origin config %s",
							p.ReqRewriterName, l, k)
					}
					p.ReqRewriter = ri
				}
				if len(p.Methods) == 0 {
					p.Methods = []string{http.MethodGet, http.MethodHead}
				}
				p.Custom = make([]string, 0)
				for _, pm := range pathMembers {
					if metadata.IsDefined("origins", k, "paths", l, pm) {
						p.Custom = append(p.Custom, pm)
					}
				}
				if metadata.IsDefined("origins", k, "paths", l, "response_body") {
					p.ResponseBodyBytes = []byte(p.ResponseBody)
					p.HasCustomResponseBody = true
				}
				if metadata.IsDefined("origins", k, "paths", l, "collapsed_forwarding") {
					if _, ok := forwarding.CollapsedForwardingTypeNames[p.CollapsedForwardingName]; !ok {
						return fmt.Errorf("invalid collapsed_forwarding name: %s", p.CollapsedForwardingName)
					}
					p.CollapsedForwardingType =
						forwarding.GetCollapsedForwardingType(p.CollapsedForwardingName)
				} else {
					p.CollapsedForwardingType = forwarding.CFTypeBasic
				}
				if mt, ok := matching.Names[strings.ToLower(p.MatchTypeName)]; ok {
					p.MatchType = mt
					p.MatchTypeName = p.MatchType.String()
				} else {
					p.MatchType = matching.PathMatchTypeExact
					p.MatchTypeName = p.MatchType.String()
				}
				oc.Paths[p.Path+"-"+strings.Join(p.Methods, "-")] = p
				j++
			}
		}

		if metadata.IsDefined("origins", k, "negative_cache_name") {
			oc.NegativeCacheName = v.NegativeCacheName
		}

		if metadata.IsDefined("origins", k, "tracing_name") {
			oc.TracingConfigName = v.TracingConfigName
		}

		if metadata.IsDefined("origins", k, "health_check_upstream_path") {
			oc.HealthCheckUpstreamPath = v.HealthCheckUpstreamPath
		}

		if metadata.IsDefined("origins", k, "health_check_verb") {
			oc.HealthCheckVerb = v.HealthCheckVerb
		}

		if metadata.IsDefined("origins", k, "health_check_query") {
			oc.HealthCheckQuery = v.HealthCheckQuery
		}

		if metadata.IsDefined("origins", k, "health_check_headers") {
			oc.HealthCheckHeaders = v.HealthCheckHeaders
		}

		if metadata.IsDefined("origins", k, "max_object_size_bytes") {
			oc.MaxObjectSizeBytes = v.MaxObjectSizeBytes
		}

		if metadata.IsDefined("origins", k, "revalidation_factor") {
			oc.RevalidationFactor = v.RevalidationFactor
		}

		if metadata.IsDefined("origins", k, "multipart_ranges_disabled") {
			oc.MultipartRangesDisabled = v.MultipartRangesDisabled
		}

		if metadata.IsDefined("origins", k, "dearticulate_upstream_ranges") {
			oc.DearticulateUpstreamRanges = v.DearticulateUpstreamRanges
		}

		if metadata.IsDefined("origins", k, "tls") {
			oc.TLS = &to.Options{
				InsecureSkipVerify:        v.TLS.InsecureSkipVerify,
				CertificateAuthorityPaths: v.TLS.CertificateAuthorityPaths,
				PrivateKeyPath:            v.TLS.PrivateKeyPath,
				FullChainCertPath:         v.TLS.FullChainCertPath,
				ClientCertPath:            v.TLS.ClientCertPath,
				ClientKeyPath:             v.TLS.ClientKeyPath,
			}
		}

		c.Origins[k] = oc
	}
	return nil
}

func (c *Config) processCachingConfigs(metadata *toml.MetaData) error {

	// setCachingDefaults assumes that processOriginConfigs was just ran

	for k, v := range c.Caches {

		if _, ok := c.activeCaches[k]; !ok {
			// a configured cache was not used by any origin. don't even instantiate it
			delete(c.Caches, k)
			continue
		}

		cc := cache.NewOptions()
		cc.Name = k

		if metadata.IsDefined("caches", k, "cache_type") {
			cc.CacheType = strings.ToLower(v.CacheType)
			if n, ok := types.Names[cc.CacheType]; ok {
				cc.CacheTypeID = n
			}
		}

		if metadata.IsDefined("caches", k, "index", "reap_interval_secs") {
			cc.Index.ReapIntervalSecs = v.Index.ReapIntervalSecs
		}

		if metadata.IsDefined("caches", k, "index", "flush_interval_secs") {
			cc.Index.FlushIntervalSecs = v.Index.FlushIntervalSecs
		}

		if metadata.IsDefined("caches", k, "index", "max_size_bytes") {
			cc.Index.MaxSizeBytes = v.Index.MaxSizeBytes
		}

		if metadata.IsDefined("caches", k, "index", "max_size_backoff_bytes") {
			cc.Index.MaxSizeBackoffBytes = v.Index.MaxSizeBackoffBytes
		}

		if cc.Index.MaxSizeBytes > 0 && cc.Index.MaxSizeBackoffBytes > cc.Index.MaxSizeBytes {
			return errors.New("MaxSizeBackoffBytes can't be larger than MaxSizeBytes")
		}

		if metadata.IsDefined("caches", k, "index", "max_size_objects") {
			cc.Index.MaxSizeObjects = v.Index.MaxSizeObjects
		}

		if metadata.IsDefined("caches", k, "index", "max_size_backoff_objects") {
			cc.Index.MaxSizeBackoffObjects = v.Index.MaxSizeBackoffObjects
		}

		if cc.Index.MaxSizeObjects > 0 && cc.Index.MaxSizeBackoffObjects > cc.Index.MaxSizeObjects {
			return errors.New("MaxSizeBackoffObjects can't be larger than MaxSizeObjects")
		}

		if cc.CacheTypeID == types.CacheTypeRedis {

			var hasEndpoint, hasEndpoints bool

			ct := strings.ToLower(v.Redis.ClientType)
			if metadata.IsDefined("caches", k, "redis", "client_type") {
				cc.Redis.ClientType = ct
			}

			if metadata.IsDefined("caches", k, "redis", "protocol") {
				cc.Redis.Protocol = v.Redis.Protocol
			}

			if metadata.IsDefined("caches", k, "redis", "endpoint") {
				cc.Redis.Endpoint = v.Redis.Endpoint
				hasEndpoint = true
			}

			if metadata.IsDefined("caches", k, "redis", "endpoints") {
				cc.Redis.Endpoints = v.Redis.Endpoints
				hasEndpoints = true
			}

			if cc.Redis.ClientType == "standard" {
				if hasEndpoints && !hasEndpoint {
					c.LoaderWarnings = append(c.LoaderWarnings,
						"'standard' redis type configured, but 'endpoints' value is provided instead of 'endpoint'")
				}
			} else {
				if hasEndpoint && !hasEndpoints {
					c.LoaderWarnings = append(c.LoaderWarnings, fmt.Sprintf(
						"'%s' redis type configured, but 'endpoint' value is provided instead of 'endpoints'",
						cc.Redis.ClientType))
				}
			}

			if metadata.IsDefined("caches", k, "redis", "sentinel_master") {
				cc.Redis.SentinelMaster = v.Redis.SentinelMaster
			}

			if metadata.IsDefined("caches", k, "redis", "password") {
				cc.Redis.Password = v.Redis.Password
			}

			if metadata.IsDefined("caches", k, "redis", "db") {
				cc.Redis.DB = v.Redis.DB
			}

			if metadata.IsDefined("caches", k, "redis", "max_retries") {
				cc.Redis.MaxRetries = v.Redis.MaxRetries
			}

			if metadata.IsDefined("caches", k, "redis", "min_retry_backoff_ms") {
				cc.Redis.MinRetryBackoffMS = v.Redis.MinRetryBackoffMS
			}

			if metadata.IsDefined("caches", k, "redis", "max_retry_backoff_ms") {
				cc.Redis.MaxRetryBackoffMS = v.Redis.MaxRetryBackoffMS
			}

			if metadata.IsDefined("caches", k, "redis", "dial_timeout_ms") {
				cc.Redis.DialTimeoutMS = v.Redis.DialTimeoutMS
			}

			if metadata.IsDefined("caches", k, "redis", "read_timeout_ms") {
				cc.Redis.ReadTimeoutMS = v.Redis.ReadTimeoutMS
			}

			if metadata.IsDefined("caches", k, "redis", "write_timeout_ms") {
				cc.Redis.WriteTimeoutMS = v.Redis.WriteTimeoutMS
			}

			if metadata.IsDefined("caches", k, "redis", "pool_size") {
				cc.Redis.PoolSize = v.Redis.PoolSize
			}

			if metadata.IsDefined("caches", k, "redis", "min_idle_conns") {
				cc.Redis.MinIdleConns = v.Redis.MinIdleConns
			}

			if metadata.IsDefined("caches", k, "redis", "max_conn_age_ms") {
				cc.Redis.MaxConnAgeMS = v.Redis.MaxConnAgeMS
			}

			if metadata.IsDefined("caches", k, "redis", "pool_timeout_ms") {
				cc.Redis.PoolTimeoutMS = v.Redis.PoolTimeoutMS
			}

			if metadata.IsDefined("caches", k, "redis", "idle_timeout_ms") {
				cc.Redis.IdleTimeoutMS = v.Redis.IdleTimeoutMS
			}

			if metadata.IsDefined("caches", k, "redis", "idle_check_frequency_ms") {
				cc.Redis.IdleCheckFrequencyMS = v.Redis.IdleCheckFrequencyMS
			}
		}

		if metadata.IsDefined("caches", k, "filesystem", "cache_path") {
			cc.Filesystem.CachePath = v.Filesystem.CachePath
		}

		if metadata.IsDefined("caches", k, "bbolt", "filename") {
			cc.BBolt.Filename = v.BBolt.Filename
		}

		if metadata.IsDefined("caches", k, "bbolt", "bucket") {
			cc.BBolt.Bucket = v.BBolt.Bucket
		}

		if metadata.IsDefined("caches", k, "badger", "directory") {
			cc.Badger.Directory = v.Badger.Directory
		}

		if metadata.IsDefined("caches", k, "badger", "value_directory") {
			cc.Badger.ValueDirectory = v.Badger.ValueDirectory
		}

		c.Caches[k] = cc
	}
	return nil
}

// Clone returns an exact copy of the subject *Config
func (c *Config) Clone() *Config {

	nc := NewConfig()
	delete(nc.Caches, "default")
	delete(nc.Origins, "default")

	nc.Main.ConfigHandlerPath = c.Main.ConfigHandlerPath
	nc.Main.InstanceID = c.Main.InstanceID
	nc.Main.PingHandlerPath = c.Main.PingHandlerPath
	nc.Main.ReloadHandlerPath = c.Main.ReloadHandlerPath
	nc.Main.HealthHandlerPath = c.Main.HealthHandlerPath
	nc.Main.PprofServer = c.Main.PprofServer
	nc.Main.ServerName = c.Main.ServerName

	nc.Main.configFilePath = c.Main.configFilePath
	nc.Main.configLastModified = c.Main.configLastModified
	nc.Main.configRateLimitTime = c.Main.configRateLimitTime

	nc.Logging.LogFile = c.Logging.LogFile
	nc.Logging.LogLevel = c.Logging.LogLevel

	nc.Metrics.ListenAddress = c.Metrics.ListenAddress
	nc.Metrics.ListenPort = c.Metrics.ListenPort

	nc.Frontend.ListenAddress = c.Frontend.ListenAddress
	nc.Frontend.ListenPort = c.Frontend.ListenPort
	nc.Frontend.TLSListenAddress = c.Frontend.TLSListenAddress
	nc.Frontend.TLSListenPort = c.Frontend.TLSListenPort
	nc.Frontend.ConnectionsLimit = c.Frontend.ConnectionsLimit
	nc.Frontend.ServeTLS = c.Frontend.ServeTLS

	nc.Resources = &Resources{
		QuitChan: make(chan bool, 1),
	}

	for k, v := range c.Origins {
		nc.Origins[k] = v.Clone()
	}

	for k, v := range c.Caches {
		nc.Caches[k] = v.Clone()
	}

	for k, v := range c.NegativeCacheConfigs {
		nc.NegativeCacheConfigs[k] = v.Clone()
	}

	for k, v := range c.TracingConfigs {
		nc.TracingConfigs[k] = v.Clone()
	}

	if c.Rules != nil && len(c.Rules) > 0 {
		nc.Rules = make(map[string]*rule.Options)
		for k, v := range c.Rules {
			nc.Rules[k] = v.Clone()
		}
	}

	if c.RequestRewriters != nil && len(c.RequestRewriters) > 0 {
		nc.RequestRewriters = make(map[string]*rwopts.Options)
		for k, v := range c.RequestRewriters {
			nc.RequestRewriters[k] = v.Clone()
		}
	}

	return nc
}

// IsStale returns true if the running config is stale versus the
func (c *Config) IsStale() bool {

	c.Main.stalenessCheckLock.Lock()
	defer c.Main.stalenessCheckLock.Unlock()

	if c.Main == nil || c.Main.configFilePath == "" ||
		time.Now().Before(c.Main.configRateLimitTime) {
		return false
	}

	if c.ReloadConfig == nil {
		c.ReloadConfig = reload.NewOptions()
	}

	c.Main.configRateLimitTime =
		time.Now().Add(time.Second * time.Duration(c.ReloadConfig.RateLimitSecs))
	t := c.CheckFileLastModified()
	if t.IsZero() {
		return false
	}
	return t != c.Main.configLastModified
}

func (c *Config) String() string {
	cp := c.Clone()

	// the toml library will panic if the Handler is assigned,
	// even though this field is annotated as skip ("-") in the prototype
	// so we'll iterate the paths and set to nil the Handler (in our local copy only)
	if cp.Origins != nil {
		for _, v := range cp.Origins {
			if v != nil {
				for _, w := range v.Paths {
					w.Handler = nil
					w.KeyHasher = nil
				}
			}
			// also strip out potentially sensitive headers
			hideAuthorizationCredentials(v.HealthCheckHeaders)

			if v.Paths != nil {
				for _, p := range v.Paths {
					hideAuthorizationCredentials(p.RequestHeaders)
					hideAuthorizationCredentials(p.ResponseHeaders)
				}
			}
		}
	}

	// strip Redis password
	for k, v := range cp.Caches {
		if v != nil && cp.Caches[k].Redis.Password != "" {
			cp.Caches[k].Redis.Password = "*****"
		}
	}

	var buf bytes.Buffer
	e := toml.NewEncoder(&buf)
	e.Encode(cp)
	return buf.String()
}

// ConfigFilePath returns the file path from which this configuration is based
func (c *Config) ConfigFilePath() string {
	if c.Main != nil {
		return c.Main.configFilePath
	}
	return ""
}

// Equal returns true if the FrontendConfigs are identical in value.
func (fc *FrontendConfig) Equal(fc2 *FrontendConfig) bool {
	return *fc == *fc2
}

var sensitiveCredentials = map[string]bool{headers.NameAuthorization: true}

func hideAuthorizationCredentials(headers map[string]string) {
	// strip Authorization Headers
	for k := range headers {
		if _, ok := sensitiveCredentials[k]; ok {
			headers[k] = "*****"
		}
	}
}
