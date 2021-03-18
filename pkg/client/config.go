package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Timeouts Timeouts `json:"timeouts,omitempty"`
}

// UnmarshalJSON caters for the unfortunate fact that time.Duration doesn't do JSON at all.
func (d *Timeouts) UnmarshalJSON(b []byte) error {
	var jsonFields map[string]interface{}
	if err := json.Unmarshal(b, &jsonFields); err != nil {
		return err
	}
	// Initialize by value copying the defaults
	*d = defaultConfig.Timeouts

	dv := reflect.ValueOf(d)
	dt := dv.Type()
	for i := dt.NumField() - 1; i >= 0; i-- {
		field := dt.Field(i)
		var name string
		jTag := field.Tag.Get("json")
		if jTag == "" {
			name = field.Name
		} else {
			name = strings.Split(jTag, ",")[0]
		}

		val := jsonFields[name]
		var timeout time.Duration
		switch val := val.(type) {
		case string:
			var err error
			timeout, err = time.ParseDuration(val)
			if err != nil {
				return fmt.Errorf("timeouts.%s: %q is not a valid duration", name, val)
			}
		case float64:
			timeout = time.Duration(val * float64(time.Second))
		default:
			return fmt.Errorf("timeouts.%s: %v is not a valid duration", name, val)
		}
		dv.Field(i).SetInt(int64(timeout))
	}
	return nil
}

type Timeouts struct {
	// AgentInstall is how long to wait for an agent to be installed (i.e. apply of service and deploy manifests)
	AgentInstall time.Duration `json:"agentInstall,omitempty"`
	// Apply is how long to wait for a k8s manifest to be applied
	Apply time.Duration `json:"apply,omitempty"`
	// ClusterConnect is the maximum time to wait for a connection to the cluster to be established
	ClusterConnect time.Duration `json:"clusterConnect,omitempty"`
	// Intercept is the time to wait for an intercept after the agents has been installed
	Intercept time.Duration `json:"intercept,omitempty"`
	// ProxyDial is how long to wait for the proxy to establish an outbound connection
	ProxyDial time.Duration `json:"proxyDial,omitempty"`
	// TrafficManagerConnect is how long to wait for the traffic-manager API to connect
	TrafficManagerConnect time.Duration `json:"trafficManagerConnect,omitempty"`
}

var defaultConfig = Config{Timeouts: Timeouts{
	ClusterConnect:        20 * time.Second,
	TrafficManagerConnect: 20 * time.Second,
	Apply:                 1 * time.Minute,
	Intercept:             5 * time.Second,
	AgentInstall:          120 * time.Second,
	ProxyDial:             5 * time.Second,
}}

var config *Config

var configOnce = sync.Once{}

// GetConfig returns the Telepresence configuration as stored in filelocation.AppUserConfigDir
// or filelocation.AppSystemConfigDirs
//
func GetConfig(c context.Context) *Config {
	configOnce.Do(func() {
		var err error
		config, err = loadConfig(c)
		if err != nil {
			config = &defaultConfig
		}
	})
	return config
}

func loadConfig(c context.Context) (*Config, error) {
	// TODO: Actually load the config
	return nil, os.ErrNotExist
}
